# Backend Go — net/http + gorilla/websocket

- **Puerto:** `8084` (configurable con `WS_PORT` o `WS_PORT_GO` en el `.env`)
- **Endpoints:** `GET /ws` (WebSocket + diagnóstico), `GET /health`,
  `POST /api/session-token` (intercambio `SESION` → token),
  `POST /api/push` y `GET /api/outbox` (puente con el .NET 4.8)
- **Requisitos:** Go 1.22+ (probado con 1.27.0 — `winget install GoLang.Go`)

Este servicio no tiene lógica de negocio: mantiene el canal WebSocket con los
navegadores y hace de puente con el backend **.NET 4.8**, que es quien decide.
Lo que el cliente envía por el canal se acumula en el outbox y el .NET lo recoge
por `GET /api/outbox`; lo que el .NET quiere entregar a una sesión lo empuja por
`POST /api/push`.

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
  ratelimit/             ventana deslizante del control de caudal
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
(Import → arrastrar el fichero). Cubre 40 peticiones repartidas en salud,
diagnóstico del handshake, intercambio `SESION` → token, puente REST y outbox, con
tests que comprueban tanto el código HTTP como el `reason`/`code` del contrato. El JWT HS256 lo firma un
pre-request script de la propia colección, así que no hace falta generar tokens
por fuera.

**Antes de lanzar nada**, pon tu secreto: *Collection > Variables > `jwtSecret` >
columna **Current value*** = el `WS_JWT_SECRET` de tu `.env`. Tiene que ser *Current
value* y no *Initial value*, porque el Initial se exporta dentro del fichero de la
colección y el `.env` está en `.gitignore` justamente para que el secreto no acabe
versionado. Si te lo saltas, todo lo autenticado responde `401 TOKEN_INVALID`.

La colección maneja **dos tokens distintos**:

| Variable | De dónde sale | Qué abre |
|---|---|---|
| `{{token}}` | Lo firma solo el pre-request script en cada petición | `/api/push`, `/api/outbox` y `/ws` |
| `{{tokenIntercambio}}` | Lo devuelve `POST /api/session-token` (carpeta 01b) | Solo `/ws` — en el puente da `401` a propósito |

Dos avisos que la colección documenta en sus propias descripciones:

- El **canal WebSocket** no se puede probar desde una petición de colección: hay
  que crear una *WebSocket Request* aparte (`New > WebSocket`), y eso solo existe
  en la app de escritorio de Postman.
- Como el backend escucha en `127.0.0.1`, Postman **en el navegador** falla con
  *"Cannot send requests to reserved address"*: su agente corre en la nube. Hay
  que cambiar el agente a **Desktop Agent** o usar la app de escritorio.

## Identificar la sesión del frontend (`POST /api/session-token`)

El login lo hace el frontend contra otro backend. De ese login se queda con un
identificador único de sesión, que en el payload viaja como `SESION`:

```json
{ "...": "...", "SESION": "3261433167311311224536722201" }
```

El canal enruta por sesión (`Registry` es un `map[sesión][]*Client`), pero el
handshake solo sabe leerla de un token firmado. Como el emisor del login no firma
tokens de este servicio, aquí se canjea:

```bash
curl -X POST http://localhost:8084/api/session-token   -H 'Origin: http://localhost:4200' -H 'Content-Type: application/json'   -d '{"SESION":"3261433167311311224536722201"}'
```

```json
{ "token": "eyJhbGciOi...", "SESION": "3261...", "expira_en": 3600, "instance": "pod-1" }
```

El token sale firmado con la sesión en `sid`, `SESION` y `sub`, más la marca
`canal: true` que lo limita al WebSocket (ver más abajo). Se usa tal cual en el
handshake:

```js
const { token } = await (await fetch(`${API}/api/session-token`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ SESION: loginPayload.SESION }),
})).json();

const ws = new WebSocket(`${WS}/ws?token=${encodeURIComponent(token)}`);
```

A partir de ahí, `POST /api/push` con `{"Sesion": "3261...", "Payload": { ... }}`
llega a todas las pestañas abiertas de ese usuario. Ver *Contrato con el .NET* más abajo.

