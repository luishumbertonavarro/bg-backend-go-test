// Package logging concentra el formato de los eventos del POC (§8).
//
// Antes, cada punto de rechazo montaba a mano una cadena
// "[REJECT] stack=go reason=%s code=%d remote=%s origin=%s detail=%s". Estaba
// repetida quince veces y bastaba con que una omitiera un campo para que el
// parseo de los logs dejara de funcionar sin que nadie se diera cuenta.
//
// El formato de salida es exactamente el mismo que antes: es un contrato con
// quien lee los logs, no un detalle interno.
package logging

import (
	"fmt"
	"log"

	"wspoc-go/internal/protocol"
)

// stack identifica este backend entre los cuatro del POC en cada línea.
const stack = "go"

// sinValor es lo que se escribe en un campo que no aplica al evento.
const sinValor = "-"

// Setup fija el formato de marca de tiempo, con microsegundos para poder ordenar
// eventos de conexiones concurrentes.
func Setup() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

// Reject registra un rechazo. Es el evento auditable del §8: todo camino que
// niegue el servicio pasa por aquí.
func Reject(rejection protocol.Rejection, remote, origin, detail string) {
	log.Printf("[REJECT] stack=%s reason=%s code=%d remote=%s origin=%s detail=%s",
		stack, rejection.Reason, rejection.Code, orDash(remote), orDash(origin), orDash(detail))
}

// RejectRaw registra un rechazo que no está en el catálogo del protocolo, como
// los fallos de infraestructura (Redis, webhook del .NET) que no tienen código
// de cierre WebSocket propio.
func RejectRaw(reason string, code int, remote, origin, detail string) {
	log.Printf("[REJECT] stack=%s reason=%s code=%d remote=%s origin=%s detail=%s",
		stack, reason, code, orDash(remote), orDash(origin), orDash(detail))
}

// Accept registra una conexión aceptada tras superar todos los controles.
func Accept(connID, remote, subject, session string, active int32) {
	log.Printf("[ACCEPT] stack=%s conn=%s remote=%s sub=%s sesion=%s active=%d",
		stack, connID, remote, subject, session, active)
}

// Close registra el cierre de una conexión y las que quedan activas.
func Close(connID string, active int32) {
	log.Printf("[CLOSE] stack=%s conn=%s active=%d", stack, connID, active)
}

// Push registra una entrega del .NET 4.8 hacia las conexiones de una sesión.
func Push(session string, conns, payloadBytes int) {
	log.Printf("[PUSH] stack=%s sesion=%s conns=%d bytes=%d", stack, session, conns, payloadBytes)
}

// PushFromRedis registra una entrega que llegó publicada por otra réplica.
func PushFromRedis(session string, conns int) {
	log.Printf("[PUSH] stack=%s sesion=%s conns=%d origen=redis", stack, session, conns)
}

// Outbox registra el sobre que se le enviaría (o se le envió) al .NET 4.8.
func Outbox(raw []byte) {
	log.Printf("[OUTBOX] stack=%s %s", stack, raw)
}

// Infof registra un evento operativo sin formato de auditoría: arranque,
// estrategia de entrega elegida y similares.
func Infof(format string, args ...any) {
	log.Printf(format, args...)
}

// Fatalf registra y aborta. Solo debe usarse desde el arranque.
func Fatalf(format string, args ...any) {
	log.Fatalf(format, args...)
}

func orDash(value string) string {
	if value == "" {
		return sinValor
	}
	return value
}

// Detail formatea un valor arbitrario —típicamente el de un recover()— como
// campo detail, para que quien lo registra no tenga que importar fmt.
func Detail(v any) string { return fmt.Sprint(v) }
