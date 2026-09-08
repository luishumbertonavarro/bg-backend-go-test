// Package protocol define el contrato del POC: los mensajes que viajan por el
// canal, los sobres que se intercambian con el .NET 4.8 y el catálogo de
// rechazos compartido por los cuatro backends (SECURITY-CHECKLIST.md §9).
//
// Es deliberadamente independiente de config y de net/http en su lógica: solo
// conoce formas de datos y reglas de validación, no de dónde salen los límites
// ni cómo se transportan. Quien lo usa inyecta lo uno y traduce lo otro.
package protocol

import (
	"net/http"
	"time"
)

// Rejection es un motivo de rechazo del contrato del POC.
//
// Antes, cada punto de rechazo repetía a mano la terna (reason, code, status) y
// nada garantizaba que un mismo motivo se reportara igual desde el handshake y
// desde el REST. Aquí el catálogo es único y los tres campos viajan juntos.
type Rejection struct {
	// Reason es la constante compartida por los cuatro backends del POC.
	Reason string
	// Code es el código de cierre WebSocket que recibiría el cliente.
	Code int
	// HTTPStatus es el equivalente cuando el rechazo se responde por HTTP.
	HTTPStatus int
}

// Catálogo de rechazos. Cualquier motivo nuevo se añade aquí, no en el punto de uso.
var (
	TokenMissing       = Rejection{"TOKEN_MISSING", 4001, http.StatusUnauthorized}
	TokenInvalid       = Rejection{"TOKEN_INVALID", 4002, http.StatusUnauthorized}
	TokenExpired       = Rejection{"TOKEN_EXPIRED", 4003, http.StatusUnauthorized}
	TokenClaimsInvalid = Rejection{"TOKEN_CLAIMS_INVALID", 4004, http.StatusUnauthorized}
	RateLimitExceeded  = Rejection{"RATE_LIMIT_EXCEEDED", 4008, http.StatusTooManyRequests}
	MessageTooLarge    = Rejection{"MESSAGE_TOO_LARGE", 4009, http.StatusRequestEntityTooLarge}
	InvalidPayload     = Rejection{"INVALID_PAYLOAD", 4010, http.StatusBadRequest}
	InvalidSession     = Rejection{"INVALID_SESSION", 4010, http.StatusBadRequest}
	ServerAtCapacity   = Rejection{"SERVER_AT_CAPACITY", 4013, http.StatusServiceUnavailable}
	IdleTimeout        = Rejection{"IDLE_TIMEOUT", 4014, http.StatusRequestTimeout}
	OriginNotAllowed   = Rejection{"ORIGIN_NOT_ALLOWED", 4403, http.StatusForbidden}
	SessionNotFound    = Rejection{"SESSION_NOT_FOUND", 404, http.StatusNotFound}
	Backpressure       = Rejection{"BACKPRESSURE", 4009, http.StatusServiceUnavailable}
)

// Tipos de mensaje que el cliente puede enviar. El enum es cerrado: cualquier
// otro valor es INVALID_PAYLOAD.
const (
	TypePing = "ping"
	TypeEcho = "echo"
)

// Tipos de frame que emite el servidor.
const (
	TypePong  = "pong"
	TypePush  = "push"
	TypeError = "error"
)

// ClientMessage es el único esquema aceptado por el canal (§4).
type ClientMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	TS      int64  `json:"ts"`
	Payload string `json:"payload"`
}

// Envelope es el sobre que se intercambia con el backend .NET 4.8, en los dos
// sentidos: {sesion, payload}.
//
// El campo se llama `sesion` en castellano porque es el nombre acordado del
// contrato con el .NET, no un descuido de nomenclatura.
type Envelope struct {
	Session string `json:"sesion"`
	Payload string `json:"payload"`
}

// Frame es un mensaje servidor -> cliente.
//
// `instance` y `sesion` son campos extra legítimos: el esquema estricto del §4
// solo rige en sentido cliente -> servidor.
//
// Es un struct y no un map[string]any porque el compilador puede entonces
// verificar los campos, y porque `omitempty` reproduce con exactitud la regla de
// que `reason` solo aparece cuando hay motivo de error.
type Frame struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	TS       int64  `json:"ts"`
	Payload  string `json:"payload"`
	Instance string `json:"instance"`
	Session  string `json:"sesion"`
	Reason   string `json:"reason,omitempty"`
}

// Framer construye los frames de salida de esta réplica.
//
// Existe para que la identidad de la instancia se inyecte una sola vez al
// arrancar, en lugar de que cada punto de construcción alcance una variable global.
type Framer struct {
	Instance string
}

// NewFramer crea el constructor de frames de una réplica.
func NewFramer(instance string) Framer { return Framer{Instance: instance} }

// Pong responde a un ping del cliente.
func (f Framer) Pong(id, session string) Frame {
	return f.frame(TypePong, id, "", "", session)
}

// Echo devuelve al cliente su propio payload ya validado.
func (f Framer) Echo(id, payload, session string) Frame {
	return f.frame(TypeEcho, id, payload, "", session)
}

// Push entrega a una sesión un payload que llegó del .NET 4.8.
func (f Framer) Push(payload, session string) Frame {
	return f.frame(TypePush, TypePush, payload, "", session)
}

// Error informa del motivo antes de cerrar la conexión.
func (f Framer) Error(rejection Rejection, session string) Frame {
	return f.frame(TypeError, "-", "", rejection.Reason, session)
}

func (f Framer) frame(kind, id, payload, reason, session string) Frame {
	return Frame{
		Type:     kind,
		ID:       id,
		TS:       time.Now().UnixMilli(),
		Payload:  payload,
		Instance: f.Instance,
		Session:  session,
		Reason:   reason,
	}
}
