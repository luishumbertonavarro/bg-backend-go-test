# Checklist de seguridad — POC WebSocket en Go

Este documento es el **contrato** que implementa el backend: los ocho controles, los códigos
de cierre y el formato de log. Nació como contrato común de un POC que comparaba cuatro
stacks (.NET 10, Java, Python y Go); elegido Go, se conserva aquí recortado a lo que aplica.

Cualquier desviación del contrato debe quedar anotada en `backend-go/README.md`.

---

## 0. Parámetros comunes

| Parámetro | Valor por defecto | Variable de entorno |
|---|---|---|
| Puerto | `8084` | `WS_PORT`, si no `WS_PORT_GO` |
| Dirección de escucha | `127.0.0.1` | `WS_BIND_ADDRESS` |
| Ruta del endpoint | `/ws` | — |
| Ruta de salud | `/health` | — |
| Secreto JWT (HS256) | ver `.env` | `WS_JWT_SECRET` |
| Issuer esperado | `ws-poc-issuer` | `WS_JWT_ISSUER` |
| Audience esperada | `ws-poc-clients` | `WS_JWT_AUDIENCE` |
| Orígenes permitidos | `http://localhost:4200` | `WS_ALLOWED_ORIGINS` (coma-separados) |
| Tamaño máx. de mensaje | `65536` bytes (64 KB) | `WS_MAX_MESSAGE_BYTES` |
| Rate limit | `20` msg/seg por conexión | `WS_RATE_LIMIT_PER_SEC` |
| Conexiones concurrentes máx. | `200` | `WS_MAX_CONNECTIONS` |
| Idle timeout | `60` segundos | `WS_IDLE_TIMEOUT_SECONDS` |

El servidor y las herramientas de `tools/` leen la misma `.env` de la raíz del repositorio.
Una variable de entorno real siempre gana al valor del fichero.

---

## 1. WSS / TLS

En el POC el servidor escucha en `ws://localhost:8084` (texto plano) para evitar la fricción
de certificados autofirmados en el navegador, que impediría medir el handshake.
`backend-go/README.md` documenta la configuración exacta de TLS para producción, tanto con
`ListenAndServeTLS` como detrás de un reverse proxy.

## 2. Autenticación por JWT en el handshake

- Algoritmo **HS256**, secreto simétrico compartido (`WS_JWT_SECRET`).
- Claims obligatorios y verificados: `sub`, `iss`, `aud`, `exp`, `iat`.
- El token viaja como **query string** `?token=<jwt>`: la API `WebSocket` del navegador no
  permite enviar cabeceras `Authorization` personalizadas. En producción esto se mitiga con
  tokens de un solo uso y vida corta (ver README de cada backend).
- **La validación ocurre ANTES de aceptar el upgrade.** Si falla, el servidor responde con un
  status HTTP (401) y nunca abre el WebSocket.

## 3. Verificación de Origin

- Se compara la cabecera `Origin` contra `WS_ALLOWED_ORIGINS`.
- Comparación exacta de cadena (esquema + host + puerto). Sin comodines.
- Rechazo **antes del upgrade** con HTTP 403.
- Nota: `Origin` solo lo envían los navegadores; un cliente nativo puede falsificarlo. Es un
  control anti-CSWSH (Cross-Site WebSocket Hijacking), no un sustituto de la autenticación.

## 4. Validación y sanitización de mensajes

Todo frame entrante debe ser JSON que cumpla este esquema:

```jsonc
{
  "type": "echo" | "ping",   // enum cerrado, obligatorio
  "id":   "string",          // obligatorio, 1..64 chars, [A-Za-z0-9_-]
  "ts":   1234567890123,     // obligatorio, entero epoch ms
  "payload": "string"        // obligatorio para "echo", 0..8192 chars
}
```

Reglas de rechazo:

- JSON malformado.
- Campo faltante, tipo incorrecto o `type` fuera del enum.
- Campos desconocidos adicionales (esquema estricto).
- `payload` con caracteres de control (U+0000–U+001F salvo tab, LF y CR).
- `payload` que contenga patrones de script: `<script`, `javascript:`, `onerror=`, `onload=`.
- El servidor **nunca** reenvía el payload sin escapar; el eco lo devuelve como texto dentro
  de un campo JSON, y el frontend lo renderiza como texto, jamás como HTML.

Respuesta del servidor:

```jsonc
{ "type": "echo" | "pong" | "error", "id": "<mismo id>", "ts": <epoch ms del servidor>,
  "payload": "<eco>", "reason": "<solo en error>" }
```

## 5. Límite de tamaño de mensaje

- Máximo **64 KB** por frame, aplicado por el propio servidor WebSocket (no solo en la capa de
  aplicación), de modo que el buffer nunca crezca sin control.
- Al excederse: cerrar la conexión con código `4009`.

## 6. Rate limiting por conexión

- Ventana deslizante de 1 segundo, máximo **20 mensajes**.
- Al excederse: cerrar la conexión con código `4008`.
- El contador es **por conexión**, no global, para que una conexión abusiva no afecte a otras.

## 7. Protección básica contra DoS

- **Máximo de conexiones concurrentes** global (`200`). Al superarse, rechazo pre-upgrade con
  HTTP 503 (código lógico `4013`).
- **Idle timeout**: si no llega ningún frame en 60 s, cerrar con código `4014`. Se implementa
  con ping/pong de protocolo más un temporizador de inactividad.
- **Aislamiento de fallos**: la gestión de cada conexión va envuelta en manejo de errores; una
  excepción en un socket cierra únicamente ese socket y nunca propaga al aceptador.
- **Backpressure**: si el buffer de salida de un cliente se llena, se cierra esa conexión en
  lugar de acumular memoria.

## 8. Logging de rechazos

