# Canal WebSocket en Go — POC

Servidor WebSocket en **Go** (`net/http` + `gorilla/websocket`) pensado para reemplazar el canal en
tiempo real del backend institucional en **.NET Framework 4.5**, cuyas librerías están deprecadas y
no interoperan con clientes modernos como Angular 20.

No es un proxy hacia el 4.5: es un servidor independiente que implementa por su cuenta los ocho
controles de **[SECURITY-CHECKLIST.md](SECURITY-CHECKLIST.md)**.

El canal tiene **dos extremos**: el navegador por WebSocket, y el backend **.NET 4.8** por REST.
El sobre que se intercambia con el .NET es siempre `{sesion, payload}`, en los dos sentidos.

Este proyecto sale de un POC previo (`bg-test-back`) que comparó cuatro stacks —.NET 10, Java Spring
Boot, Python FastAPI y Go— con el mismo contrato de seguridad. **Elegido Go, aquí está solo su
lado**, extraído sin cambios de comportamiento. Las cifras y los pendientes que se citan más abajo
vienen de aquellas mediciones.

---

## Arranque rápido

```powershell
# Compila el binario y lo levanta (+ el frontend Angular con -Frontend)
.\start.ps1 -Frontend                       # http://localhost:4200

# Un token de prueba para pegar en la UI
node tools/gen-token.js

# Para pararlo todo
.\start.ps1 -Stop
```

La salida del servidor va a `logs\go.log`, **no** a una ventana de consola: una consola interactiva
de Windows *congela el proceso que escribe en ella* si alguien hace clic dentro (**QuickEdit**), y el
backend se queda mudo a mitad de un handshake — pasó durante las pruebas y cuesta un rato de
diagnosticar. Para seguirlo en vivo:

```powershell
Get-Content logs\go.log -Wait
```

Comprobación desde la línea de comandos:

```bash
node tools/probe.js       # 28 sondas de seguridad (canal + puente REST)
node tools/loadtest.js    # 120 conexiones concurrentes
```

---

## Estructura

```
backend-go/            El servidor (+ vendor/, Dockerfile)
frontend-angular/      Angular 20 — cliente con métricas, sondas y prueba de carga  → :4200
tools/                 gen-token.js · probe.js · loadtest.js · fake-dotnet.js · peticion.js
k8s/                   kind + 2 réplicas — el escenario multi-instancia
backend-go/.env        Configuración compartida por el servidor y las herramientas
SECURITY-CHECKLIST.md  El contrato que el servidor cumple
start.ps1              Compila, levanta y comprueba
logs/                  Salida de ejecución (no versionada)
```

El servidor es un solo paquete, por capas:

| Fichero | Capa |
|---|---|
| `internal/config/` | Carga de configuración. Busca el `.env` **subiendo por los directorios padre**; una variable de entorno real siempre gana al fichero. El arranque deja en el log de qué fichero salió. |
| `internal/security/` | El núcleo de seguridad: emisión y verificación de tokens, y validación de origen. Solo emite veredictos; no escribe respuestas HTTP. |
| `internal/protocol/` | El contrato: mensajes del canal, sobres del .NET, catálogo de rechazos y todo el saneado. |
| `internal/bridge/` | La llamada **síncrona** al .NET 4.8: pregunta y espera la respuesta, con timeout y tope de concurrencia. |
| `internal/transport/httpapi/` | Transporte: rutas, handshake y el bucle de lectura, una goroutine por conexión con `recover()` propio. |
| `internal/session/` | El mapa sesión → conexiones, y la **única goroutine que escribe** en cada socket. |

---

## El contrato de seguridad

| Control | Estado |
|---|---|
| §1 WSS/TLS | ⚠️ documentado (el POC corre en `ws://`) |
| §2 JWT validado **antes** del upgrade, algoritmo fijado a HS256 | ✅ |
| §3 Origin exacto contra lista blanca | ✅ |
| §4 Esquema estricto y sanitización | ✅ |
| §5 Límite de 64 KB, cortado en streaming | ✅ |
| §6 Rate limit por conexión | ✅ |
| §7 Máximo de conexiones con reserva atómica | ✅ |
| §7 Idle timeout (cierre `4014`) | ✅ |
| §7 Aislamiento de fallos por conexión | ✅ |
| §7 Backpressure (plazo de escritura de 5 s) | ✅ |
| §8 Logging uniforme de rechazos | ✅ |
| §9 Diagnóstico legible desde el navegador | ✅ |
| §10 Puente REST con el .NET 4.8 (mismos controles que el canal) | ✅ |