Vigencia por defecto: 1 h (`WS_SESSION_TOKEN_TTL_SECONDS`), alineada con la sesión
de login, así que el mismo token sirve para reconectar. Ante un cierre `4003
TOKEN_EXPIRED`, el frontend repite el canje.

### Qué asume este endpoint

Es el único sin `Authorization`, porque es el que la produce: **quien presente una
`SESION` válida obtiene sus mensajes**, de modo que la `SESION` es la credencial.
Lo que la protege es:

| Control | Ajuste |
|---|---|
| `Origin` presente y en la lista blanca | `WS_ALLOWED_ORIGINS` |
| Caudal propio, separado del push | `WS_SESSION_TOKEN_RATE_LIMIT_PER_SEC` (20/s) |
| Vigencia acotada | `WS_SESSION_TOKEN_TTL_SECONDS` (3600) |
| Mismo alfabeto que el resto de sesiones | `[A-Za-z0-9_-]`, ≤64 caracteres |
| El token solo abre el canal, no el puente | Marca `canal: true`, rechazada por `CheckBridgeToken` |

**Lo que el token canjeado NO puede hacer.** Un token de este endpoint sirve para
`/ws` y para nada más. `/api/push` y `/api/outbox` lo rechazan con `401
TOKEN_CLAIMS_INVALID`, porque ahí se elige a qué sesión se entrega y qué mensajes
se leen: con el mismo control en las dos puertas, quien canjeara una SESION
cualquiera podría empujar mensajes a la sesión de otro.

La exclusión se apoya en la marca `canal`, no en un permiso explícito, porque el
backend que empuja no tiene por qué cambiar sus tokens: los suyos no llevan la
marca y siguen valiendo igual. Lo que sostiene el control es que para producir un
token **sin** la marca hay que conocer `WS_JWT_SECRET`, que es justo lo que se le
exigía al puente antes de que este endpoint existiera. La contrapartida: un emisor
nuevo que olvidara la marca reabriría el puente, y por eso la pone `Issuer` en un
único sitio.

El riesgo residual depende de la entropía de la `SESION`: si fuera secuencial o
derivable, el endpoint sería enumerable pese al caudal limitado. Con TLS y una
`SESION` aleatoria, el ataque se reduce a adivinarla.

## Contrato con el .NET (`Sesion` / `Identificador` / `Payload`)

El sobre que se intercambia con el .NET usa la grafía con la que ese backend
serializa, en mayúscula. Va en los dos sentidos y también en el frame que llega al
navegador.

**El .NET empuja** (`POST /api/push`, con `Authorization: Bearer <token>`):

```json
{
  "Sesion": "12412412412414141",
  "Identificador": 130,
  "Payload": {
    "CodigoError": "COD000",
    "Datos": [
      { "codigoError": "COD005", "opstatus": 0, "mensaje": "Su sesión ha expirado!", "httpStatusCode": 200 }
    ],
    "Mensaje": "OK"
  }
}
```

`Payload` es **JSON en crudo** (`json.RawMessage`): normalmente un objeto, pero
vale cualquier JSON válido. El canal no interpreta su contenido —solo lo sanea como
texto y lo entrega verbatim—, así que el .NET puede cambiar su forma interna sin que
haya que tocar ni desplegar este servicio. `Identificador` es opcional; `Payload` no:
si falta, `400 INVALID_PAYLOAD`.

**El navegador recibe**:

```json
{
  "type": "push",
  "id": "push",
  "ts": 1788964701502,
  "instance": "pod-1",
  "Sesion": "12412412412414141",
  "Identificador": 130,
  "Payload": { "CodigoError": "COD000", "Datos": [ ... ], "Mensaje": "OK" }
}
```

```js
ws.onmessage = (e) => {
  const frame = JSON.parse(e.data);
  if (frame.type !== 'push') return;

  const datos = frame.Payload;               // ya es un objeto, sin JSON.parse
  console.log(frame.Identificador);          // 130
  console.log(datos.Datos[0].mensaje);       // "Su sesión ha expirado!"
};
```

