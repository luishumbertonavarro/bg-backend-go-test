#!/usr/bin/env node
/**
 * Ejerce el ciclo completo desde el lado del cliente: abre el WebSocket, manda
 * una `peticion` y espera la respuesta que el .NET devolvio a traves de Go.
 *
 *   node tools/peticion.js
 *   node tools/peticion.js --payload '{"operacion":"sumar","a":2,"b":3}'
 *   node tools/peticion.js --sesion 12345
 *   node tools/peticion.js --ping     # tras la peticion, comprueba que el canal sigue vivo
 *
 * Opciones:
 *   --sesion   SESION a usar (def. un numero, que es lo que genera el frontend)
 *   --payload  JSON a enviar (def. un objeto de ejemplo)
 *   --url      base del WebSocket (def. ws://localhost:<WS_PORT_GO>/ws)
 *   --espera   ms a esperar la respuesta (def. 10000)
 *   --ping     manda un ping despues, para verificar que un fallo no cerro el canal
 *
 * Codigo de salida 0 si llega una `respuesta`, 1 si llega un `error` o no llega
 * nada: encadenable en scripts.
 */
const { execFileSync } = require('node:child_process');
const path = require('node:path');
const fs = require('node:fs');
const WebSocket = require('ws');

function loadEnv() {
  const file = path.join(__dirname, '..', 'backend-go', '.env');
  const env = {};
  if (fs.existsSync(file)) {
    for (const line of fs.readFileSync(file, 'utf8').split(/\r?\n/)) {
      const m = /^\s*([A-Z0-9_]+)\s*=\s*(.*)\s*$/.exec(line);
      if (m) env[m[1]] = m[2];
    }
  }
  return { ...env, ...process.env };
}

const argv = process.argv.slice(2);
const opts = {};
for (let i = 0; i < argv.length; i++) {
  if (!argv[i].startsWith('--')) continue;
  const key = argv[i].slice(2);
  if (key === 'ping') opts.ping = true;
  else opts[key] = argv[++i];
}

const env = loadEnv();
// El frontend genera un numero, no un uuid: se imita aqui para probar lo mismo
// que va a pasar en produccion.
const sesion = opts.sesion || String(Date.now() % 100000000);
const base = opts.url || `ws://localhost:${env.WS_PORT_GO || '8084'}/ws`;
const espera = Number(opts.espera || 10000);
const payload = opts.payload || '{"operacion":"ejemplo","valor":42}';

const token = execFileSync(
  process.execPath,
  [path.join(__dirname, 'gen-token.js'), '--sid', sesion],
  { encoding: 'utf8' },
).trim();

const ws = new WebSocket(`${base}?token=${token}`, { origin: 'http://localhost:4200' });
const id = 'p-' + Math.random().toString(36).slice(2, 8);
let resuelto = false;

const salir = (code) => {
  if (resuelto) return;
  resuelto = true;
  try { ws.close(); } catch { /* ya cerrado */ }
  process.exit(code);
};

const temporizador = setTimeout(() => {
  console.error(`Sin respuesta tras ${espera} ms.`);
  salir(1);
}, espera);

ws.on('open', () => {
  console.log(`Conectado. SESION=${sesion}`);
  const msg = { type: 'peticion', id, ts: Date.now(), payload: JSON.parse(payload) };
  console.log('->', JSON.stringify(msg));
  ws.send(JSON.stringify(msg));
});

ws.on('message', (data) => {
  const m = JSON.parse(data);
  console.log('<-', JSON.stringify(m));

  if (m.id !== id) return; // no es lo que estabamos esperando

  clearTimeout(temporizador);

  if (m.type === 'error') {
    console.error(`La peticion fallo: ${m.reason}`);
    if (!opts.ping) return salir(1);
    // Un fallo del .NET NO debe cerrar el canal: se comprueba mandando un ping.
    console.log('Comprobando que el canal sigue vivo...');
    ws.send(JSON.stringify({ type: 'ping', id: 'tras-error', ts: Date.now() }));
    setTimeout(() => {
      console.error('El canal no respondio al ping: se cerro cuando no debia.');
      salir(1);
    }, 2000);
    return;
  }

  if (m.type === 'respuesta') {
    console.log('OK: respuesta del .NET recibida por el mismo socket, con el id de la peticion.');
    return salir(0);
  }
});

ws.on('message', (data) => {
  const m = JSON.parse(data);
  if (m.id === 'tras-error' && m.type === 'pong') {
    console.log('OK: el canal sigue vivo tras el fallo del .NET.');
    salir(0);
  }
});

ws.on('close', (code) => {
  if (!resuelto) {
    console.error(`El socket se cerro (${code}) antes de responder.`);
    salir(1);
  }
});

ws.on('error', (err) => {
  console.error(err.message);
  salir(1);
});
