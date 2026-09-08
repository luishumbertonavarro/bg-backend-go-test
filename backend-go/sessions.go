package main

import (
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
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

	closeOnce sync.Once
	done      chan struct{}
	// finished se cierra cuando writePump ha salido de verdad. Sin esperarlo, el
	// cierre escribiría en el socket mientras el escritor sigue dentro de
	// WriteMessage — exactamente la carrera que este diseño evita.
	finished chan struct{}
}

// outBuffer es cuánto se tolera que un cliente vaya por detrás antes de darlo por
// atascado. Con el rate limit en 20 msg/s, 64 frames son ~3 s de margen.
const outBuffer = 64

func NewClient(conn *websocket.Conn, session, connID string) *Client {
	return &Client{
		conn:     conn,
		out:      make(chan []byte, outBuffer),
		session:  session,
		connID:   connID,
		done:     make(chan struct{}),
		finished: make(chan struct{}),
	}
}

// Enqueue encola un frame. Devuelve false si la cola está llena: el cliente no
// drena y hay que cerrarlo en vez de acumular memoria (§7 backpressure).
func (c *Client) Enqueue(payload map[string]any) bool {
	raw, err := json.Marshal(payload)
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

// writePump es la única goroutine que escribe en el socket.
func (c *Client) writePump() {
	defer close(c.finished)
	for {
		select {
		case <-c.done:
			return
		case raw, ok := <-c.out:
			if !ok {
				return
			}
			// Backpressure: si el cliente no drena en 5 s, la escritura falla y se
			// cierra la conexión en vez de acumular memoria (§7).
			if err := c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
				return
			}
		}
	}
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
	case <-time.After(6 * time.Second):
		// El escritor está atascado en una escritura con su propio plazo de 5 s.
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
			if err := c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, raw); err != nil {
				return
			}
		default:
			return
		}
	}
}

// ---------------------------------------------------------- registro de sesiones

// SessionRegistry mantiene el contador global del tope anti-DoS (§7) y, además,
// el mapa de sesión -> conexiones que permite entregar a un usuario concreto.
//
// Una misma sesión puede tener varias conexiones (varias pestañas abiertas): la
// entrega va a todas.
type SessionRegistry struct {
	active int32
	max    int32

	mu       sync.RWMutex
	sessions map[string][]*Client
}

func NewSessionRegistry(max int32) *SessionRegistry {
	return &SessionRegistry{max: max, sessions: make(map[string][]*Client)}
}

func (r *SessionRegistry) Active() int32 { return atomic.LoadInt32(&r.active) }

// TryAdd reserva una plaza solo si queda hueco. Atómico frente a handshakes
// concurrentes: sin esto, dos a la vez podrían superar el máximo.
func (r *SessionRegistry) TryAdd() (int32, bool) {
	for {
		current := atomic.LoadInt32(&r.active)
		if current >= r.max {
			return current, false
		}
		if atomic.CompareAndSwapInt32(&r.active, current, current+1) {
			return current + 1, true
		}
	}
}

// Release devuelve la plaza reservada por TryAdd sin haber llegado a registrar
// un cliente (por ejemplo, si el upgrade falla).
func (r *SessionRegistry) Release() int32 { return atomic.AddInt32(&r.active, -1) }

// Bind asocia un cliente ya aceptado a su sesión.
func (r *SessionRegistry) Bind(c *Client) {
	r.mu.Lock()
	r.sessions[c.session] = append(r.sessions[c.session], c)
	r.mu.Unlock()
}

// Remove desasocia el cliente y libera su plaza. Devuelve las conexiones activas.
func (r *SessionRegistry) Remove(c *Client) int32 {
	r.mu.Lock()
	clients := r.sessions[c.session]
	for i, existing := range clients {
		if existing == c {
			r.sessions[c.session] = append(clients[:i], clients[i+1:]...)
			break
		}
	}
	if len(r.sessions[c.session]) == 0 {
		delete(r.sessions, c.session)
	}
	r.mu.Unlock()

	c.Close()
	return atomic.AddInt32(&r.active, -1)
}

// Deliver encola un frame en todas las conexiones de una sesión. Devuelve a
// cuántas llegó; 0 significa que la sesión no está conectada en este proceso.
func (r *SessionRegistry) Deliver(session string, payload map[string]any) int {
	r.mu.RLock()
	clients := make([]*Client, len(r.sessions[session]))
	copy(clients, r.sessions[session])
	r.mu.RUnlock()

	delivered := 0
	for _, c := range clients {
		if c.Enqueue(payload) {
			delivered++
			continue
		}
		// Cola llena: el cliente no drena. Se cierra en vez de dejarla crecer.
		log.Printf("[REJECT] stack=go reason=BACKPRESSURE code=4009 remote=- origin=- detail=conn=%s sesion=%s",
			c.connID, c.session)
		c.Close()
	}
	return delivered
}
