# Backend Go — net/http + gorilla/websocket

- **Puerto:** `8084` (configurable con `WS_PORT` o `WS_PORT_GO` en el `.env` de la raíz)
- **Endpoints:** `GET /ws` (WebSocket + diagnóstico), `GET /health`,
  `POST /api/push` y `GET /api/outbox` (puente con el .NET 4.8)
- **Requisitos:** Go 1.22+ (probado con 1.27.0 — `winget install GoLang.Go`)

## Instalar y levantar

```bash
cd backend-go
go mod download
go build -o wspoc-go.exe .
./wspoc-go.exe
# o directamente:
go run .
```

Cambiar el puerto:

```powershell
$env:WS_PORT = 9084; go run .
```

## Comprobar que funciona

```bash
curl http://localhost:8084/health
node tools/probe.js
node tools/loadtest.js
```

## Controles de seguridad implementados

| # | Control | Estado | Cómo está implementado |
|---|---|---|---|
| 1 | WSS/TLS | ⚠️ documentado | El POC usa `ws://`. Ver *Habilitar TLS* más abajo. |
| 2 | JWT en el handshake | ✅ | `golang-jwt/v5` con `WithValidMethods(["HS256"])`, `WithIssuer`, `WithAudience`, `WithExpirationRequired`, `WithLeeway(0)` y `sub` obligatorio. Se evalúa **antes** de `upgrader.Upgrade()`; si falla se responde 401/403 y el socket no se abre. |
| 3 | Verificación de Origin | ✅ | Comparación exacta contra `WS_ALLOWED_ORIGINS` en `evaluate()`. `upgrader.CheckOrigin` se deja en `true` a propósito: la decisión ya se tomó antes y no debe duplicarse en dos sitios. |
| 4 | Validación/sanitización | ✅ | `json.Decoder` con `DisallowUnknownFields()` más `decoder.More()` (rechaza un segundo documento pegado detrás), enum cerrado de `type`, `id` `[A-Za-z0-9_-]{1,64}`, payload ≤ 8192 bytes, sin caracteres de control ni patrones de script. |
| 5 | Tamaño máximo (64 KB) | ✅ | Corte **en streaming**: `io.LimitReader(reader, max+1)` descarta el frame sin materializarlo entero, más `SetReadLimit(max*4)` como tope duro de respaldo. Cierre `4009`. |
| 6 | Rate limiting por conexión | ✅ | `SlidingWindowLimiter` de 1 s / 20 mensajes, propio de cada goroutine de lectura (sin mutex, sin estado compartido). Cierre `4008`. |
| 7 | Protección DoS | ✅ | Máx. 200 conexiones con reserva atómica (`CompareAndSwapInt32`), idle timeout por `SetReadDeadline` que **no** se renueva con los pong, `recover()` por conexión, `ReadHeaderTimeout` contra slowloris, deadline de escritura de 5 s como backpressure y apagado ordenado con `server.Shutdown`. |
| 8 | Logging de rechazos | ✅ | `[ACCEPT]` / `[REJECT]` / `[CLOSE]` / `[PUSH]` / `[OUTBOX]` con el `log` estándar y marca de tiempo en microsegundos. |
| 10 | Puente REST con el .NET 4.8 | ✅ | `POST /api/push` reutiliza `CheckToken` (cabecera `Authorization`) y aplica la misma sanitización que el canal: un push acaba en el DOM igual que un eco. Preflight `OPTIONS` contestado, con el origen concreto de la lista blanca. |

### La escritura tiene que estar serializada

`gorilla/websocket` **no admite escrituras concurrentes**. Mientras solo escribía la goroutine de
lectura bastaba con escribir directo al socket; desde que el REST puede empujar a la vez, dos
escrituras simultáneas entrelazarían los frames. Por eso cada conexión tiene una **única goroutine
escritora** (`sessions.go`) y todo lo demás encola.

El cierre es el punto delicado: antes de escribir el frame de cierre hay que **parar al escritor y
esperar a que salga de verdad** (`StopWriter`), y luego vaciar lo que quedara en la cola
(`FlushPending`) — si no, un `error` recién encolado se perdería al cerrar.

### Detalle que costó encontrar

Escribir el frame de cierre y cerrar el TCP acto seguido hace que el cliente vea `1006` en vez del
código real (`4008`, `4009`…): lo que quedaba en el buffer de salida se descarta. `closeWith()`
drena la conexión durante 500 ms tras escribir el cierre, de modo que el cliente recibe tanto los
ecos pendientes como el código correcto. Sin ese drenaje, el POC “mentía” sobre el motivo del cierre.

### Otras notas

- Un mensaje inválido envía primero un frame `error` con el motivo y **después** cierra con
  `4010`: no se sigue leyendo en esa conexión.
- Sin cabecera `Origin` se permite la conexión (clientes no navegador). En producción,
  `CheckOrigin` pasaría a rechazar.
- El idle timeout se apoya en la fecha límite de lectura y no se renueva con los pong de
  protocolo, así que una conexión “viva pero muda” también se cierra.

## Habilitar TLS (wss://) en producción

```go
server := &http.Server{
    Addr:              fmt.Sprintf(":%d", cfg.Port),
    Handler:           mux,
    ReadHeaderTimeout: 10 * time.Second,
    TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
}
log.Fatal(server.ListenAndServeTLS("cert.pem", "key.pem"))
```

Detrás de un reverse proxy que termine TLS, hay que asegurarse de que reenvíe las cabeceras
`Origin`, `Upgrade` y `Connection`, y usar `X-Forwarded-For` para la IP real en los logs.

## Resultados medidos

120 conexiones × 10 mensajes @ 10 msg/s, `tools/loadtest.js`.

| Métrica | Valor |
|---|---|
| Conexiones aceptadas | 120/120 |
| Handshake p50 / p95 / máx | 61,0 / 65,6 / 75,7 ms |
| RTT p50 / p95 / máx | 3,3 / 4,2 / 4,6 ms |
| Mensajes perdidos | 0 / 1200 |
| Throughput medio | 432 msg/s |
| Servidor sano tras la carga | Sí (pong en 2,1 ms) |
| Prueba de tope: 260 conexiones | 200 aceptadas, 60 rechazadas con `SERVER_AT_CAPACITY`, servidor sano |
| Sondas de seguridad (`tools/probe.js`) | 28/28 (17 del canal + 11 del puente REST) |
