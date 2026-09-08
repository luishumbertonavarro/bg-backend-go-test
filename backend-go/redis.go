package main

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisChannel es el canal pub/sub por el que viajan los push entre réplicas.
const redisChannel = "wspoc:push"

// RedisDeliverer resuelve el problema de las varias réplicas.
//
// La conexión WebSocket vive en la memoria de UN proceso. Con 2 pods, un POST al
// Service puede caer en el que no tiene la sesión y el mensaje se perdería. Aquí
// el push se publica en Redis, todas las réplicas están suscritas, y entrega la
// que tenga la conexión.
type RedisDeliverer struct {
	client   *redis.Client
	registry *SessionRegistry
}

// redisPush es lo que viaja por el canal: la sesión destino y el frame ya armado.
type redisPush struct {
	Session string         `json:"sesion"`
	Frame   map[string]any `json:"frame"`
}

func NewRedisDeliverer(addr string, registry *SessionRegistry) *RedisDeliverer {
	d := &RedisDeliverer{
		client:   redis.NewClient(&redis.Options{Addr: addr}),
		registry: registry,
	}
	go d.subscribe()
	return d
}

// subscribe escucha los push que publican las demás réplicas (y esta misma) y
// los entrega a las conexiones locales.
func (d *RedisDeliverer) subscribe() {
	ctx := context.Background()
	for {
		sub := d.client.Subscribe(ctx, redisChannel)
		for msg := range sub.Channel() {
			var push redisPush
			if err := json.Unmarshal([]byte(msg.Payload), &push); err != nil {
				log.Printf("[REJECT] stack=go reason=REDIS_INVALID_MESSAGE code=500 remote=redis origin=- detail=%v", err)
				continue
			}
			if n := d.registry.Deliver(push.Session, push.Frame); n > 0 {
				log.Printf("[PUSH] stack=go sesion=%s conns=%d origen=redis", push.Session, n)
			}
		}
		// El canal se cierra si se pierde la conexión con Redis: se reintenta.
		_ = sub.Close()
		log.Printf("[REJECT] stack=go reason=REDIS_DISCONNECTED code=502 remote=%s origin=- detail=reintentando", cfg.RedisAddr)
		time.Sleep(2 * time.Second)
	}
}

// Deliver publica el push para todas las réplicas.
//
// Devuelve las conexiones alcanzadas EN ESTE proceso más las que haya en otros,
// que no se pueden contar desde aquí: por eso, si la publicación funciona, se
// responde 1 aunque la sesión esté en otro pod. Contar de verdad exigiría una
// confirmación de vuelta por Redis, que para el POC no compensa.
func (d *RedisDeliverer) Deliver(session string, payload map[string]any) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// ¿Está la sesión aquí? Si sí, se entrega directo y nos ahorramos el viaje.
	if n := d.registry.Deliver(session, payload); n > 0 {
		return n
	}

	// Si no, puede estar en otra réplica: se publica y se comprueba que alguien
	// escuchaba. Redis devuelve a cuántos suscriptores llegó.
	raw, err := json.Marshal(redisPush{Session: session, Frame: payload})
	if err != nil {
		return 0
	}
	receivers, err := d.client.Publish(ctx, redisChannel, raw).Result()
	if err != nil {
		log.Printf("[REJECT] stack=go reason=REDIS_UNREACHABLE code=502 remote=%s origin=- detail=%v", cfg.RedisAddr, err)
		return 0
	}
	// Esta misma réplica también está suscrita y ya sabemos que no tiene la
	// sesión, así que hace falta al menos otra escuchando.
	if receivers <= 1 {
		return 0
	}
	return 1
}
