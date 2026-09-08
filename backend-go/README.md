# Backend Go — net/http + gorilla/websocket

- **Puerto:** `8084` (configurable con `WS_PORT` o `WS_PORT_GO` en el `.env`)
- **Endpoints:** `GET /ws` (WebSocket + diagnóstico), `GET /health`,
  `POST /api/push` y `GET /api/outbox` (puente con el .NET 4.8)
- **Requisitos:** Go 1.22+ (probado con 1.27.0 — `winget install GoLang.Go`)

Implementa el mismo contrato de seguridad que los backends .NET 10, Java y Python
del POC. Las referencias a `SECURITY-CHECKLIST.md §N` que aparecen en los
comentarios del código apuntan a ese documento, que vive en el repositorio
original (`bg-backend-go-test`) y no se copió aquí.

## Instalar y levantar

```bash
go mod download
go build -o wspoc-go.exe ./cmd/server
./wspoc-go.exe
# o directamente:
go run ./cmd/server
```

Cambiar el puerto:

```powershell
$env:WS_PORT = 9084; go run ./cmd/server
```

## Estructura

El proyecto está organizado por capas. La regla es que las dependencias apuntan
siempre hacia dentro: `transport` conoce el dominio, el dominio no sabe que
existe HTTP.

```
cmd/server/              arranque: construye las dependencias y las inyecta
internal/
  config/                carga del .env y variables de entorno
  protocol/              contrato del POC: mensajes, sobres, frames y catálogo de rechazos
  security/              controles del handshake (origen y JWT)
  session/               conexiones vivas (Client) y registro por sesión (Registry)
  delivery/              estrategia de entrega: local o Redis entre réplicas
  outbox/                buffer de sobres hacia el .NET 4.8 y reenvío por webhook
  ratelimit/             ventana deslizante del §6
  logging/               formato de [ACCEPT] / [REJECT] / [CLOSE] / [PUSH] / [OUTBOX]
  transport/httpapi/     handlers HTTP y bucle del WebSocket
```

`internal/` no es decorativo: el compilador impide que nada fuera de este módulo
importe esos paquetes, así que la superficie pública del proyecto es el binario,
no sus tripas.

`cmd/server/main.go` es el **único** sitio donde se construyen dependencias y se
eligen implementaciones concretas. Todo lo demás las recibe ya resueltas, lo que
permite que ninguna capa alcance estado global y que los controles se puedan
probar con distintas configuraciones sin tocar el proceso.

Dónde tocar según qué cambie:

| Cambio | Sitio |
|---|---|
| Un motivo o código de rechazo nuevo | `internal/protocol/protocol.go` (catálogo) |
| Una regla de validación de mensajes | `internal/protocol/validate.go` |
| Un control del handshake | `internal/security/guards.go` |
| Un endpoint o su forma de respuesta | `internal/transport/httpapi/` |
| El bucle de la conexión y los cierres | `internal/transport/httpapi/pump.go` |
| Un límite configurable | `internal/config/config.go` |

## Comprobar que funciona

```bash
curl http://localhost:8084/health
```

Para el resto hay una colección de Postman en la raíz: **`postman_collection.json`**
(Import → arrastrar el fichero). Cubre 30 peticiones repartidas en salud,
diagnóstico del handshake, puente REST y outbox, con tests que comprueban tanto
el código HTTP como el `reason`/`code` del contrato. El JWT HS256 lo firma un
pre-request script de la propia colección, así que no hace falta generar tokens
por fuera.

Dos avisos que la colección documenta en sus propias descripciones:

- El **canal WebSocket** no se puede probar desde una petición de colección: hay
  que crear una *WebSocket Request* aparte (`New > WebSocket`), y eso solo existe
  en la app de escritorio de Postman.
- Como el backend escucha en `127.0.0.1`, Postman **en el navegador** falla con
  *"Cannot send requests to reserved address"*: su agente corre en la nube. Hay
  que cambiar el agente a **Desktop Agent** o usar la app de escritorio.

## Contrato de rechazos

Motivos y códigos viven en un único catálogo (`internal/protocol/protocol.go`).
El `code` es el de cierre WebSocket; cuando el rechazo se responde por HTTP se
usa el estado de la última columna.

