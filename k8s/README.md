# El backend Go en 2 pods — escenario multi-instancia

Despliegue local del backend con **2 réplicas**, para observar cómo se comporta el cliente Angular
cuando ya no hay un único proceso al otro lado.

```
kind-cluster.yaml   Cluster de un nodo, con el NodePort mapeado al host
common.yaml         Namespace + ConfigMap + Secret
redis.yaml          Redis (1 réplica) — bus de push entre réplicas
go.yaml             Deployment (2 réplicas) + Service NodePort 30084
```

| | Local (`start.ps1`) | Kubernetes |
|---|---|---|
| Go | `ws://localhost:8084/ws` | `ws://localhost:30084/ws` |

---

## Arranque

```powershell
# 1. Cluster (una sola vez)
kind create cluster --config k8s/kind-cluster.yaml

# 2. Imagen. No hay registry: se carga directamente en el nodo.
docker build -t wspoc-go:poc ./backend-go
kind load docker-image wspoc-go:poc --name wspoc

# 3. Desplegar. Redis ANTES que el backend: sin el, el enrutado del push
#    entre replicas no funciona (go.yaml apunta a wspoc-redis:6379).
kubectl apply -f k8s/common.yaml
kubectl apply -f k8s/redis.yaml
kubectl apply -f k8s/go.yaml
kubectl -n wspoc get pods

# Tras cambiar codigo: reconstruir, recargar y reiniciar
docker build -t wspoc-go:poc ./backend-go
kind load docker-image wspoc-go:poc --name wspoc
kubectl -n wspoc rollout restart deploy/wspoc-go
```

Para borrarlo todo: `kind delete cluster --name wspoc`.

La UI sigue fuera del clúster (`.\start.ps1 -Frontend`), en `http://localhost:4200`. Como el origen
del navegador no cambia, la whitelist de `WS_ALLOWED_ORIGINS` vale tal cual. En el panel, sustituye
la URL por `ws://localhost:30084/ws`.

---

## Dos cosas que hubo que cambiar en el código

1. **`WS_BIND_ADDRESS`.** El servidor escuchaba solo en `127.0.0.1`, escrito literal en el código.
   Dentro de un contenedor eso lo hace inalcanzable. Ahora es configurable, con default
   **`127.0.0.1`**: solo los manifiestos de aquí lo ponen a `0.0.0.0`, para no exponer sin querer un
   servidor de desarrollo al arrancarlo en local.

2. **`WS_INSTANCE_ID`.** Sin esto no había forma de saber qué réplica atendió una conexión. Se
   inyecta el nombre del pod (`fieldRef: metadata.name`) y viaja en `/health` y en cada frame
   servidor→cliente. Es seguro: el esquema estricto de §4 solo rige en sentido cliente→servidor.

El `.env` no entra en la imagen; toda la configuración va por variables de entorno, que en este POC
tienen prioridad sobre el fichero. **`WS_JWT_SECRET` es obligatorio**: sin él, o con menos de 32
bytes, el proceso aborta al arrancar.

**Descarga de dependencias detrás del proxy corporativo.** El proxy intercepta TLS con una CA que la
imagen base no conoce, así que bajar dependencias *desde dentro* del contenedor falla. El
`Dockerfile` lo esquiva compilando contra **`vendor/`** (`go mod vendor` en el host, con el
directorio versionado). Si añades una dependencia, vuelve a ejecutarlo o el build de Docker fallará.

---

## Resultados medidos

Todos los controles siguen en pie tras el despliegue: **`probe.js` da 17/17** contra el Service.

```powershell
node tools/probe.js --url ws://localhost:30084/ws
```

### El push entre réplicas

Con 2 pods, la conexión de un usuario vive en **uno solo**. Un `POST /api/push` al Service puede
caer en el otro, que no la tiene. Por eso `go.yaml` define `WS_REDIS_ADDR`: el push se publica en
Redis, ambas réplicas están suscritas y entrega la que tiene la conexión.

