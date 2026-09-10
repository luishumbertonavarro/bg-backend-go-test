# Backend Go — net/http + gorilla/websocket

- **Puerto:** `8084` (configurable con `WS_PORT` o `WS_PORT_GO` en el `.env`)
- **Endpoints:** `GET /ws` (WebSocket + diagnóstico), `GET /health` y
  `POST /api/session-token` (intercambio `SESION` → token)
- **Requisitos:** Go 1.22+ (probado con 1.27.0 — `winget install GoLang.Go`)

Este servicio no tiene lógica de negocio: mantiene el canal WebSocket con los
navegadores y hace de puente con el backend **.NET 4.8**, que es quien decide.
Lo que el cliente pregunta por el canal se reenvía por REST al .NET, que responde
en ese mismo HTTP, y la respuesta baja por el mismo canal.

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
  bridge/                la llamada síncrona al .NET 4.8: pregunta y espera
  ratelimit/             ventana deslizante del control de caudal
  logging/               formato de [ACCEPT] / [REJECT] / [CLOSE] / [TOKEN] / [PETICION]
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
| La llamada al .NET (timeout, concurrencia) | `internal/bridge/bridge.go` |
| Un límite configurable | `internal/config/config.go` |

## Comprobar que funciona

```bash
curl http://localhost:8084/health
```

Para el resto hay una colección de Postman en la raíz: **`postman_collection.json`**
(Import → arrastrar el fichero). Son 8 peticiones en tres carpetas, pensadas para
repartirlas:

| Carpeta | Para quién | Qué comprueba |
|---|---|---|
| `00 — Salud` | cualquiera | Que el backend está levantado y accesible |
| `01 — Frontend` | el dev de frontend | El canje del token, el diagnóstico del handshake y el 409 al reutilizar una sesión conectada. Su LEEME lleva el ciclo completo y la lista de qué probar |
| `02 — .NET 4.8` | el dev del .NET | Que su endpoint cumple lo que Go valida: esquema cerrado, tamaño, saneado y tiempo de respuesta |

**La colección no firma tokens ni conoce ningún secreto.** El token sale de
`POST /api/session-token`, igual que en el frontend real; la petición deja la URL
del WebSocket lista para copiar en la variable `wsUrl`.

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

El token sale firmado con la sesión en `sid`, `SESION` y `sub` — los tres claims
que sabe leer el handshake. Se usa tal cual:

```js
const { token } = await (await fetch(`${API}/api/session-token`, {
  method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ SESION: loginPayload.SESION }),
})).json();

const ws = new WebSocket(`${WS}/ws?token=${encodeURIComponent(token)}`);
```

A partir de ahí, lo que el cliente pregunte con `type: "peticion"` viaja al .NET y su
respuesta baja por ese mismo socket. Ver *El ciclo completo* más abajo.

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
| Caudal propio | `WS_SESSION_TOKEN_RATE_LIMIT_PER_SEC` (20/s) |
| Vigencia acotada | `WS_SESSION_TOKEN_TTL_SECONDS` (3600) |
| Mismo alfabeto que el resto de sesiones | `[A-Za-z0-9_-]`, ≤64 caracteres |
| No se emite para una sesión ya conectada | `409 SESSION_IN_USE`, consultando el `Registry` |

**Lo que el token canjeado NO puede hacer.** Sirve para abrir `/ws` y para nada
más, porque no hay nada más: es la única puerta autenticada del servicio. Hubo un
puente REST (`/api/push`, `/api/outbox`) que exigía un token distinto y una marca
`canal` para distinguirlos; al retirarse el puente, la distinción sobró.

**El riesgo abierto: la `SESION` es un número.** La genera el frontend, así que no
hay nada que adivinar: probar `1`, `2`, `3`… basta para intentar quedarse con el
canal de otro. Lo que hay puesto es `409 SESSION_IN_USE` —que cierra el caso
realista, robar una sesión mientras su dueño la usa—, el caudal limitado y la traza
`[TOKEN]` de cada emisión. Lo que **no** cierra es reclamar una `SESION` antes que
su dueño; para eso el número tendría que llevar entropía, generarlo Go o validarlo
el .NET, y las tres cosas cambian el contrato del login. Ver §12 del
[checklist](../SECURITY-CHECKLIST.md).

## El ciclo completo: una peticion del cliente al .NET y vuelta

Es el recorrido que hace una peticion real del frontend. Lo que sube por el canal baja por el
mismo canal, con el `.NET` en medio:

```
Angular --(WS: peticion)--> Go --(REST: POST)--> .NET 4.8
                                                    | calcula
Angular <--(WS: respuesta)-- Go <--(misma respuesta HTTP)--
```

