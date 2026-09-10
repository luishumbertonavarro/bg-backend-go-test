package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func validador() Validator { return NewValidator(65536) }

func TestMessageAceptaPeticionConObjeto(t *testing.T) {
	msg, rej := validador().Message([]byte(`{"type":"peticion","id":"p1","ts":1,"payload":{"a":1}}`))
	if rej != nil {
		t.Fatalf("rechazada: %s", rej.Reason)
	}
	if string(msg.Payload) != `{"a":1}` {
		t.Errorf("el objeto tiene que llegar en crudo, llegó %s", msg.Payload)
	}
}

// El canal de eco existía antes que el payload en crudo y sigue mandando texto.
// Una cadena entrecomillada es JSON válido, así que tiene que seguir encajando.
func TestMessageSigueAceptandoTexto(t *testing.T) {
	msg, rej := validador().Message([]byte(`{"type":"echo","id":"e1","ts":1,"payload":"hola"}`))
	if rej != nil {
		t.Fatalf("rechazada: %s", rej.Reason)
	}
	if string(msg.Payload) != `"hola"` {
		t.Errorf("Payload = %s", msg.Payload)
	}
}

// Un payload ausente se normaliza en vez de dejar un null que el cliente
// recibiría de vuelta como algo que no envió.
func TestPayloadAusenteSeNormalizaSalvoEnPeticion(t *testing.T) {
	msg, rej := validador().Message([]byte(`{"type":"ping","id":"p","ts":1}`))
	if rej != nil {
		t.Fatalf("un ping sin payload es válido: %s", rej.Reason)
	}
	if string(msg.Payload) != `""` {
		t.Errorf("Payload = %s, se esperaba la cadena vacía", msg.Payload)
	}

	if _, rej := validador().Message([]byte(`{"type":"peticion","id":"p","ts":1}`)); rej == nil {
		t.Error("una peticion sin payload no tiene nada que preguntarle al .NET")
	}
}

func TestMessageRechazaScriptEnObjeto(t *testing.T) {
	if _, rej := validador().Message(
		[]byte(`{"type":"peticion","id":"p1","ts":1,"payload":{"x":"<script>alert(1)</script>"}}`),
	); rej == nil {
		t.Error("el saneado tiene que mirar dentro del objeto, no solo el texto suelto")
	}
}

func TestRespuestaBackendAceptaSesionVacia(t *testing.T) {
	env, rej := validador().RespuestaBackend([]byte(`{"Sesion":"","Payload":{"ok":true}}`))
	if rej != nil {
		t.Fatalf("rechazada: %s", rej.Reason)
	}
	if env.Session != "" {
		t.Errorf("Sesion = %q", env.Session)
	}
}

func TestRespuestaBackendExigeSesionUtilizableSiViene(t *testing.T) {
	if _, rej := validador().RespuestaBackend(
		[]byte(`{"Sesion":"../otra","Payload":{"ok":true}}`),
	); rej == nil {
		t.Error("una Sesion presente pero mal formada tiene que rechazarse")
	}
}

func TestRespuestaBackendExigePayload(t *testing.T) {
	if _, rej := validador().RespuestaBackend([]byte(`{"Sesion":"12345"}`)); rej == nil {
		t.Error("una respuesta sin Payload no entrega nada al cliente")
	}
}

func TestRespuestaBackendRechazaCampoDesconocido(t *testing.T) {
	if _, rej := validador().RespuestaBackend(
		[]byte(`{"Sesion":"12345","Payload":{},"extra":1}`),
	); rej == nil {
		t.Error("el esquema es cerrado también en la respuesta")
	}
}

func TestRespuestaBackendRespetaElLimiteDePayload(t *testing.T) {
	grande, _ := json.Marshal(strings.Repeat("a", 9000))
	if _, rej := validador().RespuestaBackend(
		[]byte(`{"Sesion":"12345","Payload":` + string(grande) + `}`),
	); rej == nil {
		t.Error("un payload por encima del tope tenía que rechazarse")
	}
}
