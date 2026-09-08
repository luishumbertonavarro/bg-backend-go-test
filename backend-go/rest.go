package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
)

// Envelope es el sobre que se intercambia con el backend .NET 4.8, en los dos
// sentidos: {sesion, payload}.
type Envelope struct {
	Session string `json:"sesion"`
	Payload string `json:"payload"`
}

var (
	outbox      *Outbox
	pushLimiter *SlidingWindowLimiter
	pushMu      sync.Mutex
)

// ------------------------------------------------------------------ .NET -> cliente

// handlePush recibe del .NET 4.8 un {sesion, payload} y lo entrega por WebSocket
// a esa sesión. Es el reemplazo del canal push del .NET Framework 4.5.
func handlePush(w http.ResponseWriter, r *http.Request) {
	if setAPICORS(w, r) {
		return // preflight resuelto
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"reason": "METHOD_NOT_ALLOWED"})
		return
	}

	// Mismo control de token que el handshake (§2), pero leyendo la cabecera:
	// el .NET no es un navegador, así que sí puede enviar cabeceras.
	if verdict := CheckToken(bearerToken(r), cfg); !verdict.Allowed {
		log.Printf("[REJECT] stack=go reason=%s code=%d remote=%s origin=- detail=push",
			verdict.Reason, verdict.Code, r.RemoteAddr)
		writeJSON(w, verdict.HTTPStatus, map[string]any{"reason": verdict.Reason, "code": verdict.Code})
		return
	}

	// El rate limit es global del endpoint, así que necesita exclusión mutua:
	// el limitador por conexión no la necesitaba porque solo lo usaba su goroutine.
	pushMu.Lock()
	allowed := pushLimiter.Allow()
	pushMu.Unlock()
	if !allowed {
		log.Printf("[REJECT] stack=go reason=RATE_LIMIT_EXCEEDED code=4008 remote=%s origin=- detail=push limit=%d/s",
			r.RemoteAddr, cfg.PushRateLimitPerSec)
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"reason": "RATE_LIMIT_EXCEEDED", "code": 4008})
		return
	}

	raw, err := readLimited(r, cfg.MaxMessageBytes)
	if err != nil {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"reason": "MESSAGE_TOO_LARGE", "code": 4009})
		return
	}

	env, reason := ValidateEnvelope(raw, cfg)
	if env == nil {
		log.Printf("[REJECT] stack=go reason=%s code=4010 remote=%s origin=- detail=push", reason, r.RemoteAddr)
		writeJSON(w, http.StatusBadRequest, map[string]any{"reason": reason, "code": 4010})
		return
	}

	delivered := deliverer.Deliver(env.Session, frame("push", "push", env.Payload, "", env.Session))
	if delivered == 0 {
		log.Printf("[REJECT] stack=go reason=SESSION_NOT_FOUND code=404 remote=%s origin=- detail=sesion=%s",
			r.RemoteAddr, env.Session)
		writeJSON(w, http.StatusNotFound, map[string]any{"reason": "SESSION_NOT_FOUND", "sesion": env.Session})
		return
	}

	log.Printf("[PUSH] stack=go sesion=%s conns=%d bytes=%d", env.Session, delivered, len(env.Payload))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"delivered": delivered, "sesion": env.Session, "instance": cfg.InstanceID,
	})
}

// ------------------------------------------------------------------ cliente -> .NET

// handleOutbox devuelve los sobres que se le enviarían (o se le enviaron) al
// .NET 4.8. Existe porque todavía no hay URL del .NET: permite ver el contrato
// exacto sin tener el otro extremo levantado.
func handleOutbox(w http.ResponseWriter, r *http.Request) {
	if setAPICORS(w, r) {
		return // preflight resuelto
	}

	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"reason": "METHOD_NOT_ALLOWED"})
		return
	}
	if verdict := CheckToken(bearerToken(r), cfg); !verdict.Allowed {
		writeJSON(w, verdict.HTTPStatus, map[string]any{"reason": verdict.Reason, "code": verdict.Code})
		return
	}

	items := outbox.Items()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"webhookUrl": cfg.DotNetWebhookURL, // vacío = modo inspección
		"count":      len(items),
		"items":      items,
	})
}

// Outbox guarda los últimos sobres en memoria, en un buffer circular.
type Outbox struct {
	mu    sync.Mutex
	items []Envelope
	max   int
}

func NewOutbox(max int) *Outbox {
	if max <= 0 {
		max = 200
	}
	return &Outbox{max: max, items: make([]Envelope, 0, max)}
}

// Add registra el sobre y, si hay URL configurada, lo envía al .NET 4.8.
func (o *Outbox) Add(env Envelope) {
	raw, _ := json.Marshal(env)
	log.Printf("[OUTBOX] stack=go %s", raw)

	o.mu.Lock()
	if len(o.items) >= o.max {
		o.items = o.items[1:]
	}
	o.items = append(o.items, env)
	o.mu.Unlock()

	if cfg.DotNetWebhookURL != "" {
		// En una goroutine: el .NET no debe poder frenar el bucle de lectura del
		// cliente. Un fallo se registra y se descarta, no tumba la conexión.
		go postToDotNet(raw)
	}
}

func (o *Outbox) Items() []Envelope {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Envelope, len(o.items))
	copy(out, o.items)
	return out
}

func postToDotNet(raw []byte) {
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("[REJECT] stack=go reason=WEBHOOK_FAULT code=500 remote=%s origin=- detail=%v",
				cfg.DotNetWebhookURL, rec)
		}
	}()

	resp, err := dotNetClient.Post(cfg.DotNetWebhookURL, "application/json", bytes.NewReader(raw))
	if err != nil {
		log.Printf("[REJECT] stack=go reason=WEBHOOK_UNREACHABLE code=502 remote=%s origin=- detail=%v",
			cfg.DotNetWebhookURL, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[REJECT] stack=go reason=WEBHOOK_REJECTED code=%d remote=%s origin=- detail=-",
			resp.StatusCode, cfg.DotNetWebhookURL)
	}
}

// ----------------------------------------------------------------------- ayudas

// setAPICORS prepara la respuesta para que el navegador pueda llamar a estos
// endpoints desde :4200.
//
// No basta con Allow-Origin, que es lo que necesita el diagnostico: como estas
// peticiones llevan cabecera Authorization, el navegador manda antes un OPTIONS
// de preflight y, si no se le contesta con los metodos y cabeceras permitidos,
// bloquea la llamada real. La UI lo veia como SERVIDOR_INALCANZABLE, que es
// justo el sintoma que despista.
//
// Devuelve true si la peticion era el preflight y ya esta contestada.
func setAPICORS(w http.ResponseWriter, r *http.Request) bool {
	setDiagnosticCORS(w, r) // origen concreto de la lista blanca, nunca "*"
	origin := r.Header.Get("Origin")
	if _, allowed := cfg.AllowedOrigins[origin]; origin != "" && allowed {
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "600")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	// El prefijo "Bearer " es opcional: se acepta también el token pelado.
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return strings.TrimSpace(auth)
}

// readLimited lee el cuerpo cortando un byte por encima del máximo, igual que
// hace el canal WebSocket con los frames grandes (§5).
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
