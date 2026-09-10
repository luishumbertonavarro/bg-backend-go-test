package httpapi

import (
	"encoding/json"
	"testing"

	"wspoc-go/internal/protocol"
)

// destinoDeLaRespuesta es la regla que decide a quién se le entrega lo que
// contestó el .NET. Se prueba aparte porque es la única decisión de seguridad
// del puente: obedecer una Sesion ajena entregaría la respuesta de un usuario a
// otro, y eso no lo detecta ninguna prueba de camino feliz.
func TestDestinoDeLaRespuesta(t *testing.T) {
	c := &connection{remote: "127.0.0.1:1234"}

	casos := []struct {
		nombre   string
		responde string
		origen   string
		quiere   string
	}{
		{
			nombre:   "sin Sesion se entrega a quien preguntó",
			responde: "",
			origen:   "12345",
			quiere:   "12345",
		},
		{
			nombre:   "con la misma Sesion se entrega",
			responde: "12345",
			origen:   "12345",
			quiere:   "12345",
		},
		{
			nombre:   "con una Sesion ajena se descarta",
			responde: "99999",
			origen:   "12345",
			quiere:   "",
		},
	}

	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			env := protocol.Envelope{Session: caso.responde, Payload: json.RawMessage(`{}`)}
			if got := c.destinoDeLaRespuesta(env, caso.origen, "p1"); got != caso.quiere {
				t.Errorf("destino = %q, se esperaba %q", got, caso.quiere)
			}
		})
	}
}
