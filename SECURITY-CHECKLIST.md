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

## 10. La sesión

Sale del claim **`sid`** del JWT ya validado en el handshake; si el token no lo trae, del `SESION`
y, en último término, del `sub`. **El cliente nunca la envía por el canal**, así que no puede
declarar la sesión de otro. Debe cumplir `[A-Za-z0-9_-]{1,64}`, igual que el `id` de los
mensajes: viaja en logs y en respuestas JSON.

Una misma sesión puede tener varias conexiones (varias pestañas). Lo que se entrega por sesión
—la respuesta del .NET— va a **todas**. El eco y el pong, en cambio, van solo al socket que
preguntó.

### Lo que se retiró

Existían `POST /api/push` y `GET /api/outbox`: un puente REST para que el .NET empujara mensajes
a una sesión y recogiera lo que el cliente enviaba. Se retiraron porque exigían que el .NET 4.8
llamara a Go, y ese backend no se toca.

Con ellos desapareció la única razón por la que `WS_JWT_SECRET` tenía que compartirse con el
.NET: **hoy el secreto no sale del servidor Go**. Y con ellos desapareció también la necesidad
de un bus entre réplicas (Redis), porque ya no hay ninguna entrega que cruzar de un proceso a
otro: la petición sube por un socket, la llamada al .NET la hace ese mismo proceso y la respuesta
baja por ese mismo socket.

## 11. Puente sincrono con el .NET 4.8 (`type: "peticion"`)

Go reenvia la peticion del cliente a `WS_DOTNET_API_URL` y **espera** la respuesta, que devuelve
por el mismo socket. Los controles:

| Control | Como |
|---|---|
| Saneado de la ida | El `payload` del cliente pasa por un esquema cerrado, sin patrones de script y dentro del tope de tamano. |
| Saneado de la vuelta | Lo que responde el .NET **tambien** se valida y sanea. Acaba en el DOM del navegador, asi que no puede entrar por una puerta mas laxa que el resto. |
| Tope de tamano | La respuesta se corta en streaming a `WS_MAX_MESSAGE_BYTES`; un .NET averiado no puede tumbar el proceso mandando un cuerpo gigante. |
| Timeout | `WS_DOTNET_TIMEOUT_SECONDS` siempre, en el cliente HTTP y en el contexto. Un .NET colgado no retiene una goroutine para siempre. |
| Tope de concurrencia | `WS_DOTNET_MAX_INFLIGHT` llamadas simultaneas. Al llenarse se rechaza con `BACKEND_BUSY` en vez de encolar sin limite. |
| Enrutado de la respuesta | Si el .NET devuelve una `Sesion` **distinta** de la que pregunto, la respuesta se **descarta** y se registra `BACKEND_SESSION_MISMATCH`. Obedecerla entregaria la respuesta de un usuario a otro. |
| El fallo no cierra el canal | Los cuatro motivos (`BACKEND_UNAVAILABLE`, `BACKEND_TIMEOUT`, `BACKEND_ERROR`, `BACKEND_BUSY`) llegan como frame `error` con el `id` de la peticion, sin cerrar el socket. |

## 12. Riesgo conocido y aceptado: la SESION es un numero

`POST /api/session-token` emite el token del canal a **cualquiera** que presente una cadena con
forma de sesion desde un Origin de la lista blanca. Como la SESION la genera el frontend y es un
**numero**, no hay nada que adivinar: probar `1`, `2`, `3`... basta para intentar quedarse con el
canal de otro usuario.

**Lo que hay puesto:**

- **`SESSION_IN_USE` (409).** No se emite un segundo token para una SESION que ya tiene conexion
  viva. Cierra el caso realista —robar una sesion mientras su dueno la usa— y no rompe la
  reconexion, porque una conexion caida se desregistra y la sesion vuelve a quedar libre.
- **Rate limit propio** del endpoint (`WS_SESSION_TOKEN_RATE_LIMIT_PER_SEC`), separado del push.
- **Traza de cada emision** con la SESION y el remoto (`[TOKEN]` en el log), para que un intento
  de enumeracion se vea.

**Lo que NO cierra:** reclamar una SESION **antes** que su dueno. Para eso el numero tendria que
llevar entropia (uno aleatorio grande en vez de un correlativo), generarlo Go, o validarlo el
.NET. Las tres cosas cambian el contrato del login, asi que es una decision a tomar **antes de
produccion**, no un olvido.
