package httpapi

import (
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
	"wspoc-go/internal/security"
	"wspoc-go/internal/session"
)

// connIDLen es la longitud del identificador corto de conexión que sale en los logs.
const connIDLen = 8

// diagnosticResponse es lo que se responde a un GET normal (sin upgrade).
//
// Reason y Code son `any` para poder emitir null cuando la petición se acepta:
// el frontend distingue "sin motivo" de "motivo vacío".
type diagnosticResponse struct {
	Allowed bool `json:"allowed"`
	Reason  any  `json:"reason"`
	Code    any  `json:"code"`
}

// handleWS atiende tanto el handshake real como el diagnóstico del POC.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	verdict := s.evaluate(r)

	// Petición GET normal (sin upgrade): endpoint de diagnóstico del POC.
	if !websocket.IsWebSocketUpgrade(r) {
		s.writeDiagnostic(w, r, verdict)
		return
	}

	if !verdict.Allowed {
		// Rechazo ANTES del upgrade: nunca se abre el WebSocket.
		logging.Reject(verdict.Rejection, r.RemoteAddr, r.Header.Get("Origin"), "handshake")
		http.Error(w, verdict.Rejection.Reason, verdict.Rejection.HTTPStatus)
		return
	}

	s.acceptConnection(w, r, verdict)
}

// evaluate aplica los controles pre-upgrade en orden: origen -> token -> capacidad.
//
// El orden es contrato observable: con un origen prohibido y un token inválido a
// la vez, lo que se reporta es el origen. Cambiarlo cambiaría el error que ve la UI.
func (s *Server) evaluate(r *http.Request) security.Verdict {
	if verdict := s.guard.CheckOrigin(r.Header.Get("Origin")); !verdict.Allowed {
		return verdict
	}
	verdict := s.guard.CheckToken(r.URL.Query().Get("token"))
	if !verdict.Allowed {
		return verdict
	}
	if s.registry.AtCapacity() {
		return security.Verdict{Allowed: false, Rejection: protocol.ServerAtCapacity}
	}
	return verdict
}

// writeDiagnostic responde el veredicto en JSON, sin abrir ningún socket.
func (s *Server) writeDiagnostic(w http.ResponseWriter, r *http.Request, verdict security.Verdict) {
	s.setDiagnosticCORS(w, r)

	if verdict.Allowed {
		writeJSON(w, http.StatusOK, diagnosticResponse{Allowed: true})
		return
	}
	writeJSON(w, verdict.Rejection.HTTPStatus, diagnosticResponse{
		Allowed: false,
		Reason:  verdict.Rejection.Reason,
		Code:    verdict.Rejection.Code,
	})
}

// acceptConnection reserva la plaza, hace el upgrade y atiende la conexión hasta
// que muere.
func (s *Server) acceptConnection(w http.ResponseWriter, r *http.Request, verdict security.Verdict) {
	remote := r.RemoteAddr
	origin := r.Header.Get("Origin")

	// Reserva atómica de la plaza antes del upgrade: sin esto, dos handshakes
	// simultáneos podrían pasar la comprobación y superar el máximo.
	active, ok := s.registry.TryAdd()
	if !ok {
		logging.Reject(protocol.ServerAtCapacity, remote, origin,
			"max="+strconv.Itoa(int(s.registry.Max())))
		http.Error(w, protocol.ServerAtCapacity.Reason, protocol.ServerAtCapacity.HTTPStatus)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.registry.Release()
		logging.RejectRaw("UPGRADE_FAILED", 1002, remote, origin, err.Error())
		return
	}

	client := session.NewClient(conn, verdict.Session, uuid.NewString()[:connIDLen])
	s.registry.Bind(client)
	logging.Accept(client.ConnID(), remote, verdict.Subject, verdict.Session, active)

	// Aislamiento de fallos: un pánico en una conexión no debe tumbar el proceso (§7).
	defer func() {
		if rec := recover(); rec != nil {
			logging.RejectRaw("CONNECTION_FAULT", 1011, remote, origin, logging.Detail(rec))
		}
		_ = conn.Close()
		logging.Close(client.ConnID(), s.registry.Remove(client))
	}()

	s.newConnection(client, remote).run()
}
