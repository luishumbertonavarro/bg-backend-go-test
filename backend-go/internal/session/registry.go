package session

import (
	"sync"
	"sync/atomic"

	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
)

// Registry mantiene el contador global del tope anti-DoS (§7) y, además, el mapa
// de sesión -> conexiones que permite entregar a un usuario concreto.
//
// Una misma sesión puede tener varias conexiones (varias pestañas abiertas): la
// entrega va a todas.
type Registry struct {
	active int32
	max    int32

	mu       sync.RWMutex
	sessions map[string][]*Client
}

// NewRegistry crea el registro con el tope de conexiones simultáneas.
func NewRegistry(max int32) *Registry {
	return &Registry{max: max, sessions: make(map[string][]*Client)}
}

// Active son las conexiones abiertas ahora mismo.
func (r *Registry) Active() int32 { return atomic.LoadInt32(&r.active) }

// Max es el tope de conexiones simultáneas.
func (r *Registry) Max() int32 { return r.max }

// AtCapacity indica si ya no queda hueco. Es una comprobación optimista para
// rechazar pronto; la reserva real y atómica la hace TryAdd.
func (r *Registry) AtCapacity() bool { return r.Active() >= r.max }

// TryAdd reserva una plaza solo si queda hueco. Atómico frente a handshakes
// concurrentes: sin esto, dos a la vez podrían superar el máximo.
func (r *Registry) TryAdd() (int32, bool) {
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
func (r *Registry) Release() int32 { return atomic.AddInt32(&r.active, -1) }

// Bind asocia un cliente ya aceptado a su sesión.
func (r *Registry) Bind(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[c.session] = append(r.sessions[c.session], c)
}

// Remove desasocia el cliente y libera su plaza. Devuelve las conexiones activas.
func (r *Registry) Remove(c *Client) int32 {
	r.unbind(c)
	c.Close()
	return atomic.AddInt32(&r.active, -1)
}

func (r *Registry) unbind(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()

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
}

// Deliver encola un frame en todas las conexiones de una sesión. Devuelve a
// cuántas llegó; 0 significa que la sesión no está conectada en este proceso.
func (r *Registry) Deliver(session string, frame protocol.Frame) int {
	delivered := 0
	for _, c := range r.clientsOf(session) {
		if c.Enqueue(frame) {
			delivered++
			continue
		}
		// Cola llena: el cliente no drena. Se cierra en vez de dejarla crecer.
		logging.Reject(protocol.Backpressure, "", "", "conn="+c.connID+" sesion="+c.session)
		c.Close()
	}
	return delivered
}

// clientsOf devuelve una copia de las conexiones de una sesión.
//
// La copia es deliberada: entregar mantiene el lock solo el tiempo de copiar la
// lista, no el de encolar en cada cliente. Encolar bajo el lock bloquearía todo
// el registro mientras un cliente lento drena.
func (r *Registry) clientsOf(session string) []*Client {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clients := make([]*Client, len(r.sessions[session]))
	copy(clients, r.sessions[session])
	return clients
}