### La regla de la grafía

**Mayúscula lo que es dato, minúscula lo que es protocolo del canal.** `type`, `id`,
`ts`, `instance` y `reason` son del canal; `Sesion`, `Identificador` y `Payload`
vienen del .NET y viajan con su nombre original.

Hay una asimetría que conviene tener presente: el navegador **envía** `payload` en
minúscula (`ClientMessage`, esquema estricto) y **recibe** `Payload` en
mayúscula. Se envía en el lenguaje del canal y se recibe con la grafía del dato.

El **tipo** de `Payload` depende del `type` del frame, porque su contenido tiene
orígenes distintos:

| Frame | `Payload` |
|---|---|
| `push` | El objeto del .NET, verbatim |
| `echo` | Cadena: lo que envió el propio cliente |
| `pong`, `error` | Cadena vacía |

El frontend ya distingue por `type`, así que no tiene que adivinarlo. En el sentido
inverso (el sobre que va al .NET por el webhook) `Payload` es siempre una cadena: es
texto del navegador, no un objeto.

### Compatibilidad

En el sentido de entrada la grafía da igual: `encoding/json` empareja las claves
ignorando mayúsculas, así que un `{"sesion": "...", "payload": "..."}` en minúscula
—lo que mandaban los clientes antiguos— sigue aceptándose. Lo que no se acepta es un
campo que el esquema no conozca: sigue siendo `400 INVALID_PAYLOAD`.

### Dos límites

- El `Payload` **serializado** no puede pasar de **8192 caracteres**
  (`maxPayloadChars`). Se mide sobre el JSON ya compactado, no sobre el objeto. Es un
  límite del protocolo, compartido con los otros tres backends del POC.
- El filtro anti-XSS mira el texto del payload (`<script`, `javascript:`, `onerror=`,
  `onload=`, `<iframe`, `data:text/html`). Un mensaje de negocio que contuviera una
  de esas cadenas rebotaría con `400 INVALID_PAYLOAD`.

Y lo que no es código: el `Sesion` que manda el .NET tiene que ser **el mismo valor
exacto** que la `SESION` que el frontend canjeó al hacer login. Es lo único que hace
que el mensaje encuentre a su destinatario.

## Contrato de rechazos

Motivos y códigos viven en un único catálogo (`internal/protocol/protocol.go`).
El `code` es el de cierre WebSocket; cuando el rechazo se responde por HTTP se
usa el estado de la última columna.

