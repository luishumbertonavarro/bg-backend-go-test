package httpapi

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/gorilla/websocket"

	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
	"wspoc-go/internal/ratelimit"
	"wspoc-go/internal/session"
)

const (
	// closeWriteTimeout es el plazo para escribir el frame de cierre.
	closeWriteTimeout = 2 * time.Second
	// closeDrainTimeout es cuánto se drena la conexión tras escribir el cierre.
	closeDrainTimeout = 500 * time.Millisecond
	// readLimitFactor multiplica el máximo para el tope duro de gorilla. El
	// límite efectivo es el corte en streaming; este solo cubre un fallo de aquel.
	readLimitFactor = 4
)

// connection es el bucle de una conexión ya aceptada: idle timeout, tamaño,
// rate limit y eco validado.
//
// Es un tipo y no una función con seis parámetros porque el estado que necesita
// (cliente, limitador, dirección remota) vive durante toda la conexión y lo
// comparten todos los pasos del bucle.
type connection struct {
	server  *Server
	client  *session.Client
	limiter *ratelimit.SlidingWindow
	remote  string
}

// newConnection prepara el bucle con su propio limitador de caudal.
//
// El limitador es POR CONEXIÓN: un cliente ruidoso no consume el cupo de los demás.
func (s *Server) newConnection(client *session.Client, remote string) *connection {
	return &connection{
		server:  s,
		client:  client,
		limiter: ratelimit.NewSlidingWindow(s.cfg.RateLimitPerSec),
		remote:  remote,
	}
}

// run atiende la conexión hasta que se cierra.
func (c *connection) run() {
	conn := c.client.Conn()
	idle := time.Duration(c.server.cfg.IdleTimeoutSeconds) * time.Second

	// Tope duro por si el corte en streaming fallase; el límite efectivo es el de abajo.
	conn.SetReadLimit(c.server.cfg.MaxMessageBytes * readLimitFactor)

	for {
		// Idle timeout: la fecha límite NO se renueva con los pong, solo con datos
		// reales (§7). Así una conexión "viva pero muda" también se cierra.
		if err := conn.SetReadDeadline(time.Now().Add(idle)); err != nil {
			return
		}

		raw, ok := c.readFrame()
		if !ok {
			return
		}
		if !c.process(raw) {
			return
		}
	}
}

// readFrame lee el siguiente mensaje de texto. El bool dice si se puede seguir.
func (c *connection) readFrame() ([]byte, bool) {
	conn := c.client.Conn()

	messageType, reader, err := conn.NextReader()
	if err != nil {
		if isTimeout(err) {
			c.reject(protocol.IdleTimeout, "")
			c.closeWith(protocol.IdleTimeout)
		}
		return nil, false
	}

	if messageType != websocket.TextMessage {
		c.reject(protocol.InvalidPayload, "frame=binario")
		c.closeWith(protocol.InvalidPayload)
		return nil, false
	}

	// Corte en streaming: se lee un byte más que el máximo; si llega, el frame
	// se descarta sin haberlo materializado entero en memoria (§5).
	max := c.server.cfg.MaxMessageBytes
	raw, err := io.ReadAll(io.LimitReader(reader, max+1))
	if err != nil {
		return nil, false
	}
	if int64(len(raw)) > max {
		c.reject(protocol.MessageTooLarge, "")
		c.closeWith(protocol.MessageTooLarge)
		return nil, false
	}
	return raw, true
}

// process aplica caudal y validación, y responde. El bool dice si se puede seguir.
func (c *connection) process(raw []byte) bool {
	if !c.limiter.Allow() {
		c.reject(protocol.RateLimitExceeded, fmt.Sprintf("limit=%d/s", c.server.cfg.RateLimitPerSec))
		c.closeWith(protocol.RateLimitExceeded)
		return false
	}

	msg, rejection := c.server.validator.Message(raw)
	if rejection != nil {
		c.reject(*rejection, "")
		// El frame de error se encola ANTES de cerrar: informa del motivo a un
		// cliente que, si no, solo vería un código de cierre.
		c.client.Enqueue(c.server.framer.Error(*rejection, c.client.Session()))
		c.closeWith(*rejection)
		return false
	}

	c.respond(msg)
	return true
}

// respond contesta al mensaje ya validado.
func (c *connection) respond(msg protocol.ClientMessage) {
	sessionID := c.client.Session()

	if msg.Type == protocol.TypePing {
		c.client.Enqueue(c.server.framer.Pong(msg.ID, sessionID))
		return
	}

	c.client.Enqueue(c.server.framer.Echo(msg.ID, msg.Payload, sessionID))
	// El mensaje del cliente viaja al .NET 4.8 en el sobre {sesion, payload}.
	c.server.outbox.Add(protocol.Envelope{Session: sessionID, Payload: msg.Payload})
}

// reject registra el rechazo con el contexto de esta conexión.
func (c *connection) reject(rejection protocol.Rejection, extra string) {
	detail := "conn=" + c.client.ConnID()
	if extra != "" {
		detail += " " + extra
	}
	logging.Reject(rejection, c.remote, "", detail)
}

// closeWith cierra la conexión con el código del protocolo.
//
// El orden de los pasos es lo delicado: se para al escritor y se ESPERA a que
// salga antes de tocar el socket —si no, dos goroutines escribirían a la vez y
// gorilla/websocket no lo admite—, luego se vacía lo que quedara encolado (por
// ejemplo el frame de `error` de un 4010, que si no se perdería), y solo
// entonces se escribe el cierre.
func (c *connection) closeWith(rejection protocol.Rejection) {
	conn := c.client.Conn()

	c.client.StopWriter()
	c.client.FlushPending()

	_ = conn.SetWriteDeadline(time.Now().Add(closeWriteTimeout))
	closeFrame := websocket.FormatCloseMessage(rejection.Code, rejection.Reason)
	if err := conn.WriteMessage(websocket.CloseMessage, closeFrame); err != nil {
		return
	}

	// Cerrar el TCP inmediatamente después de escribir el frame descarta lo que aún
	// no ha salido: el cliente vería un 1006 en vez del código real. Se drena la
	// conexión brevemente para completar el cierre ordenado.
	_ = conn.SetReadDeadline(time.Now().Add(closeDrainTimeout))
	for {
		if _, _, err := conn.NextReader(); err != nil {
			return
		}
	}
}

func isTimeout(err error) bool {
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}