**La llamada al .NET es sincrona a proposito.** El .NET Framework 4.8 no es codigo nuestro y no
se toca, asi que no puede llamar de vuelta a `POST /api/push`: la unica forma de que su respuesta
llegue al navegador es que Go la recoja del mismo HTTP en el que pregunto.

### El ciclo, paso a paso

**1. La SESION.** La genera el frontend: un numero unico, que es tambien el identificador de su
conexion. Los digitos ya pasan el alfabeto `[A-Za-z0-9_-]` del validador, asi que no hace falta
ninguna forma especial.

**2. Canjearla por un token.**

```
POST /api/session-token
Origin: http://localhost:4200
{ "SESION": "12345678" }

200 { "token": "...", "SESION": "12345678", "expira_en": 3600, "instance": "..." }
```

**3. Abrir el canal** con ese token: `GET /ws?token=<token>`. La sesion que enruta los mensajes
sale del token, nunca de lo que declare el cliente por el canal.

**4. Preguntar.**

```json
{ "type": "peticion", "id": "q1", "ts": 1700000000000, "payload": { "operacion": "lo que sea" } }
```

El `payload` de una `peticion` es un **objeto**, no texto: es lo que el .NET espera recibir. (El
`echo`, que existe para probar el canal sin el .NET delante, sigue aceptando una cadena — `"hola"`
es JSON valido.)

**5. Recibir la respuesta**, por el mismo socket y con el **mismo `id`**:

```json
{ "type": "respuesta", "id": "q1", "Sesion": "12345678", "Payload": { ... }, "instance": "..." }
```

Ese `id` es lo que permite casar pregunta y respuesta cuando hay varias en vuelo. **No viaja al
.NET ni vuelve de el**: como la llamada es sincrona, Go ya sabe a que peticion corresponde lo que
recibe, y se limita a guardar el `id` mientras espera. El .NET no tiene que devolver ningun
`Identificador`.

### Ojo con la grafia

Se **envia** `payload` en minuscula y se **recibe** `Payload` en mayuscula. No es un descuido: se
manda en el lenguaje del canal y se recibe con la grafia del dato, que es la del .NET. Es lo
primero que despista al escribir el frontend.

### A quien se le entrega la respuesta

El .NET devuelve una `Sesion` en el sobre. La regla, en orden:

| Lo que devuelve el .NET | Que hace Go |
|---|---|
| `Sesion` vacia | La entrega a quien pregunto. El .NET no tiene que repetir un dato que Go ya sabe. |
| `Sesion` igual a la de origen | La entrega. |
| `Sesion` **distinta** | **La descarta** y avisa al cliente con `BACKEND_ERROR`. |

El tercer caso es deliberado: en un round-trip sincrono no hay razon legitima para que difiera, y
obedecerla convertiria un fallo del .NET en la respuesta de un usuario entregada a otro.

### Cuando el .NET falla

Llega un frame `error` con el **mismo `id`** de la peticion, y **el canal sigue abierto**: que el
backend de negocio falle no es culpa de quien pregunto, y cerrarle el socket le costaria ademas
todas las peticiones que tuviera en vuelo.

| `reason` | Cuando |
|---|---|
| `BACKEND_UNAVAILABLE` | No hay `WS_DOTNET_API_URL` configurada. |
| `BACKEND_TIMEOUT` | El .NET tardo mas de `WS_DOTNET_TIMEOUT_SECONDS`. |
| `BACKEND_ERROR` | El .NET respondio 5xx, algo que no pasa la validacion, o una `Sesion` ajena. |
| `BACKEND_BUSY` | Ya hay `WS_DOTNET_MAX_INFLIGHT` llamadas en vuelo hacia el .NET. |

El tope de llamadas simultaneas no es opcional: con 200 conexiones a 20 msg/s, sin el saldrian
4000 peticiones por segundo contra un Framework 4.8. Al llegar al tope se **rechaza en el acto**
en vez de encolar — un cliente prefiere saber que no hay sitio a esperar una respuesta que llega
tarde o no llega.

### Probarlo sin el .NET real

```bash
node tools/fake-dotnet.js            # simula la ruta del .NET, en otra consola
node tools/peticion.js --ping        # abre el canal, pregunta y espera la respuesta
```

`fake-dotnet.js` devuelve por defecto la `Sesion` que recibe y el número con un estado, como hará
el .NET real. Acepta `--delay`, `--status`, `--sesion` y `--sin-sesion` para forzar cada uno de los
caminos de
arriba. La carpeta `01` de la coleccion Postman lleva el mismo guion para hacerlo a mano, y la `02`
es el contrato que tiene que cumplir el .NET — con un Mock Server de Postman en su papel
mientras la ruta real no exista.