| Motivo | Código | HTTP | Cuándo |
|---|---|---|---|
| `TOKEN_MISSING` | 4001 | 401 | Sin token en la query o en `Authorization` |
| `TOKEN_INVALID` | 4002 | 401 | Firma incorrecta, o `alg` distinto de HS256 |
| `TOKEN_EXPIRED` | 4003 | 401 | `exp` vencido (sin margen de reloj) |
| `TOKEN_CLAIMS_INVALID` | 4004 | 401 | `iss`/`aud` incorrectos, sin `exp`, sesión ausente/no válida, o token del canal (`canal`) usado en el puente REST |
| `RATE_LIMIT_EXCEEDED` | 4008 | 429 | 20 msg/s por conexión, 100/s en `/api/push`, 20/s en `/api/session-token` |
| `MESSAGE_TOO_LARGE` | 4009 | 413 | Frame o cuerpo por encima de 64 KB |
| `INVALID_PAYLOAD` | 4010 | 400 | Esquema, tipo, `ts`, `id`, longitud o patrón de script |
| `INVALID_SESSION` | 4010 | 400 | `Sesion` vacía, larga o fuera de `[A-Za-z0-9_-]` |
| `SERVER_AT_CAPACITY` | 4013 | 503 | Se alcanzó `WS_MAX_CONNECTIONS` |
| `IDLE_TIMEOUT` | 4014 | 408 * | 60 s sin señales del cliente: ni datos, ni respuesta al latido |
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
| 7 | Protección DoS | ✅ | Máx. 200 conexiones con reserva atómica (`CompareAndSwapInt32`), idle timeout por `SetReadDeadline` renovado por los datos del cliente y por el latido de protocolo, `recover()` por conexión, `ReadHeaderTimeout` contra slowloris, deadline de escritura de 5 s como backpressure y apagado ordenado con `server.Shutdown`. Una conexión que calla pero contesta al ping se mantiene; a la que no contesta la cierra el idle timeout, y el número total lo acota `WS_MAX_CONNECTIONS`. |
| 8 | Logging de rechazos | ✅ | `internal/logging`: un solo sitio construye `[ACCEPT]` / `[REJECT]` / `[CLOSE]` / `[PUSH]` / `[OUTBOX]`, con el `log` estándar y marca de tiempo en microsegundos. El formato es contrato con quien lee los logs, no un detalle interno. |
| 10 | Puente REST con el .NET 4.8 | ✅ | `POST /api/push` usa `Guard.CheckBridgeToken` (cabecera `Authorization`, con prefijo `Bearer` opcional): la misma verificación del canal **más** el rechazo de los tokens marcados con `canal`, que emite el intercambio del frontend. Sin esa exclusión, cualquiera que canjeara una SESION podría empujar mensajes a la sesión de otro. Aplica además la misma sanitización que el canal: un push acaba en el DOM igual que un eco. Preflight `OPTIONS` contestado, con el origen concreto de la lista blanca. |

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
  `CheckOrigin` pasaría a rechazar. La excepción es `POST /api/session-token`, que **exige**
  un `Origin` de la lista blanca: ese hueco es aceptable para el canal, pero no para el
  endpoint que emite los tokens.
- El idle timeout se apoya en la fecha límite de lectura y **sí** se renueva con el latido
  de protocolo: lo que cierra es una conexión que no *contesta*, no una que solo calla. Ver
  *Latido* más abajo.
- La sesión sale del token —claim `sid`, si no `SESION`, si no `sub`—, **nunca** de algo que
  declare el cliente en el canal: así nadie puede enrutar mensajes a la sesión de otro.
- Hay dos controles de token, no uno: `CheckToken` para el canal y `CheckBridgeToken` para el
  puente REST. El segundo es el primero más el rechazo de los tokens marcados con `canal`.
  Emitir tokens al frontend y autenticar a los backends que empujan dejaron de ser lo mismo
  en cuanto apareció el intercambio.

## Latido (keepalive)

**El servidor manda un ping de protocolo cada `WS_IDLE_TIMEOUT_SECONDS / 2`** (30 s
con la configuración por defecto) y el cliente responde con un pong que renueva el
plazo de lectura. El frontend no necesita hacer nada: el JavaScript de un navegador
no puede *enviar* pings —la API de WebSocket no lo expone— pero responde a los
nuestros automáticamente.

Un cliente nativo (Postman, k6) puede además mandar sus propios pings; se le contesta
el pong y también renuevan el plazo.

Comprobado con `WS_IDLE_TIMEOUT_SECONDS=6`:

| Cliente | Resultado |
|---|---|
| Calla, pero responde a los pings (un navegador) | sobrevive |
| Manda pings propios (un cliente nativo) | sobrevive |
| No contesta a los pings (proceso colgado) | `4014 IDLE_TIMEOUT` a los 6 s |

### Qué se ganó y qué se perdió

Antes el plazo solo lo renovaban los datos de aplicación, así que se cerraba toda
conexión que se limitara a escuchar — que es justo el caso de uso de este servicio:
un navegador esperando un valor que le llega por push. Recibir pushes no cuenta,
porque el plazo mide el sentido cliente → servidor.

A cambio, una conexión abierta y silenciosa puede quedarse indefinidamente mientras
su TCP siga vivo. Lo que acota el consumo ya no es el tiempo sino el cupo,
`WS_MAX_CONNECTIONS`.

El latido va en `writePump` (`internal/session/client.go`), que es la única goroutine
que escribe; los handlers que renuevan el plazo están en `connection.keepAlive`
(`internal/transport/httpapi/pump.go`).

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
