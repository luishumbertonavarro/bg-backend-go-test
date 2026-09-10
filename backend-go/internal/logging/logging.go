// Package logging concentra el formato de los eventos auditables del servicio.
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

// Reject registra un rechazo. Es el evento auditable central: todo camino que
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

// Token registra la emisión de un token del intercambio SESION -> JWT.
//
// Es un evento auditable de pleno derecho: es el punto donde una SESION pasa a
// tener acceso al canal, así que cada emisión tiene que quedar rastreada igual
// que quedan los rechazos.
func Token(session, remote, origin string, ttlSeconds int) {
	log.Printf("[TOKEN] stack=%s sesion=%s remote=%s origin=%s ttl=%ds",
		stack, session, orDash(remote), orDash(origin), ttlSeconds)
}

// Peticion registra un viaje completo al .NET 4.8: lo que subio por el canal, lo
// que contesto el backend y cuanto tardo.
//
// Se registra el camino FELIZ, no solo el fallo. Sin esta linea, una peticion que
// funciona no deja rastro en ningun sitio: el unico modo de saber si el .NET
// llego a procesarla era mirar los logs del propio .NET, que no siempre se
// tienen delante. El tiempo va aqui porque es lo que decide si
// WS_DOTNET_TIMEOUT_SECONDS esta bien puesto.
func Peticion(session, id string, ms int64, bytesEnviados, bytesRecibidos int) {
	log.Printf("[PETICION] stack=%s sesion=%s id=%s ms=%d envio=%dB recibio=%dB",
		stack, session, id, ms, bytesEnviados, bytesRecibidos)
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
