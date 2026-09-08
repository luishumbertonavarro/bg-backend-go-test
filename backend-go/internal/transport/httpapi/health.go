package httpapi

import "net/http"

// healthResponse es la vista del estado del proceso.
//
// Es un struct con etiquetas json y no un map porque esta respuesta es contrato
// público: la consumen las sondas del POC y el readiness de k8s.
type healthResponse struct {
	Stack             string `json:"stack"`
	Status            string `json:"status"`
	Instance          string `json:"instance"`
	ActiveConnections int32  `json:"activeConnections"`
	MaxConnections    int32  `json:"maxConnections"`
}

// handleHealth informa de la vida del proceso y de su ocupación.
//
// Sin autenticación a propósito: es la sonda que debe responder incluso cuando
// todo lo demás rechaza, para poder distinguir "servicio caído" de "servicio que
// deniega". Por eso no revela nada más que su propia capacidad.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{
		Stack:             "go",
		Status:            "ok",
		Instance:          s.cfg.InstanceID,
		ActiveConnections: s.registry.Active(),
		MaxConnections:    s.registry.Max(),
	})
}
