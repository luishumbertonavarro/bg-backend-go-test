package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// errTrailingContent señala un segundo documento JSON pegado detrás del primero.
var errTrailingContent = errors.New("contenido sobrante tras el documento JSON")

// rejected devuelve una COPIA del motivo del catálogo.
//
// Devolver la dirección de una entrada del catálogo entregaría un puntero a una
// variable de paquete, y cualquier llamante podría alterarla para todo el proceso.
func rejected(r Rejection) *Rejection { return &r }

// Límites del contrato (§4). Son del protocolo, no del despliegue, así que viven
// aquí y no en la configuración: cambiarlos cambia lo que los cuatro backends
// aceptan, no cómo se despliega este.
const (
	maxPayloadChars = 8192
	maxIDChars      = 64
	maxSessionChars = 64
)

// scriptPatterns son los patrones que se rechazan en cualquier payload.
//
// Es defensa en profundidad, no la única capa: el frontend debe seguir escapando
// al pintar. Un filtro de patrones no sustituye al escapado, pero corta lo
// evidente antes de que se propague.
var scriptPatterns = []string{"<script", "javascript:", "onerror=", "onload=", "<iframe", "data:text/html"}

// Validator aplica las reglas del §4. Recibe el límite de tamaño por inyección
// para no depender del paquete de configuración.
type Validator struct {
	MaxMessageBytes int64
}

// NewValidator crea el validador del canal.
func NewValidator(maxMessageBytes int64) Validator {
	return Validator{MaxMessageBytes: maxMessageBytes}
}

// Message valida y sanea un frame entrante del cliente (§4).
//
// Devuelve un *Rejection nil cuando el mensaje es válido; así el llamante no
// puede olvidarse de comprobar el resultado, como pasaba con el `reason string`
// vacío que devolvía la versión anterior.
func (v Validator) Message(raw []byte) (ClientMessage, *Rejection) {
	if int64(len(raw)) > v.MaxMessageBytes {
		return ClientMessage{}, rejected(MessageTooLarge)
	}

	var msg ClientMessage
	if err := decodeStrict(raw, &msg); err != nil {
		return ClientMessage{}, rejected(InvalidPayload)
	}

	if msg.Type != TypeEcho && msg.Type != TypePing {
		return ClientMessage{}, rejected(InvalidPayload)
	}
	if msg.TS == 0 {
		return ClientMessage{}, rejected(InvalidPayload)
	}
	if !isValidID(msg.ID, maxIDChars) {
		return ClientMessage{}, rejected(InvalidPayload)
	}
	if !isCleanText(msg.Payload) {
		return ClientMessage{}, rejected(InvalidPayload)
	}
	return msg, nil
}

// Envelope valida el sobre {sesion, payload} que manda el .NET 4.8.
//
// Aplica la misma sanitización que el canal WebSocket a propósito: el push acaba
// en el DOM del navegador igual que un eco, así que no puede ser un camino más laxo.
func (v Validator) Envelope(raw []byte) (Envelope, *Rejection) {
	var env Envelope
	if err := decodeStrict(raw, &env); err != nil {
		return Envelope{}, rejected(InvalidPayload)
	}
	if !isValidID(env.Session, maxSessionChars) {
		return Envelope{}, rejected(InvalidSession)
	}
	if !isCleanText(env.Payload) {
		return Envelope{}, rejected(InvalidPayload)
	}
	return env, nil
}

// decodeStrict decodifica con esquema cerrado: cualquier campo fuera del
// contrato, o un segundo documento JSON pegado detrás, hacen fallar la decodificación.
//
// Lo segundo importa porque json.Decoder se detiene tras el primer documento: sin
// comprobar el resto, un cuerpo con dos sobres aceptaría el primero y descartaría
// el segundo en silencio, y lo que quedara en los logs dejaría de coincidir con
// lo que de verdad se procesó.
func decodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.More() {
		return errTrailingContent
	}
	return nil
}

// ValidSessionID aplica al identificador de sesión las mismas reglas que al
// `sesion` de un sobre.
//
// Lo usa la validación del token: la sesión sale del claim `sid` (o de `sub`), y
// un identificador que no valdría en un sobre tampoco puede valer viniendo de un
// token. Tener una sola definición evita que las dos rutas diverjan.
func ValidSessionID(session string) bool {
	return isValidID(session, maxSessionChars)
}

// isValidID valida un identificador que va a viajar en URLs, en claves de Redis
// y en logs: no vacío, dentro de longitud y con alfabeto restringido.
func isValidID(id string, maxChars int) bool {
	if len(id) == 0 || len(id) > maxChars {
		return false
	}
	return isSafeID(id)
}

// isCleanText comprueba que un payload es apto para llegar al DOM del navegador:
// dentro de longitud, sin caracteres de control y sin patrones de script.
func isCleanText(payload string) bool {
	if len(payload) > maxPayloadChars {
		return false
	}
	if hasControlChars(payload) {
		return false
	}
	lowered := strings.ToLower(payload)
	for _, pattern := range scriptPatterns {
		if strings.Contains(lowered, pattern) {
			return false
		}
	}
	return true
}

// isSafeID restringe el alfabeto a [A-Za-z0-9_-]. Cierra el paso a barras y ".."
// que confundirían a un enrutado por path, y a saltos de línea que permitirían
// falsificar entradas en el log, ya que el identificador se escribe tal cual.
func isSafeID(id string) bool {
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

func hasControlChars(s string) bool {
	for _, c := range s {
		if c < 32 && c != '\t' && c != '\n' && c != '\r' {
			return true
		}
	}
	return false
}