El detalle de cada control, con el código que lo implementa, está en
[`backend-go/README.md`](backend-go/README.md).

### Códigos de cierre

| Situación | Momento | HTTP pre-upgrade | Close code | `reason` |
|---|---|---|---|---|
| Token ausente | handshake | 401 | `4001` | `TOKEN_MISSING` |
| Token inválido (firma/formato) | handshake | 401 | `4002` | `TOKEN_INVALID` |
| Token expirado | handshake | 401 | `4003` | `TOKEN_EXPIRED` |
| Issuer/audience incorrectos | handshake | 401 | `4004` | `TOKEN_CLAIMS_INVALID` |
| Origin no permitido | handshake | 403 | `4403` | `ORIGIN_NOT_ALLOWED` |
| Límite de conexiones | handshake | 503 | `4013` | `SERVER_AT_CAPACITY` |
| Rate limit excedido | post-upgrade | — | `4008` | `RATE_LIMIT_EXCEEDED` |
| Mensaje > 64 KB | post-upgrade | — | `4009` | `MESSAGE_TOO_LARGE` |
| Mensaje inválido (esquema) | post-upgrade | — | `4010` | `INVALID_PAYLOAD` |
| Idle timeout | post-upgrade | — | `4014` | `IDLE_TIMEOUT` |

Cada rechazo deja una línea de log de formato fijo:

```
[ACCEPT] stack=go conn=<id8> remote=<ip:puerto> sub=<jwt sub> sesion=<sid> active=<n>
[REJECT] stack=go reason=<CODE> code=<4xxx> remote=<...> origin=<...> detail=<...>
[CLOSE]  stack=go conn=<id> active=<n>
[PUSH]   stack=go sesion=<sid> conns=<n> bytes=<n>
[OUTBOX] stack=go {"sesion":"<sid>","payload":"<...>"}
```

---

## Protocolo

**Cliente → servidor.** Esquema estricto: se rechaza cualquier campo extra, y también un segundo
documento JSON pegado detrás del primero.

```jsonc
{ "type": "echo" | "ping", "id": "[A-Za-z0-9_-]{1,64}", "ts": 1725700000000, "payload": "0..8192 chars" }
```

**Servidor → cliente.** El esquema estricto solo rige en el otro sentido; aquí `instance` y `sesion`
son campos legítimos: identifican qué réplica atendió y a qué sesión pertenece la conexión.

```jsonc
{ "type": "echo"|"pong"|"error"|"push", "id": "<el mismo id>", "ts": 1725700000123,
  "payload": "<eco>", "reason": "<solo en error>",
  "instance": "<pod/host>", "sesion": "<id de sesión>" }
```

`push` es el único que no responde a nada del cliente: lo empuja el .NET 4.8.

El token viaja como **query string** (`?token=<jwt>`) porque la API `WebSocket` del navegador no
admite cabeceras personalizadas, y se valida **antes** del upgrade.

### Endpoints

**`GET /health`** — sonda de salud y estado del proceso:

```json
{ "stack": "go", "status": "ok", "instance": "...", "activeConnections": 3, "maxConnections": 200 }
```

**`GET /ws`** — hace dos cosas según las cabeceras:

- **Con cabeceras de upgrade** → abre el WebSocket. El orden importa: origen → token → capacidad;
  un rechazo se responde con HTTP **antes** de abrir el socket, y la plaza se reserva de forma
  atómica (`CompareAndSwapInt32`) para que dos conexiones simultáneas no puedan superar el tope.
- **Sin ellas** → devuelve el mismo veredicto en JSON: `{"allowed":true}` o
  `{"allowed":false,"reason":"TOKEN_EXPIRED","code":4003}`.

Ese segundo modo existe porque **el navegador no expone a JavaScript el status HTTP de un handshake
rechazado**: solo entrega un `close` con `1006`. Sin el diagnóstico, la UI no puede distinguir "token
caducado" de "servidor caído". La respuesta lleva `Access-Control-Allow-Origin` con el **origen
concreto de la lista blanca, nunca `*`**.

