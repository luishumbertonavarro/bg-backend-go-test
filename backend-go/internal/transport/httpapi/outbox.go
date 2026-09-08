package httpapi

import (
	"net/http"

	"wspoc-go/internal/protocol"
)

// outboxResponse es la vista del buffer de sobres pendientes.
type outboxResponse struct {
	// WebhookURL vacía significa modo inspección: no se está enviando a ningún sitio.
	WebhookURL string              `json:"webhookUrl"`
	Count      int                 `json:"count"`
	Items      []protocol.Envelope `json:"items"`
}

// handleOutbox devuelve los sobres que se le enviarían (o se le enviaron) al
// .NET 4.8. Existe porque todavía no hay URL del .NET: permite ver el contrato
// exacto sin tener el otro extremo levantado.
//
// Está autenticado porque expone el contenido de los mensajes de TODOS los
// usuarios junto a su identificador de sesión: abierto sería una fuga directa de
// datos de terceros, no solo un endpoint de diagnóstico.
func (s *Server) handleOutbox(w http.ResponseWriter, r *http.Request) {
	if s.setAPICORS(w, r) {
		return // preflight resuelto
	}
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	if !s.authorize(w, r, "outbox") {
		return
	}

	items := s.outbox.Items()
	writeJSON(w, http.StatusOK, outboxResponse{
		WebhookURL: s.outbox.WebhookURL(),
		Count:      len(items),
		Items:      items,
	})
}
