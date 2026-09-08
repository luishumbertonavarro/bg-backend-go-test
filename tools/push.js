#!/usr/bin/env node
/**
 * Simula al backend .NET 4.8 empujando un mensaje a una sesión conectada.
 *
 *   node tools/push.js --sesion demo-1 --payload "aviso desde el 4.8"
 *   node tools/push.js --sesion demo-1 --payload "x" --no-token   # prueba el 401
 *   node tools/push.js --outbox                                   # ve lo que se le enviaría al .NET
 *
 * Opciones:
 *   --sesion   sesión destino (el claim `sid` del token del cliente)
 *   --payload  texto a entregar
 *   --url      base del backend (def. http://localhost:<WS_PORT_GO>)
 *   --token    JWT a usar (def. uno válido generado al vuelo)
 *   --no-token no enviar cabecera Authorization
 *   --outbox   consultar GET /api/outbox en vez de empujar
 */
const { execFileSync } = require('node:child_process');
const path = require('node:path');
const fs = require('node:fs');
const http = require('node:http');

function loadEnv() {
  const file = path.join(__dirname, '..', '.env');
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
  if (key === 'no-token' || key === 'outbox') opts[key] = true;
  else opts[key] = argv[++i];
}

const env = loadEnv();
const base = opts.url || `http://localhost:${env.WS_PORT_GO || '8084'}`;
const token =
  opts.token ||
  execFileSync(process.execPath, [path.join(__dirname, 'gen-token.js')], { encoding: 'utf8' }).trim();

function request(method, urlPath, body) {
  return new Promise((resolve, reject) => {
    const url = new URL(base + urlPath);
    const payload = body ? Buffer.from(JSON.stringify(body)) : null;
    const headers = {};
    if (!opts['no-token']) headers.Authorization = `Bearer ${token}`;
    if (payload) {
      headers['Content-Type'] = 'application/json';
      headers['Content-Length'] = payload.length;
    }
    const req = http.request(
      { hostname: url.hostname, port: url.port, path: url.pathname, method, headers },
      (res) => {
        let data = '';
        res.on('data', (c) => (data += c));
        res.on('end', () => resolve({ status: res.statusCode, body: data }));
      },
    );
    req.on('error', reject);
    if (payload) req.write(payload);
    req.end();
  });
}

(async () => {
  let res;
  if (opts.outbox) {
    res = await request('GET', '/api/outbox');
  } else {
    if (!opts.sesion) {
      console.error('Falta --sesion. Cópiala de la UI, o genera el token con: gen-token.js --sid <sesion>');
      process.exit(2);
    }
    res = await request('POST', '/api/push', {
      sesion: opts.sesion,
      payload: opts.payload ?? 'hola desde el .NET 4.8',
    });
  }

  console.log(`HTTP ${res.status}`);
  try {
    console.log(JSON.stringify(JSON.parse(res.body), null, 2));
  } catch {
    console.log(res.body);
  }
  // 2xx -> 0; cualquier rechazo -> 1, para poder encadenarlo en scripts.
  process.exit(res.status >= 200 && res.status < 300 ? 0 : 1);
})().catch((err) => {
  console.error(err.message);
  process.exit(1);
});