## Contrato con el .NET (`Sesion` / `Identificador` / `Payload`)

El sobre que se intercambia con el .NET usa la grafía con la que ese backend
serializa, en mayúscula. Va en los dos sentidos y también en el frame que llega al
navegador.

**Lo que el .NET responde** a una `peticion`:

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
haya que tocar ni desplegar este servicio. `Identificador` es opcional y hoy nadie lo
rellena —con la llamada síncrona, Go ya sabe a qué petición corresponde la respuesta—;
`Payload` no lo es: si falta, la respuesta se rechaza.

**El navegador recibe**:

```json
{
  "type": "respuesta",
  "id": "q1",
  "ts": 1788964701502,
  "instance": "pod-1",
  "Sesion": "12412412412414141",
  "Payload": { "CodigoError": "COD000", "Datos": [ ... ], "Mensaje": "OK" }
}
```

```js
ws.onmessage = (e) => {
  const frame = JSON.parse(e.data);
  if (frame.type !== 'respuesta') return;

  const datos = frame.Payload;               // ya es un objeto, sin JSON.parse
  console.log(frame.id);                     // "q1", el id con el que preguntaste
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
| `RATE_LIMIT_EXCEEDED` | 4008 | 429 | 20 msg/s por conexión, 20/s en `/api/session-token` |
| `MESSAGE_TOO_LARGE` | 4009 | 413 | Frame o cuerpo por encima de 64 KB |
| `INVALID_PAYLOAD` | 4010 | 400 | Esquema, tipo, `ts`, `id`, longitud o patrón de script |
| `INVALID_SESSION` | 4010 | 400 | `Sesion` vacía, larga o fuera de `[A-Za-z0-9_-]` |
| `SERVER_AT_CAPACITY` | 4013 | 503 | Se alcanzó `WS_MAX_CONNECTIONS` |
| `IDLE_TIMEOUT` | 4014 | 408 * | 60 s sin señales del cliente: ni datos, ni respuesta al latido |
| `ORIGIN_NOT_ALLOWED` | 4403 | 403 | `Origin` fuera de `WS_ALLOWED_ORIGINS` |
| `SESSION_NOT_FOUND` | 404 | 404 | Push a una sesión sin conexiones |
| `BACKPRESSURE` | 4009 | 503 \* | La cola de salida del cliente está llena. **Cierra la conexión.** |
| `SESSION_IN_USE` | 4016 | 409 | Se pidió un token para una `SESION` que ya tiene conexión viva |
| `BACKEND_UNAVAILABLE` | 4015 | 502 | No hay `WS_DOTNET_API_URL` configurada |
| `BACKEND_TIMEOUT` | 4015 | 504 | El .NET tardó más de `WS_DOTNET_TIMEOUT_SECONDS` |
| `BACKEND_ERROR` | 4015 | 502 | El .NET respondió 5xx, algo no válido, o una `Sesion` ajena |
| `BACKEND_BUSY` | 4015 | 503 | Ya hay `WS_DOTNET_MAX_INFLIGHT` llamadas al .NET en vuelo |

\* Motivos que hoy solo cierran sockets: su estado HTTP está definido en el catálogo
por uniformidad, pero ninguna ruta los responde.

### Los cuatro `BACKEND_*` NO cierran la conexión

Es la distinción que más importa al escribir el cliente. Un frame `error` puede significar
dos cosas opuestas, y se distinguen por el motivo:

- **`BACKEND_*`** — falló el .NET, o está saturado. Llega con el `id` de tu petición y **el
  socket sigue abierto**: falla esa petición, no la sesión. Puedes reintentar sin reconectar.
- **Todos los demás** — rompiste el contrato del canal (esquema, tamaño, caudal) o se agotó
  el plazo. Llega con `id: "-"` y **detrás viene el cierre**.

Por eso `BACKEND_BUSY` no reutiliza `BACKPRESSURE` aunque ambos signifiquen "ahora no puedo":
el de la cola de escritura cierra la conexión y el del puente no, y un mismo `reason` con dos
consecuencias opuestas es indistinguible desde el frontend.

## Controles de seguridad implementados

| # | Control | Estado | Cómo está implementado |
|---|---|---|---|
| 1 | WSS/TLS | ⚠️ documentado | El POC usa `ws://`. Ver *Habilitar TLS* más abajo. |
| 2 | JWT en el handshake | ✅ | `golang-jwt/v5` con `WithValidMethods(["HS256"])`, `WithIssuer`, `WithAudience`, `WithExpirationRequired`, `WithIssuedAt` y `WithLeeway(0)` (`internal/security/guards.go`). Lo obligatorio es la **sesión** —`sid`, si no `SESION`, si no `sub`—, no el `sub`: un token que identifique a alguien pero no diga a qué sesión enrutar no sirve para nada. Se evalúa **antes** de `upgrader.Upgrade()`; si falla se responde 401/403 y el socket no se abre. |
| 3 | Verificación de Origin | ✅ | Comparación exacta contra `WS_ALLOWED_ORIGINS` en `Server.evaluate()` (`internal/transport/httpapi/ws.go`). `upgrader.CheckOrigin` se deja en `true` a propósito: la decisión ya se tomó antes y no debe duplicarse en dos sitios. |
| 4 | Validación/sanitización | ✅ | `internal/protocol/validate.go`: `json.Decoder` con `DisallowUnknownFields()` más `decoder.More()` (rechaza un segundo documento pegado detrás), enum cerrado de `type`, `id` `[A-Za-z0-9_-]{1,64}`, payload ≤ 8192 bytes, sin caracteres de control ni patrones de script. |
| 5 | Tamaño máximo (64 KB) | ✅ | Corte **en streaming**: `io.LimitReader(reader, max+1)` descarta el frame sin materializarlo entero, más `SetReadLimit(max*4)` como tope duro de respaldo. `POST /api/session-token` y la respuesta del .NET usan `http.MaxBytesReader` e `io.LimitReader` con el mismo criterio. Cierre `4009`. |
| 6 | Rate limiting | ✅ | `ratelimit.SlidingWindow` de 1 s: 20 mensajes por conexión y 20/s en `/api/session-token`. El mutex vive **dentro** del limitador, no en cada punto de uso: el endpoint compartido no puede olvidarse de tomarlo. Cierre `4008`. |
| 7 | Protección DoS | ✅ | Máx. 200 conexiones con reserva atómica (`CompareAndSwapInt32`), idle timeout por `SetReadDeadline` renovado por los datos del cliente y por el latido de protocolo, `recover()` por conexión, `ReadHeaderTimeout` contra slowloris, deadline de escritura de 5 s como backpressure y apagado ordenado con `server.Shutdown`. Una conexión que calla pero contesta al ping se mantiene; a la que no contesta la cierra el idle timeout, y el número total lo acota `WS_MAX_CONNECTIONS`. |
| 8 | Logging de rechazos | ✅ | `internal/logging`: un solo sitio construye `[ACCEPT]` / `[REJECT]` / `[CLOSE]` / `[TOKEN]` / `[PETICION]`, con el `log` estándar y marca de tiempo en microsegundos. El formato es contrato con quien lee los logs, no un detalle interno. |
| 10 | Puente con el .NET 4.8 | ✅ | `internal/bridge` llama al .NET y **espera** su respuesta, con timeout obligatorio y tope de llamadas simultáneas. Lo que vuelve se valida y sanea igual que lo que entra: acaba en el DOM del navegador. Si el .NET nombra una sesión distinta de la que preguntó, la respuesta se descarta. |

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

