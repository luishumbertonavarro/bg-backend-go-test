# Frontend — Angular 20

Cliente que prueba el backend Go, con métricas en vivo y prueba de carga desde el navegador.

- **Puerto del dev server:** `4200` — es también el origen que el backend tiene en su lista blanca
  (`WS_ALLOWED_ORIGINS`).
- **Requisitos:** Node 20+ (probado con 22.18).

## Levantar

```bash
cd frontend-angular
npm install
npm start          # http://localhost:4200
```

Necesitas el backend levantado (ver `../start.ps1`) y un token JWT:

```bash
node ../tools/gen-token.js          # token válido, 60 minutos
node ../tools/gen-token.js --all    # también los inválidos, para probar los rechazos
```

Pega el token en el panel y pulsa **Conectar**.

## Qué hay dentro

| Fichero | Qué hace |
|---|---|
| `src/app/core/ws.models.ts` | Contrato compartido con los backends: esquema de mensajes, códigos de cierre, forma de las métricas. |
| `src/app/core/websocket.service.ts` | El servicio reutilizable: `connect()`, `disconnect()`, `send()`, `ping()`, observables de mensajes, métricas, estado y cierres. Instrumenta handshake y RTT. |
| `src/app/core/load-test.service.ts` | Prueba de carga desde el navegador: N conexiones concurrentes, percentiles y comprobación de salud posterior. |
| `src/app/core/backends.ts` | El backend y su URL por defecto. |
| `src/app/backend-panel/` | El panel: conexión, métricas, eco, sondas de seguridad, carga y log. |

### El servicio de WebSocket

No es `providedIn: 'root'`. Cada panel lo declara en sus `providers`, así que cada pestaña mantiene
conexión y métricas propias. Los paneles se quedan montados al cambiar de pestaña (se ocultan con
CSS), de modo que una conexión abierta sobrevive mientras miras otra cosa. La estructura de pestañas
se conserva del POC comparativo: hoy solo hay una, la de Go.

```ts
const ws = inject(WebSocketService);          // providers: [WebSocketService]

ws.connect({ url: 'ws://localhost:8084/ws', token });
ws.messages$.subscribe(msg => /* ... */);
ws.metrics$.subscribe(m => /* handshakeMs, avgRttMs, p95RttMs, msgsPerSec ... */);
ws.send('hola');
ws.disconnect();
```

También expone `status()` y `metrics()` como signals, para consumirlos directamente en plantillas.

### Por qué hay un endpoint de diagnóstico

Cuando un backend rechaza el handshake (token inválido, origen no permitido, servidor lleno), el
navegador **no** expone el status HTTP a JavaScript: solo entrega un `close` con código `1006`. Para
poder mostrar el motivo real, el servicio consulta `GET /ws` sin cabeceras de upgrade, y el backend
devuelve ahí el veredicto en JSON. Es un endpoint del POC; en producción no se expondría.

### Sondas de seguridad

Los cuatro botones de *Sondas de seguridad* violan un control a propósito (rate limit, tamaño,
`<script>`, JSON roto). Sirven para ver en vivo cómo responde el servidor: qué código de cierre
devuelve, qué registra en consola y si sigue atendiendo al resto de conexiones.

### Sobre la prueba de carga del navegador

Chrome y Edge limitan las conexiones simultáneas por origen, y el propio JavaScript compite por la
CPU de la pestaña. Los números del navegador sirven para verlo en vivo, no como medida definitiva:
las cifras de referencia salen de `node ../tools/loadtest.js`, que corre fuera del navegador.

## Comandos

```bash
npm start          # dev server en :4200
npm run build      # build de producción en dist/
npm test           # tests unitarios (Karma)
```