---

## El puente con el .NET 4.8

```
navegador --WS: peticion--> Go --POST--> .NET 4.8 calcula
navegador <--WS: respuesta-- Go <--misma respuesta HTTP--
```

**Es síncrono, y es el único camino.** El .NET 4.8 no es código nuestro y no se toca, así que no
puede llamar de vuelta a Go: la única forma de que su respuesta llegue al navegador es que Go la
recoja del mismo HTTP en el que preguntó.

Hubo un camino inverso —`POST /api/push`, para que el .NET empujara sin que nadie se lo pidiera—
y se retiró: exigía que el .NET supiera llamar a Go, que es justo lo que no puede hacer. Está en
el historial de git por si algún día hay un backend que sí sepa.

El ciclo completo, con su contrato y sus modos de fallo, está en
[`backend-go/README.md`](backend-go/README.md#el-ciclo-completo-una-peticion-del-cliente-al-net-y-vuelta).

### La sesión: quién es cada usuario

Sale del claim **`sid`** del JWT (o del `sub` si no lo trae). **El cliente nunca la envía**, así que
no puede declarar la sesión de otro. Es la dirección que usa el .NET para escribirle.

```bash
node tools/gen-token.js --sid ana-1     # token para la sesión "ana-1"
```

La UI la muestra en las métricas, junto a la instancia, para poder copiarla.

**El día que exista la ruta del .NET**, se rellena `WS_DOTNET_API_URL` en `backend-go/.env` y las
`peticion` empiezan a viajar. No hay que tocar código: con la variable vacía, una `peticion`
responde `BACKEND_UNAVAILABLE` y el resto del canal sigue funcionando.

### Con varias réplicas no hace falta nada

La conexión vive en la memoria de **un** proceso, y ahí se queda todo: la `peticion` sube por ese
socket, la llamada al .NET la hace ese mismo proceso y la respuesta baja por ese mismo socket.
No hay entrega que cruzar entre réplicas, así que escalar es poner más pods y repartir conexiones.

Antes sí hacía falta un bus (Redis), porque un `POST /api/push` podía caer en el pod que no tenía
la sesión. Al retirarse ese camino, la necesidad desapareció con él.

---

## Configuración

Todo sale de `backend-go/.env`. Una variable de entorno real gana al valor del fichero, y el log
de arranque dice de qué fichero salió la configuración que está corriendo.

| Variable | Por defecto | Qué hace |
|---|---|---|
| `WS_PORT` | — | Puerto. Si falta se usa `WS_PORT_GO`. |
| `WS_PORT_GO` | `8084` | Puerto por defecto del stack. |
| `WS_BIND_ADDRESS` | `127.0.0.1` | Interfaz de escucha. En contenedor hay que ponerlo a `0.0.0.0`. |
| `WS_INSTANCE_ID` | *hostname* | Identifica la réplica; viaja en `/health` y en cada frame. |
| `WS_JWT_SECRET` | — | **Obligatorio.** Con menos de 32 bytes el proceso aborta al arrancar. |
| `WS_JWT_ISSUER` | `ws-poc-issuer` | `iss` esperado. |
| `WS_JWT_AUDIENCE` | `ws-poc-clients` | `aud` esperada. |
| `WS_ALLOWED_ORIGINS` | `http://localhost:4200` | Lista blanca de orígenes, separados por comas. |
| `WS_MAX_MESSAGE_BYTES` | `65536` | Tope de tamaño por mensaje (§5). |
| `WS_RATE_LIMIT_PER_SEC` | `20` | Mensajes por segundo y conexión (§6). |
| `WS_MAX_CONNECTIONS` | `200` | Conexiones concurrentes por **proceso** (§7). |
| `WS_IDLE_TIMEOUT_SECONDS` | `60` | Silencio tolerado antes del cierre `4014`. |
| `WS_DOTNET_API_URL` | *(vacío)* | Ruta del .NET 4.8 a la que se reenvían las `peticion`. **Vacío = no hay a quién preguntar**: la petición responde `BACKEND_UNAVAILABLE` y el resto del canal sigue funcionando. Se acepta el nombre antiguo `WS_DOTNET_WEBHOOK_URL`. |
| `WS_DOTNET_TIMEOUT_SECONDS` | `5` | Timeout de la llamada al .NET. Superarlo da `BACKEND_TIMEOUT`. |
| `WS_DOTNET_MAX_INFLIGHT` | `32` | Llamadas simultáneas al .NET. Al llenarse se rechaza con `BACKPRESSURE` en vez de encolar. |
| `WS_SESSION_TOKEN_TTL_SECONDS` | `3600` | Vigencia del token que emite `POST /api/session-token`. |
| `WS_SESSION_TOKEN_RATE_LIMIT_PER_SEC` | `20` | Tope propio del intercambio, separado del push. |

El `.env` se busca **subiendo por los directorios padre** desde el binario: si mueves `wspoc-go.exe`
fuera de `backend-go/`, deja de encontrarlo y hay que pasar la configuración por entorno.

---

## Herramientas

| Script | Para qué |
|---|---|
| `node tools/gen-token.js` | Token HS256 válido (60 min). `--kind expired\|badsig\|badiss\|badaud` genera los inválidos; `--all` los imprime todos en JSON; `--sub` y `--ttl` para ajustarlo. |
| `node tools/probe.js` | 23 sondas que comprueban que cada control rechaza lo que debe y con el código correcto: el handshake, el bucle del canal y el ciclo de `peticion`. **Sale con código 1 si alguna falla** — usable en CI. |
| `node tools/loadtest.js` | Prueba de carga independiente del navegador. |
| `node tools/fake-dotnet.js` | Hace de ruta del .NET 4.8 para el ciclo síncrono: devuelve el número con un estado. `--delay`, `--status`, `--sesion` y `--sin-sesion` fuerzan cada camino raro del puente. |
| `node tools/peticion.js` | Ejerce el ciclo completo desde el cliente: abre el canal, manda una `peticion` y espera la respuesta. `--ping` comprueba además que un fallo del .NET no cierra el socket. **Sale con código 1 si el ciclo no se cierra.** |

Todas leen el mismo `backend-go/.env` que el servidor, así que siempre hablan el mismo idioma.
Que no fuera así costó una tarde: había un segundo `.env` en la raíz con otro secreto, el servidor
leía uno y las herramientas el otro, y todo token generado fuera fallaba con `TOKEN_INVALID` sin
que nada dijera por qué. Por eso ahora el arranque registra la ruta del fichero que cargó.

```bash
node tools/loadtest.js --conns 150 --msgs 20 --rate 10
node tools/loadtest.js --conns 260 --json          # para provocar el tope
node tools/probe.js --url ws://localhost:30084/ws  # contra el Service de k8s
```

`loadtest.js` informa de: aceptadas, rechazadas **con el motivo desglosado** (lo pregunta al endpoint
de diagnóstico por cada fallo), cerradas por el servidor a mitad de prueba, mensajes perdidos,
percentiles de handshake y de RTT, duración, throughput, el reparto por instancia y un veredicto de
salud posterior — dos segundos después de la tormenta abre una conexión nueva y manda un `ping`.

La UI tiene su propio botón de *Ejecutar prueba de carga*, útil para verlo en vivo; pero Chrome y
Edge limitan las conexiones por origen y el JS compite por la CPU de la pestaña, así que las cifras
buenas son las de Node.

---

## Docker y Kubernetes

```bash
docker build -t wspoc-go:poc ./backend-go
```

El build usa **`-mod=vendor`** contra el `vendor/` versionado, no `go mod download`: el proxy
corporativo intercepta TLS con una CA que la imagen base no conoce, y bajar dependencias desde dentro
del contenedor falla. **Si añades una dependencia, ejecuta `go mod vendor` o el build de Docker se
romperá.** La imagen final es `distroless/static` y corre como `nonroot`.

Para el escenario con **2 réplicas** en kind, incluido qué se rompe al escalar y cómo reacciona el
cliente: **[k8s/README.md](k8s/README.md)**.

---

## Resultados medidos

Windows 10, `ws://` en localhost, 120 conexiones × 10 mensajes @ 10 msg/s con `tools/loadtest.js`.

| Métrica | Go |
|---|---|
| Handshake p50 / p95 | 59,5 / 62,4 ms |
| RTT p50 / p95 | 2,1 / 3,9 ms |
| Mensajes perdidos | 0 / 1200 |
| Throughput medio | 430 msg/s |
| Sondas de seguridad | **28/28** |
| 120 conexiones | 120/120 aceptadas, servidor sano después (pong en 2,2 ms) |
| 260 conexiones (tope 200) | 200 aceptadas, 60 rechazadas con `SERVER_AT_CAPACITY` |
| Idle timeout | cierre `4014` a los 5,0 s (con `WS_IDLE_TIMEOUT_SECONDS=5`) |

En la comparación original Go dio **el RTT más bajo de los cuatro stacks**. El throughput, en cambio,
no distingue nada: **lo limitaba el generador de carga**, no el servidor — 120 conexiones a 10 msg/s
dan un techo de ~1200 msg/s que ninguno llegó a rozar. Para medir el techo real haría falta una
prueba de saturación deliberada, que este POC no hizo.

Los rechazos de la prueba de 260 son **por seguridad**, no por fallo: el servidor no llegó a
degradarse en ningún momento.

---

## Pendientes

Heredados del POC, en orden de lo que más costaría descubrir tarde:

1. **`probe.js` no cubre el idle timeout (`4014`) ni el tope de conexiones (`4013`).** Ambos están
   verificados a mano, pero una regresión pasaría desapercibida con 28/28 en verde.
2. **No hay tests unitarios.** Los `Guards` concentran todo el control de seguridad y son funciones
   puras: son el objetivo natural y barato. En el frontend solo existe el spec del scaffold;
   `WebSocketService` (RTT, handshake, `diagnose`) merece cobertura con un `WebSocket` simulado.
3. **No hay CI.** `probe.js` ya sale con código 1 si algo falla, así que un workflow es casi gratis.
4. **El tope de conexiones sigue sin ser global.** `WS_MAX_CONNECTIONS` es un contador en memoria
   del proceso, así que con N réplicas el techo real es `N × tope`. Redis ya está en el proyecto para
   el enrutado del push, así que llevar ahí también el contador es ahora un paso corto.
5. **El enrutado por Redis no confirma la entrega.** Si la sesión está en otra réplica, el `POST`
   responde 202 en cuanto la publicación tiene suscriptores, sin saber si alguien la entregó de
   verdad. Confirmarlo exigiría una respuesta de vuelta por Redis.
6. **El envío al .NET no reintenta.** Si el webhook falla, el sobre se registra y se descarta.
7. **Al apagar, el cliente no sabe por qué.** Con `SIGTERM` el servidor cierra, pero el cliente ve un
   `1006` que la UI no distingue de un rechazo de handshake. Debería emitir `1001` o `1012`.
8. **El cliente no reconecta.** No hay reintento ni backoff en `WebSocketService` — y ahora importa
   más: una conexión caída deja de recibir los push del .NET sin que nadie se entere.

## Alcance y limitaciones honestas

- **Todo corre en `ws://` sobre localhost.** El TLS está documentado en
  [`backend-go/README.md`](backend-go/README.md) pero no medido; en producción el handshake será más
  lento por el apretón de manos TLS.
- **El token viaja en la query string** porque la API `WebSocket` del navegador no admite cabeceras.
  En producción hay que emitir tokens de un solo uso y vida corta, y mantener la query fuera de los
  access logs (también los del balanceador).
- **`Origin` se permite cuando la cabecera falta**, para que las pruebas desde Node funcionen. En
  producción es una línea para endurecerlo a rechazo.
- **El secreto JWT está en el `.env` versionado.** Es un secreto de prueba; en un entorno real saldría
  del repositorio y vendría de un gestor de secretos.
- **No se midió saturación real**, ni se probó reconexión, backpressure sostenido o clustering.

## Requisitos

| Herramienta | Versión probada | Cómo instalar |
|---|---|---|
| Go | 1.27.0 (mín. 1.22) | `winget install GoLang.Go` |
| Node.js | 22.18 | `winget install OpenJS.NodeJS` |

Si acabas de instalar Go, **abre una terminal nueva**: una abierta de antes no ve el `PATH`
actualizado.
