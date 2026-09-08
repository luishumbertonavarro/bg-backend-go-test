package delivery

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
	"wspoc-go/internal/session"
)

const (
	// pushChannel es el canal pub/sub por el que viajan los push entre réplicas.
	pushChannel = "wspoc:push"
	// publishTimeout acota la publicación: un Redis lento no debe bloquear la
	// petición HTTP del .NET 4.8.
	publishTimeout = 2 * time.Second
	// reconnectDelay es la espera entre reintentos de suscripción.
	reconnectDelay = 2 * time.Second
)

// Redis resuelve el problema de las varias réplicas.
//
// La conexión WebSocket vive en la memoria de UN proceso. Con 2 pods, un POST al
// Service puede caer en el que no tiene la sesión y el mensaje se perdería. Aquí
// el push se publica en Redis, todas las réplicas están suscritas, y entrega la
// que tenga la conexión.
type Redis struct {
	client   *redis.Client
	registry *session.Registry
	addr     string
}

// envelope es lo que viaja por el canal: la sesión destino y el frame ya armado.
type envelope struct {
	Session string         `json:"sesion"`
	Frame   protocol.Frame `json:"frame"`
}

// NewRedis crea la estrategia distribuida y arranca la suscripción.
func NewRedis(addr string, registry *session.Registry) *Redis {
	d := &Redis{
		client:   redis.NewClient(&redis.Options{Addr: addr}),
		registry: registry,
		addr:     addr,
	}
	go d.subscribe()
	return d
}

// subscribe escucha los push que publican las demás réplicas (y esta misma) y
// los entrega a las conexiones locales.
func (d *Redis) subscribe() {
	ctx := context.Background()
	for {
		sub := d.client.Subscribe(ctx, pushChannel)
		for msg := range sub.Channel() {
			d.handleIncoming(msg.Payload)
		}
		// El canal se cierra si se pierde la conexión con Redis: se reintenta.
		_ = sub.Close()
		logging.RejectRaw("REDIS_DISCONNECTED", 502, d.addr, "", "reintentando")
		time.Sleep(reconnectDelay)
	}
}

func (d *Redis) handleIncoming(raw string) {
	var incoming envelope
	if err := json.Unmarshal([]byte(raw), &incoming); err != nil {
		logging.RejectRaw("REDIS_INVALID_MESSAGE", 500, "redis", "", err.Error())
		return
	}
	if n := d.registry.Deliver(incoming.Session, incoming.Frame); n > 0 {
		logging.PushFromRedis(incoming.Session, n)
	}
}

// Deliver publica el push para todas las réplicas.
//
// Devuelve las conexiones alcanzadas EN ESTE proceso más las que haya en otros,
// que no se pueden contar desde aquí: por eso, si la publicación funciona, se
// responde 1 aunque la sesión esté en otro pod. Contar de verdad exigiría una
// confirmación de vuelta por Redis, que para el POC no compensa.
func (d *Redis) Deliver(sessionID string, frame protocol.Frame) int {
	// ¿Está la sesión aquí? Si sí, se entrega directo y nos ahorramos el viaje.
	if n := d.registry.Deliver(sessionID, frame); n > 0 {
		return n
	}
	// Si no, puede estar en otra réplica.
	return d.publish(sessionID, frame)
}

// publish manda el frame al resto de réplicas. Devuelve 1 si alguna escuchaba.
func (d *Redis) publish(sessionID string, frame protocol.Frame) int {
	raw, err := json.Marshal(envelope{Session: sessionID, Frame: frame})
	if err != nil {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()

	receivers, err := d.client.Publish(ctx, pushChannel, raw).Result()
	if err != nil {
		logging.RejectRaw("REDIS_UNREACHABLE", 502, d.addr, "", err.Error())
		return 0
	}
	// Esta misma réplica también está suscrita y ya sabemos que no tiene la
	// sesión, así que hace falta al menos otra escuchando.
	if receivers <= 1 {
		return 0
	}
	return 1
}