| Motivo | Código | HTTP | Cuándo |
|---|---|---|---|
| `TOKEN_MISSING` | 4001 | 401 | Sin token en la query o en `Authorization` |
| `TOKEN_INVALID` | 4002 | 401 | Firma incorrecta, o `alg` distinto de HS256 |
| `TOKEN_EXPIRED` | 4003 | 401 | `exp` vencido (sin margen de reloj) |
| `TOKEN_CLAIMS_INVALID` | 4004 | 401 | `iss`/`aud` incorrectos, sin `exp`, sin `sub`, o sesión no válida |
| `RATE_LIMIT_EXCEEDED` | 4008 | 429 | 20 msg/s por conexión, 100/s en `/api/push` |
| `MESSAGE_TOO_LARGE` | 4009 | 413 | Frame o cuerpo por encima de 64 KB |
| `INVALID_PAYLOAD` | 4010 | 400 | Esquema, tipo, `ts`, `id`, longitud o patrón de script |
| `INVALID_SESSION` | 4010 | 400 | `sesion` vacía, larga o fuera de `[A-Za-z0-9_-]` |
| `SERVER_AT_CAPACITY` | 4013 | 503 | Se alcanzó `WS_MAX_CONNECTIONS` |
| `IDLE_TIMEOUT` | 4014 | 408 * | 60 s sin datos reales del cliente |
| `ORIGIN_NOT_ALLOWED` | 4403 | 403 | `Origin` fuera de `WS_ALLOWED_ORIGINS` |
| `SESSION_NOT_FOUND` | 404 | 404 | Push a una sesión sin conexiones |
| `BACKPRESSURE` | 4009 | 503 \* | La cola de salida del cliente está llena |

\* Motivos que hoy solo cierran sockets: su estado HTTP está definido en el catálogo
por uniformidad, pero ninguna ruta los responde.

## Controles de seguridad implementados

| # | Control | Estado | Cómo está implementado |
|---|---|---|---|
| 1 | WSS/TLS | ⚠️ documentado | El POC usa `ws://`. Ver *Habilitar TLS* más abajo. |
| 2 | JWT en el handshake | ✅ | `golang-jwt/v5` con `WithValidMethods(["HS256"])`, `WithIssuer`, `WithAudience`, `WithExpirationRequired`, `WithLeeway(0)` y `sub` obligatorio (`internal/security/guards.go`). Se evalúa **antes** de `upgrader.Upgrade()`; si falla se responde 401/403 y el socket no se abre. |
| 3 | Verificación de Origin | ✅ | Comparación exacta contra `WS_ALLOWED_ORIGINS` en `Server.evaluate()` (`internal/transport/httpapi/ws.go`). `upgrader.CheckOrigin` se deja en `true` a propósito: la decisión ya se tomó antes y no debe duplicarse en dos sitios. |
| 4 | Validación/sanitización | ✅ | `internal/protocol/validate.go`: `json.Decoder` con `DisallowUnknownFields()` más `decoder.More()` (rechaza un segundo documento pegado detrás), enum cerrado de `type`, `id` `[A-Za-z0-9_-]{1,64}`, payload ≤ 8192 bytes, sin caracteres de control ni patrones de script. |
| 5 | Tamaño máximo (64 KB) | ✅ | Corte **en streaming**: `io.LimitReader(reader, max+1)` descarta el frame sin materializarlo entero, más `SetReadLimit(max*4)` como tope duro de respaldo. El puente REST usa `http.MaxBytesReader` con el mismo criterio. Cierre `4009`. |
| 6 | Rate limiting | ✅ | `ratelimit.SlidingWindow` de 1 s: 20 mensajes por conexión y 100/s globales en `/api/push`. El mutex vive **dentro** del limitador, no en cada punto de uso: el endpoint compartido no puede olvidarse de tomarlo. Cierre `4008`. |
| 7 | Protección DoS | ✅ | Máx. 200 conexiones con reserva atómica (`CompareAndSwapInt32`), idle timeout por `SetReadDeadline` que **no** se renueva con los pong, `recover()` por conexión, `ReadHeaderTimeout` contra slowloris, deadline de escritura de 5 s como backpressure y apagado ordenado con `server.Shutdown`. |
| 8 | Logging de rechazos | ✅ | `internal/logging`: un solo sitio construye `[ACCEPT]` / `[REJECT]` / `[CLOSE]` / `[PUSH]` / `[OUTBOX]`, con el `log` estándar y marca de tiempo en microsegundos. El formato es contrato con quien lee los logs, no un detalle interno. |
| 10 | Puente REST con el .NET 4.8 | ✅ | `POST /api/push` reutiliza el mismo `Guard.CheckToken` (cabecera `Authorization`, con prefijo `Bearer` opcional) y aplica la misma sanitización que el canal: un push acaba en el DOM igual que un eco. Preflight `OPTIONS` contestado, con el origen concreto de la lista blanca. |