## Varias réplicas

No hace falta nada especial. La conexión WebSocket vive en la memoria de un proceso, y todo el
ciclo ocurre ahí: la `peticion` sube por ese socket, la llamada al .NET la hace ese mismo proceso
y la respuesta baja por ese mismo socket. No hay entrega que cruzar entre pods.

Antes sí lo había: `POST /api/push` podía caer en la réplica que no tenía la sesión, y por eso
existía un bus de Redis (`WS_REDIS_ADDR`, canal `wspoc:push`). Al retirarse ese endpoint se retiró
también el bus — está en el historial de git si algún día vuelve a hacer falta.

Lo que sigue sin ser global es el tope de conexiones: `WS_MAX_CONNECTIONS` es **por proceso**, así
que con 2 pods el máximo real es el doble.

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
| Sondas de seguridad | 23/23 (handshake, bucle del canal y ciclo de `peticion`) |

Estas cifras se midieron con `tools/loadtest.js` y `tools/probe.js` del repositorio original, que no
se copiaron aquí. **No se han vuelto a medir tras la reorganización en paquetes**; lo que sí se
verificó es que el comportamiento no cambió en los casos que sobrevivieron. Las cifras de arriba
son de **antes** de retirar el puente REST: el canal no cambió, pero conviene volver a medirlas
antes de citarlas.
