// Package ratelimit implementa el control de caudal: un tope de eventos por segundo.
package ratelimit

import (
	"sync"
	"time"
)

// SlidingWindow permite como mucho `max` eventos en el último segundo.
//
// Es seguro para uso concurrente. La versión anterior no llevaba mutex —
// correcto mientras solo lo usara la goroutine de lectura de una conexión — y el
// endpoint de push, que sí es compartido, tenía que acordarse de envolver cada
// llamada en un mutex propio. Ese acuerdo no lo verifica nadie: basta con que un
// punto de uso nuevo lo olvide para corromper el slice de marcas de tiempo.
// El coste de un mutex sin contienda es despreciable frente a esa clase de fallo.
type SlidingWindow struct {
	mu     sync.Mutex
	max    int
	stamps []time.Time
}

// NewSlidingWindow crea un limitador de maxPerSecond eventos por segundo.
func NewSlidingWindow(maxPerSecond int) *SlidingWindow {
	return &SlidingWindow{max: maxPerSecond, stamps: make([]time.Time, 0, maxPerSecond)}
}

// Allow registra un evento y dice si cabe dentro del límite.
func (l *SlidingWindow) Allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Second)

	// Se compacta sobre el mismo array para no reservar memoria en cada llamada.
	kept := l.stamps[:0]
	for _, t := range l.stamps {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.stamps = kept

	if len(l.stamps) >= l.max {
		return false
	}
	l.stamps = append(l.stamps, now)
	return true
}
