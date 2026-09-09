// Package session gestiona las conexiones vivas: cada cliente y el registro que
// las agrupa por sesión.
package session

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"wspoc-go/internal/protocol"
)

// Plazos de escritura.
const (
	// writeTimeout es el backpressure: si el cliente no drena en este tiempo, la
	// escritura falla y se cierra la conexión en vez de acumular memoria.
	writeTimeout = 5 * time.Second
	// flushTimeout es más corto porque solo se usa al cerrar, cuando ya no
	// interesa esperar a un cliente que probablemente se ha ido.
	flushTimeout = 2 * time.Second
	// writerExitTimeout es cuánto se espera a que el escritor salga. Supera a
	// writeTimeout para darle margen a terminar su escritura en curso.
	writerExitTimeout = 6 * time.Second
	// outBuffer es cuánto se tolera que un cliente vaya por detrás antes de darlo
	// por atascado. Con el rate limit en 20 msg/s, 64 frames son ~3 s de margen.
	outBuffer = 64
)

// Client es una conexión viva. Toda la escritura al socket pasa por `out` y la
// consume UNA sola goroutine (writePump).
//
// Esto no es un lujo: gorilla/websocket NO admite escrituras concurrentes. Antes
// solo escribía la goroutine de lectura, así que bastaba con escribir directo;
// desde que el REST puede empujar a la vez, dos escrituras simultáneas
// entrelazarían los frames y corromperían el canal.
type Client struct {
	conn    *websocket.Conn
	out     chan []byte
	session string
	connID  string
	// pingInterval es cada cuánto writePump manda un ping de protocolo.
	pingInterval time.Duration

	closeOnce sync.Once
	done      chan struct{}
	// finished se cierra cuando writePump ha salido de verdad. Sin esperarlo, el
	// cierre escribiría en el socket mientras el escritor sigue dentro de
	// WriteMessage — exactamente la carrera que este diseño evita.
	finished chan struct{}
}

// NewClient crea el cliente y arranca su goroutine escritora.
//
// Arrancarla aquí, y no en el llamante, hace imposible el estado intermedio en
// el que existe un Client cuya cola nadie está drenando.
func NewClient(conn *websocket.Conn, session, connID string, pingInterval time.Duration) *Client {
	// Un intervalo no positivo haría entrar en pánico al ticker de writePump.
	if pingInterval <= 0 {
		pingInterval = time.Second
	}
	c := &Client{
		conn:         conn,
		out:          make(chan []byte, outBuffer),
		session:      session,
		connID:       connID,
		pingInterval: pingInterval,
		done:         make(chan struct{}),
		finished:     make(chan struct{}),
	}
	go c.writePump()
	return c
}

// Session es la sesión a la que pertenece la conexión.
func (c *Client) Session() string { return c.session }

// ConnID identifica la conexión en los logs.
func (c *Client) ConnID() string { return c.connID }

// Conn expone el socket para las operaciones que solo la capa de transporte
// puede hacer: fijar plazos de lectura y leer frames.
func (c *Client) Conn() *websocket.Conn { return c.conn }

// Enqueue encola un frame. Devuelve false si la cola está llena: el cliente no
// drena y hay que cerrarlo en vez de acumular memoria (backpressure).
func (c *Client) Enqueue(frame protocol.Frame) bool {
	raw, err := json.Marshal(frame)
	if err != nil {
		return false
	}
	select {
	case <-c.done:
		return false
	case c.out <- raw:
		return true
	default:
		return false
	}
}

// writePump es la única goroutine que escribe en el socket, y también la que
// lleva el latido.
func (c *Client) writePump() {
	defer close(c.finished)

	ping := time.NewTicker(c.pingInterval)
	defer ping.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ping.C:
			if !c.ping() {
				return
			}
		case raw, ok := <-c.out:
			if !ok {
				return
			}
			if !c.write(raw, writeTimeout) {
				return
			}
		}
	}
}

// ping manda un PING de protocolo. Devuelve false si hay que abandonar.
//
// Lo manda el SERVIDOR, no el cliente, y esa dirección es la que importa: el
// JavaScript de un navegador no puede enviar pings —la API de WebSocket no lo
// expone—, pero sí responde el pong automáticamente. Así el latido funciona sin
// una línea de código en el frontend.
//
// Va aquí y no en la goroutine de lectura para no romper la disciplina de un
// único escritor, aunque WriteControl sea seguro en concurrencia: tenerlo en un
// solo sitio es lo que hace la regla comprobable de un vistazo.
func (c *Client) ping() bool {
	return c.conn.WriteControl(
		websocket.PingMessage, nil, time.Now().Add(writeTimeout)) == nil
}

// write hace una escritura con plazo. Devuelve false si hay que abandonar.
func (c *Client) write(raw []byte, timeout time.Duration) bool {
	if err := c.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return false
	}
	return c.conn.WriteMessage(websocket.TextMessage, raw) == nil
}

// Close libera el writePump. Es idempotente.
func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// StopWriter para el escritor y espera a que salga. Al volver, el socket es de
// quien llama en exclusiva: ya nadie más va a escribir en él.
func (c *Client) StopWriter() {
	c.Close()
	select {
	case <-c.finished:
	case <-time.After(writerExitTimeout):
		// El escritor está atascado en una escritura con su propio plazo.
		// No se espera indefinidamente: cerrar el socket lo desbloqueará.
	}
}

// FlushPending escribe lo que quedó en la cola. Solo es seguro tras StopWriter:
// por ejemplo el frame de `error` que precede a un cierre 4010, que de otro modo
// se perdería al parar el escritor.
func (c *Client) FlushPending() {
	for {
		select {
		case raw := <-c.out:
			if !c.write(raw, flushTimeout) {
				return
			}
		default:
			return
		}
	}
}