```powershell
# Conectar la UI al NodePort con una sesión conocida y empujar repetidamente:
# debe llegar SIEMPRE, caiga el POST en el pod que caiga.
node tools/gen-token.js --sid ana-1
node tools/push.js --url http://localhost:30084 --sesion ana-1 --payload "hola"
```

Quitando `WS_REDIS_ADDR` del Deployment, aproximadamente **la mitad de los push devuelven 404**:
es la demostración de por qué hace falta el bus.

### 1. El reparto entre réplicas funciona

120 conexiones contra el Service. `loadtest.js` informa del reparto por instancia
(`connectionsByInstance`, leído del campo `instance` de cada frame).

| | Go, 2 réplicas |
|---|---|
| Aceptadas | 120/120 |
| Mensajes perdidos | 0 |
| Reparto entre los 2 pods | 57 / 63 |
| Handshake p50 | 228 ms |
| RTT p50 | 6,8 ms |
| RTT p95 | 12,8 ms |

El handshake sube respecto a localhost (59,5 ms) por el salto extra del NodePort. El techo lo sigue
poniendo el generador de carga, no el servidor.

Aviso metodológico: encadenar pruebas de carga **sin reposo** produce rechazos espurios
(`UNREACHABLE`, `HTTP_200`) por contención del entorno, no por fallo del backend. Cualquier medición
aquí necesita pausas de ~20 s entre tandas.

### 2. El tope de conexiones deja de ser global

Esto **no** lo resuelve Redis, que hoy solo transporta los push:

`WS_MAX_CONNECTIONS` es un contador **en memoria de cada proceso** (`ConnectionRegistry` en
`backend-go/guards.go`). Con N réplicas el techo real del servicio es `N × tope`.

Con el tope bajado a **5** y 12 conexiones: **entraron 10**, repartidas 5 + 5. El tope declarado de
5 dejó pasar el doble.

Contraste con `sessionAffinity: ClientIP`: **5 de 12 aceptadas**, todas en el mismo pod. El número
vuelve a cuadrar, pero deja **la mitad de la capacidad ociosa** — y desde un navegador todas las
conexiones comparten IP de origen, así que ese modo concentra todo en un pod.

`4013 SERVER_AT_CAPACITY` deja de ser una propiedad del servicio y pasa a depender de a qué pod te
mandó el balanceador. Un tope real exige estado compartido — y ahora que Redis ya está desplegado
para el push, llevar ahí también el contador es un paso corto.

Efecto secundario: **`/health` solo informa del pod que atendió esa petición**, no del servicio. El
`activeConnections` que devuelve es parcial y no reproducible.

### 3. Al caer un pod, el cliente no sabe por qué

Con una conexión viva, `kubectl delete pod <el que atiende>`: el cliente ve **`1006`** a los ~3,7 s.
El manejador de `SIGTERM` (`backend-go/main.go`) cierra el servidor, pero no emite un frame de
cierre con motivo.

`1006` es indistinguible, desde la UI, de un rechazo de handshake, y se trata como una desconexión
sin motivo. Lo correcto sería **`1001` (going away)** o **`1012` (service restart)**, que le dicen al
cliente que reconecte. Queda como pendiente.

**Y el cliente no reintenta en ningún caso.** No hay reconexión ni backoff en `WebSocketService`, así
que cualquier rolling update deja a todos los usuarios desconectados hasta que pulsen Conectar.

---

## Aviso sobre Ingress

Aquí se usa **NodePort**, sin proxy L7 en medio, así que nada compite con el idle timeout y el cierre
`4014` se conserva intacto.

En cuanto se ponga un Ingress delante, cualquier `proxy-read-timeout` menor que
`WS_IDLE_TIMEOUT_SECONDS` ganará la carrera y el cliente verá `1006` en vez de `4014`. Es
exactamente el mismo fallo que en el POC original hubo que corregir dos veces *dentro* de los
servidores.

Y el token viaja en query string (`?token=`), así que ahora aparecerá también en los access logs del
balanceador.