Cada rechazo se registra en consola en una sola línea con formato uniforme, para poder
compararlo con un `diff` entre ejecuciones:

```
[REJECT] stack=go reason=<CODE> code=<4xxx|HTTP> remote=<ip:puerto> origin=<origin> detail=<texto>
```

Y cada conexión aceptada:

```
[ACCEPT] stack=go conn=<id> remote=<ip:puerto> sub=<jwt sub> active=<n>
```

---

## 9. Tabla de códigos

| Situación | Momento | HTTP pre-upgrade | Close code | `reason` |
|---|---|---|---|---|
| Token ausente | handshake | 401 | 4001 | `TOKEN_MISSING` |
| Token inválido (firma/formato) | handshake | 401 | 4002 | `TOKEN_INVALID` |
| Token expirado | handshake | 401 | 4003 | `TOKEN_EXPIRED` |
| Issuer/audience incorrectos | handshake | 401 | 4004 | `TOKEN_CLAIMS_INVALID` |
| Origin no permitido | handshake | 403 | 4403 | `ORIGIN_NOT_ALLOWED` |
| Límite de conexiones | handshake | 503 | 4013 | `SERVER_AT_CAPACITY` |
| Rate limit excedido | post-upgrade | — | 4008 | `RATE_LIMIT_EXCEEDED` |
| Mensaje > 64 KB | post-upgrade | — | 4009 | `MESSAGE_TOO_LARGE` |
| Mensaje inválido (esquema) | post-upgrade | — | 4010 | `INVALID_PAYLOAD` |
| Idle timeout | post-upgrade | — | 4014 | `IDLE_TIMEOUT` |

### Diagnóstico legible desde el navegador

Un rechazo pre-upgrade devuelve un status HTTP que **el navegador no expone a JavaScript**: la
API `WebSocket` solo entrega un `close` con código `1006`. Para que la UI pueda mostrar el
motivo real, el servidor acepta una petición **GET normal (sin cabeceras de upgrade) a `/ws`**
y responde con el mismo veredicto en JSON:

```jsonc
// 200 OK
{ "allowed": true }
// 401 / 403 / 503
{ "allowed": false, "reason": "TOKEN_EXPIRED", "code": 4003 }
```

El frontend hace esta consulta solo cuando la conexión falla, para etiquetar el rechazo. Es un
endpoint de diagnóstico del POC; en producción no se expondría.

---

## 10. Puente REST con el backend .NET 4.8

El canal tiene dos extremos: el navegador y el backend **.NET Framework 4.8**. El sobre que se
intercambia con él es siempre el mismo, en los dos sentidos:

```jsonc
{ "sesion": "<id de sesión>", "payload": "<texto>" }
```

### La sesión

Sale del claim **`sid`** del JWT ya validado en el handshake; si el token no lo trae, del `sub`.
**El cliente nunca la envía**, así que no puede declarar la sesión de otro. Debe cumplir
`[A-Za-z0-9_-]{1,64}`, igual que el `id` de los mensajes: viaja en logs y en respuestas JSON.

Una misma sesión puede tener varias conexiones (varias pestañas). La entrega va a todas.

### `POST /api/push` — el .NET empuja a un usuario

- **Autenticación:** JWT HS256 en `Authorization: Bearer <token>`, con la **misma validación**
  que el handshake (§2). El .NET no es un navegador, así que sí puede enviar cabeceras y no
  necesita el token en la query string.
- **Cuerpo:** esquema estricto, sin campos extra, y la **misma sanitización** que §4 — control
  de caracteres, patrones de script y tope de 8192 en el `payload`. El push acaba en el DOM
  igual que un eco, así que no puede ser un camino más laxo.
- **Rate limit:** `WS_PUSH_RATE_LIMIT_PER_SEC` (100/s por defecto), global del endpoint.

| Situación | HTTP | `reason` |
|---|---|---|
| Entregado | 202 | — (devuelve `delivered`, `sesion`, `instance`) |
| Token ausente/inválido/expirado | 401 | `TOKEN_*`, igual que §9 |
| Esquema o payload inválido | 400 | `INVALID_PAYLOAD` |
| Sesión mal formada | 400 | `INVALID_SESSION` |
| Sesión no conectada | 404 | `SESSION_NOT_FOUND` |
| Cuerpo > 64 KB | 413 | `MESSAGE_TOO_LARGE` |
| Rate limit | 429 | `RATE_LIMIT_EXCEEDED` |

Al cliente le llega un frame normal del canal con `"type": "push"`.

### `GET /api/outbox` — lo que se le enviaría al .NET

Cada mensaje válido del cliente produce un sobre. Con `WS_DOTNET_WEBHOOK_URL` configurada se
envía por `POST` a esa URL; **mientras esté vacía**, el sobre se guarda en memoria (buffer
circular) y se registra como `[OUTBOX]` en el log, para poder inspeccionar el contrato exacto
sin tener el .NET levantado. Este endpoint lo devuelve, con la misma autenticación que el push.

### CORS

Estas dos rutas llevan cabecera `Authorization`, así que el navegador manda antes un **preflight
`OPTIONS`**. Se responde con `Access-Control-Allow-Methods` y `Access-Control-Allow-Headers`,
y el `Allow-Origin` es siempre el **origen concreto de la lista blanca, nunca `*`**. Sin el
preflight, la UI ve un fallo de red indistinguible de un servidor caído.

### Límite conocido: el tope y la entrega son por proceso

La conexión vive en la memoria de un solo proceso. Con varias réplicas, un `POST` que caiga en el
pod que no tiene la sesión no entregaría nada. Se resuelve publicando el push en **Redis**
(`WS_REDIS_ADDR`), al que están suscritas todas las réplicas. Con la variable vacía la entrega es
local, que es correcto con una sola réplica.
