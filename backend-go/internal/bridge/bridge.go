// Package bridge es la llamada síncrona al backend .NET 4.8.
//
// Es el reverso del outbox: aquel manda y se olvida, este pregunta y espera. La
// diferencia importa porque el .NET 4.8 no puede llamar de vuelta a Go —no es
// código nuestro y no se toca—, así que la única forma de que su respuesta llegue
// al navegador es que Go la recoja del mismo HTTP en el que preguntó.
//
// No conoce WebSockets ni sesiones: recibe un sobre y devuelve otro. Quién lo
// entrega y a quién es cosa de la capa de transporte.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"wspoc-go/internal/config"
	"wspoc-go/internal/protocol"
)

// Errores del puente. Se distinguen porque el cliente recibe un motivo distinto
// para cada uno y no es lo mismo "no hay backend configurado" que "el backend
// tardó demasiado": lo primero es un despliegue a medias, lo segundo un problema
// de carga.
var (
	// ErrSinDestino indica que no hay URL del .NET configurada.
	ErrSinDestino = errors.New("no hay URL del .NET configurada")
	// ErrSaturado indica que ya hay demasiadas llamadas en vuelo.
	ErrSaturado = errors.New("demasiadas peticiones simultáneas hacia el .NET")
	// ErrTimeout indica que el .NET no respondió a tiempo.
	ErrTimeout = errors.New("el .NET no respondió a tiempo")
)

// Bridge hace las llamadas al .NET 4.8 con un tope de concurrencia.
type Bridge struct {
	url    string
	client *http.Client
	// inflight es un semáforo con capacidad: cada llamada ocupa un hueco y lo
	// libera al terminar. Un canal con buffer y no un contador porque así el
	// intento de ocupar hueco es atómico y no bloqueante en un `select`.
	inflight chan struct{}
	// maxBytes acota la respuesta del .NET. Se reutiliza el límite del canal: lo
	// que vuelve acaba en un frame WebSocket, así que no puede ser mayor que uno.
	maxBytes int64
	// validator aplica a la respuesta del .NET el mismo saneado que al resto. El
	// puente valida lo que entra por él, en lugar de confiar en que el otro
	// extremo mande algo apto para el navegador.
	validator protocol.Validator
}

// defaultMaxInflight se usa si la configuración da un valor absurdo. Un cero
// dejaría un semáforo sin huecos y ninguna petición saldría nunca.
const defaultMaxInflight = 32

// New construye el puente. Una URL vacía da un puente que rechaza todo con
// ErrSinDestino, que es el modo en el que arranca el POC mientras no exista la
// ruta del .NET.
func New(cfg config.Config) *Bridge {
	max := cfg.DotNetMaxInflight
	if max <= 0 {
		max = defaultMaxInflight
	}
	return &Bridge{
		url: cfg.DotNetAPIURL,
		// Timeout SIEMPRE: un .NET colgado no puede quedarse con una goroutine y
		// un hueco del semáforo para siempre.
		client:    &http.Client{Timeout: time.Duration(cfg.DotNetTimeoutSeconds) * time.Second},
		inflight:  make(chan struct{}, max),
		maxBytes:  cfg.MaxMessageBytes,
		validator: protocol.NewValidator(cfg.MaxMessageBytes),
	}
}

// Configurado indica si hay un .NET al que preguntar.
func (b *Bridge) Configurado() bool { return b.url != "" }

// URL es el destino configurado, para poder mostrarlo en el diagnóstico. Cadena
// vacía = no hay .NET al que preguntar.
func (b *Bridge) URL() string { return b.url }

// Ask envía el sobre al .NET y devuelve lo que conteste.
//
// El sobre de vuelta puede traer la `Sesion` vacía: quien llama decide qué hacer
// con eso. Aquí no se interpreta, solo se transporta.
func (b *Bridge) Ask(ctx context.Context, env protocol.Envelope) (protocol.Envelope, error) {
	if !b.Configurado() {
		return protocol.Envelope{}, ErrSinDestino
	}

	// Ocupar hueco sin bloquear: si no hay, se rechaza en el acto. Encolar sería
	// peor que rechazar —el cliente esperaría una respuesta que llega tarde o no
	// llega— y además dejaría crecer la memoria sin tope.
	select {
	case b.inflight <- struct{}{}:
		defer func() { <-b.inflight }()
	default:
		return protocol.Envelope{}, ErrSaturado
	}

	raw, err := json.Marshal(env)
	if err != nil {
		return protocol.Envelope{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(raw))
	if err != nil {
		return protocol.Envelope{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := b.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
			return protocol.Envelope{}, ErrTimeout
		}
		return protocol.Envelope{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusMultipleChoices {
		return protocol.Envelope{}, fmt.Errorf("el .NET respondió %d", resp.StatusCode)
	}

	// Se corta en streaming, igual que el canal con los frames grandes: leer
	// entero para luego medir permitiría que un .NET averiado tumbara el proceso.
	cuerpo, err := io.ReadAll(io.LimitReader(resp.Body, b.maxBytes+1))
	if err != nil {
		return protocol.Envelope{}, err
	}
	if int64(len(cuerpo)) > b.maxBytes {
		return protocol.Envelope{}, fmt.Errorf("la respuesta del .NET supera %d bytes", b.maxBytes)
	}

	respuesta, rejection := b.validator.RespuestaBackend(cuerpo)
	if rejection != nil {
		return protocol.Envelope{}, fmt.Errorf("respuesta del .NET no válida: %s", rejection.Reason)
	}
	return respuesta, nil
}
