package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
	"wspoc-go/internal/security"
)

// errTooLarge señala un cuerpo por encima del límite.
var errTooLarge = errors.New("cuerpo demasiado grande")

// pushResponse confirma una entrega.
//
// `Sesion` va con la grafía del .NET, igual que en la peticion: quien llama
// deserializa la respuesta con el mismo modelo con el que serializo el sobre.
type pushResponse struct {
	Delivered int    `json:"delivered"`
	Session   string `json:"Sesion"`
	Instance  string `json:"instance"`
}

// handlePush recibe del .NET 4.8 un {sesion, payload} y lo entrega por WebSocket
// a esa sesión. Es el reemplazo del canal push del .NET Framework 4.5.
//
// La cadena de controles se lee de arriba abajo en el mismo orden en que se
// aplica: CORS, método, token, caudal, tamaño, forma y por último entrega.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	if s.setAPICORS(w, r) {
		return // preflight resuelto
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !s.authorize(w, r, "push") {
		return
	}
	if !s.allowPushRate(w, r) {
		return
	}

	raw, err := readLimited(r, s.cfg.MaxMessageBytes)
	if err != nil {
		writeRejection(w, protocol.MessageTooLarge)
		return
	}

	env, rejection := s.validator.Envelope(raw)
	if rejection != nil {
		logging.Reject(*rejection, r.RemoteAddr, "", "push")
		writeRejection(w, *rejection)
		return
	}

	s.deliverPush(w, r, env)
}

// deliverPush entrega el sobre a las conexiones de la sesión.
func (s *Server) deliverPush(w http.ResponseWriter, r *http.Request, env protocol.Envelope) {
	frame := s.framer.Push(env.Payload, env.Session, env.Identificador)

	delivered := s.deliverer.Deliver(env.Session, frame)
	if delivered == 0 {
		logging.Reject(protocol.SessionNotFound, r.RemoteAddr, "", "sesion="+env.Session)
		// Respuesta con forma propia: quien llama necesita saber QUÉ sesión falló
		// para reintentarla, no solo que algo falló.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"reason": protocol.SessionNotFound.Reason,
			"Sesion": env.Session,
		})
		return
	}

	logging.Push(env.Session, delivered, len(env.Payload))
	writeJSON(w, http.StatusAccepted, pushResponse{
		Delivered: delivered,
		Session:   env.Session,
		Instance:  s.cfg.InstanceID,
	})
}

// authorize aplica el control de token del puente, leyendo la cabecera: el .NET
// no es un navegador, así que sí puede enviar cabeceras.
//
// Es CheckBridgeToken y no CheckToken: este endpoint elige a qué sesión entrega,
// así que un token del intercambio —que se le da a cualquiera que traiga una
// SESION— no puede valer aquí. Lo mismo rige para el outbox, que lee lo que los
// clientes enviaron.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request, detail string) bool {
	verdict := s.guard.CheckBridgeToken(security.BearerToken(r.Header.Get("Authorization")))
	if verdict.Allowed {
		return true
	}
	logging.Reject(verdict.Rejection, r.RemoteAddr, "", detail)
	writeRejection(w, verdict.Rejection)
	return false
}

// allowPushRate aplica el límite de caudal del endpoint.
//
// A diferencia del limitador por conexión, este es GLOBAL y lo comparten todos
// los llamantes: un .NET que dispare ráfagas puede agotar el cupo del resto.
func (s *Server) allowPushRate(w http.ResponseWriter, r *http.Request) bool {
	if s.pushLimiter.Allow() {
		return true
	}
	logging.Reject(protocol.RateLimitExceeded, r.RemoteAddr, "",
		fmt.Sprintf("push limit=%d/s", s.cfg.PushRateLimitPerSec))
	writeRejection(w, protocol.RateLimitExceeded)
	return false
}

// requireMethod rechaza cualquier verbo distinto del esperado.
//
// Hace falta porque net/http registra el handler por RUTA, no por método: sin
// esta comprobación, el mismo handler atendería PUT, DELETE o HEAD.
func requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"reason": "METHOD_NOT_ALLOWED"})
	return false
}

// readLimited lee el cuerpo cortando un byte por encima del máximo, igual que
// hace el canal WebSocket con los frames grandes.
//
// Cortar en streaming y no medir después es lo que hace que el límite proteja:
// leer el cuerpo entero para luego rechazarlo permitiría tumbar el proceso
// mandando cuerpos gigantes.
func readLimited(r *http.Request, max int64) ([]byte, error) {
	defer r.Body.Close()

	buf := new(bytes.Buffer)
	n, err := buf.ReadFrom(http.MaxBytesReader(nil, r.Body, max+1))
	if err != nil {
		return nil, err
	}
	if n > max {
		return nil, errTooLarge
	}
	return buf.Bytes(), nil
}
