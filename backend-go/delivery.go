package main

import (
	"errors"
	"log"
	"net/http"
	"time"
)

var errTooLarge = errors.New("cuerpo demasiado grande")

// dotNetClient es el cliente HTTP hacia el .NET 4.8. Con timeout siempre: un
// backend lento no debe poder acumular goroutines aquí.
var dotNetClient *http.Client

// Deliverer entrega un frame a todas las conexiones de una sesión y devuelve a
// cuántas llegó. La abstracción existe porque con varias réplicas la conexión
// puede vivir en OTRO proceso: LocalDeliverer solo alcanza las de este, y
// RedisDeliverer las de todos.
type Deliverer interface {
	Deliver(session string, payload map[string]any) int
}

var deliverer Deliverer

// LocalDeliverer entrega contra el registro en memoria de este proceso.
// Es correcto con UNA réplica; con varias, un push puede caer en el pod que no
// tiene la sesión y se perdería (por eso existe RedisDeliverer).
type LocalDeliverer struct{ registry *SessionRegistry }

func (d LocalDeliverer) Deliver(session string, payload map[string]any) int {
	return d.registry.Deliver(session, payload)
}

// SetupDelivery elige la estrategia de entrega según la configuración.
func SetupDelivery() {
	dotNetClient = &http.Client{Timeout: time.Duration(cfg.DotNetTimeoutSeconds) * time.Second}

	if cfg.RedisAddr == "" {
		deliverer = LocalDeliverer{registry: registry}
		log.Printf("Entrega: local (una réplica). Define WS_REDIS_ADDR para enrutar entre réplicas.")
		return
	}
	deliverer = NewRedisDeliverer(cfg.RedisAddr, registry)
	log.Printf("Entrega: Redis en %s (enrutado entre réplicas activo).", cfg.RedisAddr)
}
