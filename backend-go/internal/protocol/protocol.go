// Package protocol define el contrato del POC: los mensajes que viajan por el
// canal, los sobres que se intercambian con el .NET 4.8 y el catálogo de
// rechazos que se responde a unos y otros.
//
// Es deliberadamente independiente de config y de net/http en su lógica: solo
// conoce formas de datos y reglas de validación, no de dónde salen los límites
// ni cómo se transportan. Quien lo usa inyecta lo uno y traduce lo otro.
package protocol

import (
	"encoding/json"
	"net/http"
	"time"
)

// Rejection es un motivo de rechazo del contrato del POC.
//
// Antes, cada punto de rechazo repetía a mano la terna (reason, code, status) y
// nada garantizaba que un mismo motivo se reportara igual desde el handshake y
// desde el REST. Aquí el catálogo es único y los tres campos viajan juntos.
type Rejection struct {
	// Reason es la constante que identifica el motivo ante quien recibe el rechazo.
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

// ClientMessage es el único esquema aceptado por el canal.
type ClientMessage struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	TS      int64  `json:"ts"`
	Payload string `json:"payload"`
}

// Envelope es el sobre que se intercambia con el backend .NET 4.8, en los dos
// sentidos: {Sesion, Identificador, Payload}.
//
// Las claves van en castellano y en mayúscula porque es la grafía exacta con la
// que el .NET serializa, no un descuido de nomenclatura. Importa en el sentido
// de salida (el outbox reenvía este mismo struct): System.Text.Json distingue
// mayúsculas por defecto, así que emitirlas en minúscula obligaría al otro
// extremo a configurar el deserializador.
//
// En el sentido de ENTRADA la grafía es indiferente: encoding/json empareja las
// claves ignorando mayúsculas, así que un `{"sesion", "payload"}` en minúscula
// —lo que mandan los clientes antiguos— sigue encajando aquí.
type Envelope struct {
	Session string `json:"Sesion"`
	// Identificador es el correlativo del mensaje en el .NET. Es opcional: en el
	// sentido cliente -> .NET (el eco que guarda el outbox) no hay ninguno, y
	// `omitempty` lo omite en vez de emitir un 0 que nadie envió.
	Identificador int64 `json:"Identificador,omitempty"`
	// Payload es JSON en crudo: el .NET manda un objeto cuya forma interna cambia
	// según el caso de uso, y este servicio no tiene por qué conocerla.
	//
	// json.RawMessage y no una estructura concreta porque el contenido es del
	// .NET y del frontend, no de aquí: modelarlo obligaría a tocar este servicio
	// cada vez que le añadan un campo, y a desplegarlo para nada.
	Payload json.RawMessage `json:"Payload"`
}

// SessionTokenRequest es lo que manda el frontend tras el login para canjear su
// SESION por un token del canal.
//
// El campo va en mayúsculas porque es el nombre tal cual sale del payload de
// login: obligar al frontend a renombrarlo solo abriría la puerta a que lo
// renombrara mal.
type SessionTokenRequest struct {
	Session string `json:"SESION"`
}

// Frame es un mensaje servidor -> cliente.
//
// `instance` y `Sesion` son campos extra legítimos: el esquema estricto
// solo rige en sentido cliente -> servidor.
//
// Ojo a la asimetría: el cliente ENVÍA `payload` en minúscula (ClientMessage) y
// RECIBE `Payload` en mayúscula. No es un descuido — se envía en el lenguaje del
// canal y se recibe con la grafía del dato— pero es lo primero que despista al
// escribir el frontend.
//
// Es un struct y no un map[string]any porque el compilador puede entonces
// verificar los campos, y porque `omitempty` reproduce con exactitud la regla de
// que `reason` solo aparece cuando hay motivo de error.
type Frame struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	TS       int64  `json:"ts"`
	Instance string `json:"instance"`
	Reason   string `json:"reason,omitempty"`

	// Campos de dato, con la grafía del .NET: lo que el navegador lee como
	// contenido llega escrito igual que salió de allí. La regla del frame es esa:
	// mayúscula lo que es dato, minúscula lo que es protocolo del canal.
	Session       string `json:"Sesion"`
	Identificador int64  `json:"Identificador,omitempty"`
	// Payload viaja en crudo para que el objeto del .NET llegue al navegador tal
	// cual, sin que este servicio lo reescriba ni el frontend tenga que
	// deserializarlo dos veces.
	//
	// Su tipo JSON depende del `type` del frame: objeto en un `push`, cadena en un
	// `echo` (que devuelve lo que mandó el cliente) y cadena vacía en `pong` y
	// `error`. El frontend ya distingue por `type`, así que no necesita adivinarlo.
	Payload json.RawMessage `json:"Payload"`
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
	return f.frame(TypePong, id, RawText(""), "", session)
}

// Echo devuelve al cliente su propio payload ya validado.
func (f Framer) Echo(id, payload, session string) Frame {
	return f.frame(TypeEcho, id, RawText(payload), "", session)
}

// RawText convierte un texto plano en el JSON que lo representa: la cadena
// entrecomillada y escapada.
//
// Hace falta porque los frames del canal (eco, pong, error) llevan texto, no el
// objeto del .NET, y el campo por el que salen es JSON en crudo. Marshal de un
// string no puede fallar, así que el error se ignora a conciencia.
func RawText(text string) json.RawMessage {
	raw, _ := json.Marshal(text)
	return raw
}

// Push entrega a una sesión un payload que llegó del .NET 4.8.
//
// El identificador viaja tal cual hasta el navegador: es el correlativo con el
// que el .NET reconoce su propio mensaje, así que perderlo por el camino dejaría
// al frontend sin forma de casar lo que recibe con lo que se pidió.
func (f Framer) Push(payload json.RawMessage, session string, identificador int64) Frame {
	frame := f.frame(TypePush, TypePush, payload, "", session)
	frame.Identificador = identificador
	return frame
}

// Error informa del motivo antes de cerrar la conexión.
func (f Framer) Error(rejection Rejection, session string) Frame {
	return f.frame(TypeError, "-", RawText(""), rejection.Reason, session)
}

func (f Framer) frame(kind, id string, payload json.RawMessage, reason, session string) Frame {
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
