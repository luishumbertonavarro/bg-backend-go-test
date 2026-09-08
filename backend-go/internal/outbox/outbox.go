// Package outbox guarda y reenvía al backend .NET 4.8 los sobres que genera el
// cliente (sentido cliente -> .NET).
//
// Existe en modo inspeccionable porque todavía no hay URL del .NET: permite ver
// el contrato exacto sin tener el otro extremo levantado.
package outbox

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"wspoc-go/internal/config"
	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
)

// defaultSize es el tamaño del buffer si la configuración no da uno válido.
const defaultSize = 200

// Outbox guarda los últimos sobres en memoria, en un buffer circular, y —si hay
// URL configurada— los reenvía al .NET 4.8.
type Outbox struct {
	mu    sync.Mutex
	items []protocol.Envelope
	max   int

	webhookURL string
	client     *http.Client
}

// New crea el outbox con la política de reenvío que dicte la configuración.
//
// El cliente HTTP lleva timeout SIEMPRE: un backend lento no debe poder acumular
// goroutines aquí.
func New(cfg config.Config) *Outbox {
	max := cfg.OutboxSize
	if max <= 0 {
		max = defaultSize
	}
	return &Outbox{
		max:        max,
		items:      make([]protocol.Envelope, 0, max),
		webhookURL: cfg.DotNetWebhookURL,
		client:     &http.Client{Timeout: time.Duration(cfg.DotNetTimeoutSeconds) * time.Second},
	}
}

// WebhookURL es el destino configurado. Cadena vacía = modo inspección.
func (o *Outbox) WebhookURL() string { return o.webhookURL }

// Add registra el sobre y, si hay URL configurada, lo envía al .NET 4.8.
func (o *Outbox) Add(env protocol.Envelope) {
	raw, err := json.Marshal(env)
	if err != nil {
		return
	}
	logging.Outbox(raw)
	o.store(env)

	if o.webhookURL != "" {
		// En una goroutine: el .NET no debe poder frenar el bucle de lectura del
		// cliente. Un fallo se registra y se descarta, no tumba la conexión.
		go o.post(raw)
	}
}

// store añade al buffer circular, descartando el más antiguo si está lleno.
func (o *Outbox) store(env protocol.Envelope) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if len(o.items) >= o.max {
		o.items = o.items[1:]
	}
	o.items = append(o.items, env)
}

// Items devuelve una copia de los sobres guardados, del más antiguo al más reciente.
func (o *Outbox) Items() []protocol.Envelope {
	o.mu.Lock()
	defer o.mu.Unlock()

	out := make([]protocol.Envelope, len(o.items))
	copy(out, o.items)
	return out
}

// post entrega el sobre al .NET 4.8. Nunca propaga el fallo: el puente es
// best-effort y la conexión del cliente no depende de que el otro extremo viva.
func (o *Outbox) post(raw []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			logging.RejectRaw("WEBHOOK_FAULT", 500, o.webhookURL, "", logging.Detail(rec))
		}
	}()

	resp, err := o.client.Post(o.webhookURL, "application/json", bytes.NewReader(raw))
	if err != nil {
		logging.RejectRaw("WEBHOOK_UNREACHABLE", 502, o.webhookURL, "", err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices {
		logging.RejectRaw("WEBHOOK_REJECTED", resp.StatusCode, o.webhookURL, "", "")
	}
}
