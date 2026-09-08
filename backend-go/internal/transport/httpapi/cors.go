package httpapi

import "net/http"

// preflightMaxAge es cuánto puede cachear el navegador la respuesta al preflight.
const preflightMaxAge = "600"

// setDiagnosticCORS permite que el frontend, que vive en otro puerto y por tanto en otro
// origen, pueda leer la respuesta del diagnóstico; sin esta cabecera el navegador la
// bloquea y la UI no puede mostrar el motivo del rechazo. Se devuelve el origen concreto
// y solo si está en la lista blanca — nunca "*", que abriría el endpoint a cualquier web.
func (s *Server) setDiagnosticCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" || !s.cfg.OriginAllowed(origin) {
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Vary", "Origin")
}

// setAPICORS prepara la respuesta para que el navegador pueda llamar a los
// endpoints del puente REST desde :4200.
//
// No basta con Allow-Origin, que es lo que necesita el diagnóstico: como estas
// peticiones llevan cabecera Authorization, el navegador manda antes un OPTIONS
// de preflight y, si no se le contesta con los métodos y cabeceras permitidos,
// bloquea la llamada real. La UI lo veía como SERVIDOR_INALCANZABLE, que es
// justo el síntoma que despista.
//
// Devuelve true si la petición era el preflight y ya está contestada.
func (s *Server) setAPICORS(w http.ResponseWriter, r *http.Request) bool {
	s.setDiagnosticCORS(w, r) // origen concreto de la lista blanca, nunca "*"

	if origin := r.Header.Get("Origin"); origin != "" && s.cfg.OriginAllowed(origin) {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", preflightMaxAge)
	}

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}