### La escritura tiene que estar serializada

`gorilla/websocket` **no admite escrituras concurrentes**. Mientras solo escribía la goroutine de
lectura bastaba con escribir directo al socket; desde que el REST puede empujar a la vez, dos
escrituras simultáneas entrelazarían los frames. Por eso cada conexión tiene una **única goroutine
escritora** (`internal/session/client.go`) y todo lo demás encola. La goroutine se lanza dentro de
`NewClient`, de modo que no puede existir un `Client` cuya cola no esté drenando nadie.

El cierre es el punto delicado: antes de escribir el frame de cierre hay que **parar al escritor y
esperar a que salga de verdad** (`StopWriter`), y luego vaciar lo que quedara en la cola
(`FlushPending`) — si no, un `error` recién encolado se perdería al cerrar.

### Detalle que costó encontrar

Escribir el frame de cierre y cerrar el TCP acto seguido hace que el cliente vea `1006` en vez del
código real (`4008`, `4009`…): lo que quedaba en el buffer de salida se descarta.
`connection.closeWith()` (en `internal/transport/httpapi/pump.go`) drena la conexión durante 500 ms
tras escribir el cierre, de modo que el cliente recibe tanto los ecos pendientes como el código
correcto. Sin ese drenaje, el POC “mentía” sobre el motivo del cierre.

### Otras notas

- Un mensaje inválido envía primero un frame `error` con el motivo y **después** cierra con
  `4010`: no se sigue leyendo en esa conexión.
- Sin cabecera `Origin` se permite la conexión (clientes no navegador). En producción,
  `CheckOrigin` pasaría a rechazar.
- El idle timeout se apoya en la fecha límite de lectura y no se renueva con los pong de
  protocolo, así que una conexión “viva pero muda” también se cierra.
- La sesión sale del claim `sid` del token (o de `sub` si no hay `sid`), **nunca** de algo que
  declare el cliente: así nadie puede enrutar mensajes a la sesión de otro.

## Entrega entre réplicas

Con `WS_REDIS_ADDR` vacío la entrega es local y solo es correcta con **una** réplica: la conexión
WebSocket vive en la memoria de un proceso, así que con 2 pods un `POST /api/push` puede caer en el
que no tiene la sesión y el mensaje se perdería.

Con Redis configurado, el push se publica en el canal `wspoc:push`, todas las réplicas están
suscritas y entrega la que tenga la conexión. La elección se hace una sola vez en
`delivery.New()`; el resto del código habla con la interfaz `Deliverer` y no sabe cuál le tocó.

## Habilitar TLS (wss://) en producción

```go
server := &http.Server{
    Addr:              cfg.Addr(),
    Handler:           api.Routes(),
    ReadHeaderTimeout: readHeaderTimeout,
    TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
}
logging.Fatalf("TLS: %v", server.ListenAndServeTLS("cert.pem", "key.pem"))
```

Detrás de un reverse proxy que termine TLS, hay que asegurarse de que reenvíe las cabeceras
`Origin`, `Upgrade` y `Connection`, y usar `X-Forwarded-For` para la IP real en los logs.

## Resultados medidos

120 conexiones × 10 mensajes @ 10 msg/s.

| Métrica | Valor |
|---|---|
| Conexiones aceptadas | 120/120 |
| Handshake p50 / p95 / máx | 61,0 / 65,6 / 75,7 ms |
| RTT p50 / p95 / máx | 3,3 / 4,2 / 4,6 ms |
| Mensajes perdidos | 0 / 1200 |
| Throughput medio | 432 msg/s |
| Servidor sano tras la carga | Sí (pong en 2,1 ms) |
| Prueba de tope: 260 conexiones | 200 aceptadas, 60 rechazadas con `SERVER_AT_CAPACITY`, servidor sano |
| Sondas de seguridad | 28/28 (17 del canal + 11 del puente REST) |

Estas cifras se midieron con `tools/loadtest.js` y `tools/probe.js` del repositorio original, que no
se copiaron aquí. **No se han vuelto a medir tras la reorganización en paquetes**; lo que sí se
verificó es que el comportamiento no cambió: 45 casos (28 HTTP + 17 del canal WebSocket) dan
resultados idénticos y las líneas de log salen byte a byte iguales.
