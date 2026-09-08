// Package delivery decide cómo llega un frame a la sesión destino.
//
// La abstracción existe porque con varias réplicas la conexión puede vivir en
// OTRO proceso: Local solo alcanza las de este, y Redis las de todas.
package delivery

import (
	"wspoc-go/internal/config"
	"wspoc-go/internal/logging"
	"wspoc-go/internal/protocol"
	"wspoc-go/internal/session"
)

// Deliverer entrega un frame a todas las conexiones de una sesión y devuelve a
// cuántas llegó.
type Deliverer interface {
	Deliver(session string, frame protocol.Frame) int
}

// Local entrega contra el registro en memoria de este proceso.
//
// Es correcto con UNA réplica; con varias, un push puede caer en el pod que no
// tiene la sesión y se perdería (por eso existe Redis).
type Local struct{ registry *session.Registry }

// NewLocal crea la estrategia de entrega de una sola réplica.
func NewLocal(registry *session.Registry) Local { return Local{registry: registry} }

// Deliver entrega a las conexiones locales.
func (d Local) Deliver(sessionID string, frame protocol.Frame) int {
	return d.registry.Deliver(sessionID, frame)
}

// New elige la estrategia de entrega según la configuración.
//
// Es el único punto del programa que decide entre local y Redis; el resto del
// código habla siempre con la interfaz y no sabe cuál le tocó.
func New(cfg config.Config, registry *session.Registry) Deliverer {
	if cfg.RedisAddr == "" {
		logging.Infof("Entrega: local (una réplica). Define WS_REDIS_ADDR para enrutar entre réplicas.")
		return NewLocal(registry)
	}
	logging.Infof("Entrega: Redis en %s (enrutado entre réplicas activo).", cfg.RedisAddr)
	return NewRedis(cfg.RedisAddr, registry)
}
