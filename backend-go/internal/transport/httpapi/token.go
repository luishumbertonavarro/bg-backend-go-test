package httpapi

import (
	"net/http"

	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
)

// sessionTokenResponse es el token recién emitido y su vigencia.
//
// ExpiraEn va en segundos y no como fecha absoluta: el frontend solo necesita
// saber cuándo volver a pedirlo, y así no depende de que su reloj coincida con
// el del servidor.
type sessionTokenResponse struct {
	Token string `json:"token"`
	// SESION se devuelve con la misma grafía con la que se pidió, que es la del
	// payload de login: el frontend no debería tener que recordar dos formas de
	// escribir el mismo dato según lo mande o lo reciba.
	Session  string `json:"SESION"`
	ExpiraEn int    `json:"expira_en"`
	Instance string `json:"instance"`
}

// handleSessionToken canjea la SESION que el frontend recibió al hacer login por
// un token del canal, que es lo que el handshake de /ws sabe leer.
//
// Es el único endpoint sin Authorization, porque es precisamente el que la
// produce. Eso lo convierte en la superficie pública del servicio: quien presente
// una SESION válida obtiene acceso a sus mensajes. Los controles de abajo son lo
// único que separa eso de que cualquier web acuñe tokens ajenos.
func (s *Server) handleSessionToken(w http.ResponseWriter, r *http.Request) {
	if s.setAPICORS(w, r) {
		return // preflight resuelto
	}
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	if !s.requireBrowserOrigin(w, r) {
		return
	}
	if !s.allowTokenRate(w, r) {
		return
	}

	raw, err := readLimited(r, s.cfg.MaxMessageBytes)
	if err != nil {
		writeRejection(w, protocol.MessageTooLarge)
		return
	}

	req, rejection := s.validator.SessionTokenRequest(raw)
	if rejection != nil {
		logging.Reject(*rejection, r.RemoteAddr, r.Header.Get("Origin"), "session-token")
		writeRejection(w, *rejection)
		return
	}

	s.issueSessionToken(w, r, req.Session)
}

// issueSessionToken firma el token de una sesión ya validada y lo responde.
func (s *Server) issueSessionToken(w http.ResponseWriter, r *http.Request, session string) {
	token, ttl, err := s.issuer.ForSession(session)
	if err != nil {
		// La sesión ya pasó el validador, así que llegar aquí significa un fallo
		// de firma: es un problema del servidor, no de quien llama.
		logging.RejectRaw("TOKEN_ISSUE_FAILED", 1011, r.RemoteAddr, r.Header.Get("Origin"), err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{"reason": "TOKEN_ISSUE_FAILED"})
		return
	}

	seconds := int(ttl.Seconds())
	logging.Token(session, r.RemoteAddr, r.Header.Get("Origin"), seconds)
	writeJSON(w, http.StatusOK, sessionTokenResponse{
		Token:    token,
		Session:  session,
		ExpiraEn: seconds,
		Instance: s.cfg.InstanceID,
	})
}

// requireBrowserOrigin exige un Origin presente y en la lista blanca.
//
// Es más estricto que Guard.CheckOrigin a propósito: aquel deja pasar la petición
// sin Origin para que las pruebas de carga desde Node/k6 funcionen, y ese hueco
// aquí valdría por sí solo para emitir tokens desde cualquier cliente que no sea
// un navegador. A este endpoint solo llega el frontend, así que no lo necesita.
func (s *Server) requireBrowserOrigin(w http.ResponseWriter, r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && s.cfg.OriginAllowed(origin) {
		return true
	}
	logging.Reject(protocol.OriginNotAllowed, r.RemoteAddr, r.Header.Get("Origin"), "session-token")
	writeRejection(w, protocol.OriginNotAllowed)
	return false
}

// allowTokenRate aplica el tope de emisiones por segundo.
//
// Tiene cupo propio, separado del push: acotar cuántos tokens se pueden pedir por
// segundo es lo que hace caro enumerar SESIONes a ciegas.
func (s *Server) allowTokenRate(w http.ResponseWriter, r *http.Request) bool {
	if s.tokenLimiter.Allow() {
		return true
	}
	logging.Reject(protocol.RateLimitExceeded, r.RemoteAddr, r.Header.Get("Origin"), "session-token")
	writeRejection(w, protocol.RateLimitExceeded)
	return false
}
