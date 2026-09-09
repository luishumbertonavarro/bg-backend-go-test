// Package httpapi es la capa de transporte: traduce peticiones HTTP y frames
// WebSocket a llamadas al dominio, y veredictos del dominio a respuestas.
//
// Es la única capa que conoce net/http y gorilla/websocket. Ninguna decisión de
// negocio se toma aquí: si un rechazo cambia de motivo, se cambia en el catálogo
// del protocolo, no en un handler.
package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/gorilla/websocket"

	"wspoc-go/internal/config"
	"wspoc-go/internal/delivery"
	"wspoc-go/internal/outbox"
	"wspoc-go/internal/protocol"
	"wspoc-go/internal/ratelimit"
	"wspoc-go/internal/security"
	"wspoc-go/internal/session"
)

// handshakeTimeout acota el upgrade del WebSocket.
const handshakeTimeout = 10 * time.Second

// socketBufferSize es el buffer de lectura y escritura del socket.
const socketBufferSize = 8192

// Server agrupa las dependencias de los handlers.
//
// Antes cada handler alcanzaba variables globales (cfg, registry, outbox,
// deliverer, pushLimiter). Con eso, qué necesita cada handler solo se sabía
// leyéndolo entero, y no había forma de levantar dos configuraciones distintas
// en un test. Aquí las dependencias son explícitas y se inyectan al construir.
type Server struct {
	cfg         config.Config
	guard       security.Guard
	issuer      security.Issuer
	validator   protocol.Validator
	framer      protocol.Framer
	registry    *session.Registry
	deliverer   delivery.Deliverer
	outbox      *outbox.Outbox
	pushLimiter *ratelimit.SlidingWindow
	// tokenLimiter tiene cupo propio: el intercambio lo llama el navegador y no
	// puede competir por el mismo cupo que el puente con el .NET.
	tokenLimiter *ratelimit.SlidingWindow
	upgrader     websocket.Upgrader
}

// Deps son las dependencias que el arranque construye y el servidor consume.
//
// Es un struct con nombres en vez de siete parámetros posicionales: con tantos
// del mismo tipo, un intercambio accidental compilaría sin protestar.
type Deps struct {
	Config    config.Config
	Registry  *session.Registry
	Deliverer delivery.Deliverer
	Outbox    *outbox.Outbox
}

// New construye el servidor con sus dependencias ya resueltas.
func New(deps Deps) *Server {
	return &Server{
		cfg:          deps.Config,
		guard:        security.NewGuard(deps.Config),
		issuer:       security.NewIssuer(deps.Config),
		validator:    protocol.NewValidator(deps.Config.MaxMessageBytes),
		framer:       protocol.NewFramer(deps.Config.InstanceID),
		registry:     deps.Registry,
		deliverer:    deps.Deliverer,
		outbox:       deps.Outbox,
		pushLimiter:  ratelimit.NewSlidingWindow(deps.Config.PushRateLimitPerSec),
		tokenLimiter: ratelimit.NewSlidingWindow(deps.Config.SessionTokenRateLimitPerSec),
		upgrader: websocket.Upgrader{
			HandshakeTimeout: handshakeTimeout,
			ReadBufferSize:   socketBufferSize,
			WriteBufferSize:  socketBufferSize,
			// El Origin ya se validó en evaluate(); gorilla no debe volver a decidir.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

// Routes declara el enrutado. Tenerlo en un solo sitio hace que la superficie
// HTTP del servicio se lea de un vistazo.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("/api/push", s.handlePush)
	mux.HandleFunc("/api/session-token", s.handleSessionToken)
	mux.HandleFunc("/api/outbox", s.handleOutbox)
	return mux
}

// ---------------------------------------------------------------- respuestas

// writeJSON escribe una respuesta JSON con su código.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeRejection responde un rechazo del catálogo en el formato que esperan los
// clientes del POC: {reason, code}.
func writeRejection(w http.ResponseWriter, rejection protocol.Rejection) {
	writeJSON(w, rejection.HTTPStatus, map[string]any{
		"reason": rejection.Reason,
		"code":   rejection.Code,
	})
}
