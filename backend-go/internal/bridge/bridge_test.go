package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wspoc-go/internal/config"
	"wspoc-go/internal/protocol"
)

// cfg construye una configuración mínima para el puente. Los tests que necesiten
// otra cosa la ajustan sobre lo que devuelve.
func cfg(url string) config.Config {
	return config.Config{
		DotNetAPIURL:         url,
		DotNetTimeoutSeconds: 1,
		DotNetMaxInflight:    2,
		MaxMessageBytes:      65536,
	}
}

func sobre() protocol.Envelope {
	return protocol.Envelope{Session: "12345", Payload: json.RawMessage(`{"a":1}`)}
}

func TestSinDestinoNoLlama(t *testing.T) {
	b := New(cfg(""))

	if b.Configurado() {
		t.Fatal("un puente sin URL no debería darse por configurado")
	}
	if _, err := b.Ask(context.Background(), sobre()); !errors.Is(err, ErrSinDestino) {
		t.Fatalf("se esperaba ErrSinDestino, llegó %v", err)
	}
}

func TestDevuelveLaRespuestaDelBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Sesion":"12345","Payload":{"resultado":7}}`))
	}))
	defer srv.Close()

	got, err := New(cfg(srv.URL)).Ask(context.Background(), sobre())
	if err != nil {
		t.Fatalf("no debería fallar: %v", err)
	}
	if got.Session != "12345" {
		t.Errorf("Sesion = %q, se esperaba 12345", got.Session)
	}
	if string(got.Payload) != `{"resultado":7}` {
		t.Errorf("Payload = %s", got.Payload)
	}
}

// La Sesion vacía es un caso deliberado del contrato: quien llama la interpreta
// como "entrégalo a quien preguntó". El puente solo tiene que transportarla sin
// convertirla en un error.
func TestSesionVaciaSeTransporta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Sesion":"","Payload":{"ok":true}}`))
	}))
	defer srv.Close()

	got, err := New(cfg(srv.URL)).Ask(context.Background(), sobre())
	if err != nil {
		t.Fatalf("una Sesion vacía no es un error: %v", err)
	}
	if got.Session != "" {
		t.Errorf("Sesion = %q, se esperaba vacía", got.Session)
	}
}

func TestTimeoutSeDistingue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // el timeout configurado es 1s
	}))
	defer srv.Close()

	_, err := New(cfg(srv.URL)).Ask(context.Background(), sobre())
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("se esperaba ErrTimeout, llegó %v", err)
	}
}

func TestErrorHTTPNoSeConfundeConRespuesta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"Sesion":"12345","Payload":{"a":1}}`))
	}))
	defer srv.Close()

	if _, err := New(cfg(srv.URL)).Ask(context.Background(), sobre()); err == nil {
		t.Fatal("un 500 con cuerpo válido sigue siendo un fallo")
	}
}

// Lo que responde el .NET acaba en el DOM igual que un push, así que no puede
// entrar por una puerta más laxa que el resto del servicio.
func TestRespuestaConScriptSeRechaza(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Sesion":"12345","Payload":{"x":"<script>alert(1)</script>"}}`))
	}))
	defer srv.Close()

	if _, err := New(cfg(srv.URL)).Ask(context.Background(), sobre()); err == nil {
		t.Fatal("un payload con <script> tenía que rechazarse")
	}
}

func TestRespuestaGiganteSeCorta(t *testing.T) {
	c := cfg("")
	c.MaxMessageBytes = 256
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"Sesion":"12345","Payload":{"x":"` + strings.Repeat("a", 1000) + `"}}`))
	}))
	defer srv.Close()
	c.DotNetAPIURL = srv.URL

	if _, err := New(c).Ask(context.Background(), sobre()); err == nil {
		t.Fatal("una respuesta por encima del límite tenía que rechazarse")
	}
}

// El tope de concurrencia es lo que separa un .NET 4.8 lento de un .NET 4.8
// caído: sin él, cada conexión podría abrir su propia llamada simultánea.
func TestToleraElTopeDeConcurrencia(t *testing.T) {
	libera := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-libera
		w.Write([]byte(`{"Sesion":"","Payload":{"ok":true}}`))
	}))
	defer srv.Close()

	b := New(cfg(srv.URL)) // DotNetMaxInflight = 2

	// Se ocupan los dos huecos y se dejan ocupados.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.Ask(context.Background(), sobre())
		}()
	}
	// Espera activa acotada a que ambas llamadas hayan entrado al semáforo.
	for i := 0; i < 100 && len(b.inflight) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}

	// La tercera no espera turno: se rechaza en el acto.
	if _, err := b.Ask(context.Background(), sobre()); !errors.Is(err, ErrSaturado) {
		t.Fatalf("se esperaba ErrSaturado, llegó %v", err)
	}

	close(libera)
	wg.Wait()

	// Y al liberarse los huecos, se vuelve a admitir.
	if _, err := b.Ask(context.Background(), sobre()); err != nil {
		t.Fatalf("con huecos libres debería pasar: %v", err)
	}
}
