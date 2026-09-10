package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/gorilla/websocket"

	"wspoc-go/internal/bridge"
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
	// pongWriteTimeout es el plazo para responder al ping de un cliente.
	pongWriteTimeout = 2 * time.Second
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
	c.keepAlive(idle)

	for {
		// Idle timeout: la fecha límite se renueva con los datos del cliente y con
		// el latido de protocolo (ver keepAlive). Lo que cierra ahora es una
		// conexión que no contesta, no uno que simplemente calla.
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

// keepAlive hace que el latido de protocolo renueve el plazo de lectura.
//
// Cambia lo que el idle timeout significa. Antes cerraba toda conexión que no
// enviara datos de aplicación, incluida la sana que solo escucha —que es
// justamente el caso de uso de este servicio: un navegador esperando un valor
// que le llega por push—. Ahora cierra la que no CONTESTA al ping, que es la que
// de verdad está muerta o inalcanzable.
//
// Lo que se pierde: una conexión abierta y silenciosa puede quedarse
// indefinidamente. Lo que la acota ya no es el tiempo sino el cupo,
// WS_MAX_CONNECTIONS.
//
// Los dos sentidos se atienden porque los clientes no son iguales: el navegador
// solo responde pongs a nuestros pings (su JavaScript no puede enviar pings),
// mientras que un cliente nativo —Postman, un k6— sí manda pings propios.
func (c *connection) keepAlive(idle time.Duration) {
	conn := c.client.Conn()
	renovar := func() error { return conn.SetReadDeadline(time.Now().Add(idle)) }

	conn.SetPongHandler(func(string) error { return renovar() })

	// Este handler sustituye al de gorilla, que responde el pong. Hay que seguir
	// respondiéndolo: sin pong, el cliente que nos hace ping nos da por muertos.
	conn.SetPingHandler(func(data string) error {
		_ = renovar()
		err := conn.WriteControl(
			websocket.PongMessage, []byte(data), time.Now().Add(pongWriteTimeout))
		if errors.Is(err, websocket.ErrCloseSent) {
			return nil
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			// Un pong que no cabe ahora no es motivo para tirar la conexión: el
			// idle timeout ya se encarga si el cliente resulta estar muerto.
			return nil
		}
		return err
	})
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
	// se descarta sin haberlo materializado entero en memoria.
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

	switch msg.Type {
	case protocol.TypePing:
		c.client.Enqueue(c.server.framer.Pong(msg.ID, sessionID))

	case protocol.TypePeticion:
		// En una goroutine porque la llamada al .NET puede tardar segundos y este
		// es el bucle de LECTURA de la conexión: bloquearlo dejaría al cliente sin
		// poder mandar ni un ping, y moriría por idle timeout esperando su propia
		// respuesta. El tope de concurrencia lo pone el semáforo del puente, no
		// esta goroutine.
		go c.consultarBackend(msg, sessionID)

	default:
		// El eco devuelve al cliente su propio payload y no sale del proceso.
		// Sirve para comprobar que el canal está vivo sin depender del .NET.
		c.client.Enqueue(c.server.framer.Echo(msg.ID, msg.Payload, sessionID))
	}
}

// consultarBackend pregunta al .NET 4.8 y entrega su respuesta a la sesión.
func (c *connection) consultarBackend(msg protocol.ClientMessage, sessionID string) {
	// El contexto acota la espera aunque el cliente se vaya: la llamada ya está
	// en vuelo y ocupa un hueco del semáforo, así que tiene que terminar sola.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(c.server.cfg.DotNetTimeoutSeconds)*time.Second)
	defer cancel()

	inicio := time.Now()
	respuesta, err := c.server.bridge.Ask(ctx, protocol.Envelope{
		Session: sessionID,
		Payload: msg.Payload,
	})
	if err != nil {
		rejection := rechazoDelPuente(err)
		logging.Reject(rejection, c.remote, "", "peticion id="+msg.ID+" "+err.Error())
		// Se entrega por el registro y no directamente a esta conexión para que el
		// error siga el mismo camino que la respuesta: si el cliente se reconectó
		// mientras esperaba, lo recibe igual.
		c.server.registry.Deliver(sessionID, c.server.framer.ErrorEn(msg.ID, rejection, sessionID))
		return
	}

	destino := c.destinoDeLaRespuesta(respuesta, sessionID, msg.ID)
	if destino == "" {
		// La respuesta se descarta, pero el cliente sigue esperándola: sin avisarle
		// se quedaría colgado hasta su propio timeout, y en los logs no habría nada
		// del lado del navegador que explicara el silencio.
		c.server.registry.Deliver(sessionID,
			c.server.framer.ErrorEn(msg.ID, protocol.BackendError, sessionID))
		return
	}

	logging.Peticion(sessionID, msg.ID, time.Since(inicio).Milliseconds(),
		len(msg.Payload), len(respuesta.Payload))

	frame := c.server.framer.Respuesta(msg.ID, respuesta.Payload, destino)
	if entregados := c.server.registry.Deliver(destino, frame); entregados == 0 {
		// No es un error del .NET: el cliente se fue mientras se le calculaba la
		// respuesta. Se registra porque, si pasa mucho, el .NET tarda de más.
		logging.Reject(protocol.SessionNotFound, c.remote, "", "respuesta huérfana sesion="+destino)
	}
}

// destinoDeLaRespuesta decide a qué sesión va la respuesta del .NET.
//
// La regla, en orden: sin `Sesion` se usa la de origen —Go ya sabe quién
// preguntó, así que el .NET no tiene por qué repetirlo—; con la misma, se
// entrega; con una DISTINTA, se descarta. En un round-trip síncrono no hay razón
// legítima para que difiera, y obedecerla convertiría un fallo del .NET en la
// respuesta de un usuario entregada a otro. Para empujar a una sesión ajena está
// POST /api/push, que es el camino pensado para eso.
//
// Devuelve la cadena vacía cuando hay que descartar.
func (c *connection) destinoDeLaRespuesta(respuesta protocol.Envelope, origen, id string) string {
	if respuesta.Session == "" || respuesta.Session == origen {
		return origen
	}
	logging.RejectRaw("BACKEND_SESSION_MISMATCH", 4015, c.remote, "",
		"peticion id="+id+" origen="+origen+" el .NET respondió sesion="+respuesta.Session)
	return ""
}

// rechazoDelPuente traduce el fallo del puente al motivo que ve el cliente.
func rechazoDelPuente(err error) protocol.Rejection {
	switch {
	case errors.Is(err, bridge.ErrSinDestino):
		return protocol.BackendUnavailable
	case errors.Is(err, bridge.ErrSaturado):
		return protocol.BackendBusy
	case errors.Is(err, bridge.ErrTimeout):
		return protocol.BackendTimeout
	default:
		return protocol.BackendError
	}
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
