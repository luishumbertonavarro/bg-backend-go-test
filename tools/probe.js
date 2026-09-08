#!/usr/bin/env node
/**
 * Batería de sondas de seguridad contra un backend. Comprueba que cada control del
 * SECURITY-CHECKLIST.md rechaza lo que debe rechazar y con el código correcto.
 *
 *   node tools/probe.js
 *   node tools/probe.js --url ws://localhost:8084/ws
 *
 * Salida: una línea por sonda con OK / FALLO y el código de cierre observado.
 * Código de salida 1 si alguna sonda falla — usable en CI.
 */
const { execFileSync } = require('node:child_process');
const path = require('node:path');
const fs = require('node:fs');
const http = require('node:http');
const WebSocket = require('ws');

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
  if (argv[i].startsWith('--')) opts[argv[i].slice(2)] = argv[++i];
}

const env = loadEnv();
const stack = 'go';
const URL_WS = opts.url || `ws://localhost:${env.WS_PORT_GO || '8084'}/ws`;
const ORIGIN = 'http://localhost:4200';

const token = (kind) =>
  execFileSync(process.execPath, [path.join(__dirname, 'gen-token.js'), '--kind', kind], {
    encoding: 'utf8',
  }).trim();

const TOKENS = {
  valid: token('valid'),
  expired: token('expired'),
  badsig: token('badsig'),
  badiss: token('badiss'),
};

function diag(tok, origin = ORIGIN) {
  return new Promise((resolve) => {
    const u = new global.URL(URL_WS.replace(/^ws/, 'http'));
    if (tok) u.searchParams.set('token', tok);
    const req = http.get(u, { headers: origin ? { Origin: origin } : {} }, (res) => {
      let body = '';
      res.on('data', (c) => (body += c));
      res.on('end', () => {
        let j = {};
        try { j = JSON.parse(body); } catch { /* respuesta no JSON */ }
        resolve({ status: res.statusCode, ...j });
      });
    });
    req.on('error', (e) => resolve({ status: 0, error: String(e.message) }));
  });
}

/** Abre una conexión, ejecuta `action` y resuelve con el código de cierre. */
function session(tok, action, { origin = ORIGIN, waitMs = 3000 } = {}) {
  return new Promise((resolve) => {
    const u = `${URL_WS}?token=${encodeURIComponent(tok)}`;
    const ws = new WebSocket(u, { origin, maxPayload: 1 << 21 });
    const received = [];
    let opened = false;
    const to = setTimeout(() => { try { ws.close(); } catch {} resolve({ opened, code: null, received, timeout: true }); }, waitMs);
    ws.on('open', () => { opened = true; action(ws); });
    ws.on('message', (b) => { try { received.push(JSON.parse(b.toString())); } catch { received.push({ raw: b.toString() }); } });
    ws.on('close', (code) => { clearTimeout(to); resolve({ opened, code, received }); });
    ws.on('error', () => { /* el close trae el veredicto */ });
  });
}

/** Llama al REST del puente con el .NET 4.8. */
function rest(method, urlPath, body, tok) {
  return new Promise((resolve) => {
    const u = new global.URL(URL_WS.replace(/^ws/, 'http'));
    const payload = body ? Buffer.from(JSON.stringify(body)) : null;
    const headers = {};
    if (tok) headers.Authorization = `Bearer ${tok}`;
    if (payload) {
      headers['Content-Type'] = 'application/json';
      headers['Content-Length'] = payload.length;
    }
    const req = http.request(
      { hostname: u.hostname, port: u.port, path: urlPath, method, headers },
      (res) => {
        let data = '';
        res.on('data', (c) => (data += c));
        res.on('end', () => {
          let j = {};
          try { j = JSON.parse(data); } catch { /* respuesta no JSON */ }
          resolve({ status: res.statusCode, ...j });
        });
      },
    );
    req.on('error', (e) => resolve({ status: 0, error: String(e.message) }));
    if (payload) req.write(payload);
    req.end();
  });
}

/** Abre una conexión con una sesión conocida y la deja viva para recibir push. */
function openSession(sid) {
  const tok = execFileSync(
    process.execPath,
    [path.join(__dirname, 'gen-token.js'), '--sid', sid],
    { encoding: 'utf8' },
  ).trim();
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(`${URL_WS}?token=${encodeURIComponent(tok)}`, { origin: ORIGIN });
    const received = [];
    ws.on('message', (b) => { try { received.push(JSON.parse(b.toString())); } catch {} });
    ws.on('open', () => resolve({ ws, received, token: tok }));
    ws.on('error', reject);
  });
}

const results = [];
const check = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log(`${pass ? ' OK ' : 'FALLO'}  ${name.padEnd(42)} ${detail}`);
};

(async () => {
  console.log(`\n=== Sondas de seguridad :: ${stack} :: ${URL_WS}\n`);

  // --- Handshake ----------------------------------------------------------
  let d = await diag(TOKENS.valid);
  check('Token válido aceptado', d.allowed === true, `HTTP ${d.status}`);

  d = await diag('');
  check('Token ausente rechazado (4001)', d.reason === 'TOKEN_MISSING' && d.status === 401, `HTTP ${d.status} ${d.reason}`);

  d = await diag(TOKENS.expired);
  check('Token expirado rechazado (4003)', d.reason === 'TOKEN_EXPIRED' && d.status === 401, `HTTP ${d.status} ${d.reason}`);

  d = await diag(TOKENS.badsig);
  check('Firma inválida rechazada (4002)', d.reason === 'TOKEN_INVALID' && d.status === 401, `HTTP ${d.status} ${d.reason}`);

  d = await diag(TOKENS.badiss);
  check('Issuer inválido rechazado (4004)', d.reason === 'TOKEN_CLAIMS_INVALID' && d.status === 401, `HTTP ${d.status} ${d.reason}`);

  d = await diag(TOKENS.valid, 'http://evil.test');
  check('Origin no permitido rechazado (4403)', d.reason === 'ORIGIN_NOT_ALLOWED' && d.status === 403, `HTTP ${d.status} ${d.reason}`);

  // El upgrade real con token inválido tampoco debe abrirse.
  let s = await session(TOKENS.expired, () => {});
  check('Upgrade con token expirado no abre', s.opened === false, `opened=${s.opened} code=${s.code}`);

  // --- Funcionamiento normal ---------------------------------------------
  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'echo', id: 'p1', ts: Date.now(), payload: 'hola' })),
  );
  check('Eco correcto', s.received.some((m) => m.type === 'echo' && m.payload === 'hola'), JSON.stringify(s.received[0] ?? {}));

  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'ping', id: 'p2', ts: Date.now(), payload: '' })),
  );
  check('Ping responde pong', s.received.some((m) => m.type === 'pong'), JSON.stringify(s.received[0] ?? {}));

  // --- Validación de mensajes --------------------------------------------
  s = await session(TOKENS.valid, (ws) => ws.send('{"type":"echo","id":'));
  check('JSON malformado cierra 4010', s.code === 4010, `code=${s.code}`);

  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'echo', id: 'p3', ts: Date.now(), payload: '<script>alert(1)</script>' })),
  );
  check('Payload con <script> cierra 4010', s.code === 4010, `code=${s.code}`);

  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'borrar-todo', id: 'p4', ts: Date.now(), payload: 'x' })),
  );
  check('type fuera del enum cierra 4010', s.code === 4010, `code=${s.code}`);

  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'echo', id: 'p5', ts: Date.now(), payload: 'x', extra: 'campo no declarado' })),
  );
  check('Campo desconocido cierra 4010 (esquema estricto)', s.code === 4010, `code=${s.code}`);

  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'echo', id: 'id con espacios!', ts: Date.now(), payload: 'x' })),
  );
  check('id con caracteres no permitidos cierra 4010', s.code === 4010, `code=${s.code}`);

  // --- Límite de tamaño ---------------------------------------------------
  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'echo', id: 'big', ts: Date.now(), payload: 'A'.repeat(80_000) })),
  );
  check('Mensaje > 64 KB cierra 4009', s.code === 4009, `code=${s.code}`);

  // --- Rate limit ---------------------------------------------------------
  s = await session(TOKENS.valid, (ws) => {
    for (let i = 0; i < 60; i++) {
      ws.send(JSON.stringify({ type: 'echo', id: `rl${i}`, ts: Date.now(), payload: 'x' }));
    }
  });
  check('60 mensajes de golpe cierran 4008', s.code === 4008, `code=${s.code} ecos=${s.received.length}`);

  // --- Recuperación -------------------------------------------------------
  s = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'ping', id: 'after', ts: Date.now(), payload: '' })),
  );
  check('Servidor sigue sano tras las sondas', s.received.some((m) => m.type === 'pong'), `code=${s.code}`);

  // --- Puente REST con el .NET 4.8 ---------------------------------------
  const sid = `probe-${Date.now().toString(36)}`;
  const live = await openSession(sid);

  let p = await rest('POST', '/api/push', { sesion: sid, payload: 'aviso desde el 4.8' }, TOKENS.valid);
  check('Push válido devuelve 202', p.status === 202 && p.delivered === 1, `HTTP ${p.status} delivered=${p.delivered}`);

  await new Promise((r) => setTimeout(r, 300));
  const push = live.received.find((m) => m.type === 'push');
  check('El push llega al cliente por WebSocket', !!push && push.payload === 'aviso desde el 4.8',
    push ? `payload=${push.payload}` : 'no llegó');
  check('El frame lleva la sesión', !!push && push.sesion === sid, push ? `sesion=${push.sesion}` : '-');

  p = await rest('POST', '/api/push', { sesion: sid, payload: 'x' }, null);
  check('Push sin token rechazado (401)', p.status === 401 && p.reason === 'TOKEN_MISSING', `HTTP ${p.status} ${p.reason}`);

  p = await rest('POST', '/api/push', { sesion: sid, payload: 'x' }, TOKENS.expired);
  check('Push con token expirado rechazado', p.status === 401 && p.reason === 'TOKEN_EXPIRED', `HTTP ${p.status} ${p.reason}`);

  p = await rest('POST', '/api/push', { sesion: 'no-existe-jamas', payload: 'x' }, TOKENS.valid);
  check('Push a sesión inexistente da 404', p.status === 404 && p.reason === 'SESSION_NOT_FOUND', `HTTP ${p.status} ${p.reason}`);

  p = await rest('POST', '/api/push', { sesion: sid, payload: 'x', extra: 1 }, TOKENS.valid);
  check('Push con campo desconocido da 400', p.status === 400, `HTTP ${p.status} ${p.reason}`);

  p = await rest('POST', '/api/push', { sesion: sid, payload: '<script>alert(1)</script>' }, TOKENS.valid);
  check('Push con <script> da 400', p.status === 400 && p.reason === 'INVALID_PAYLOAD', `HTTP ${p.status} ${p.reason}`);

  p = await rest('POST', '/api/push', { sesion: 'no valido!', payload: 'x' }, TOKENS.valid);
  check('Push con sesión mal formada da 400', p.status === 400 && p.reason === 'INVALID_SESSION', `HTTP ${p.status} ${p.reason}`);

  // --- Outbox: lo que se le enviaría al .NET 4.8 --------------------------
  live.ws.send(JSON.stringify({ type: 'echo', id: 'ob1', ts: Date.now(), payload: 'mensaje del cliente' }));
  await new Promise((r) => setTimeout(r, 300));

  let o = await rest('GET', '/api/outbox', null, TOKENS.valid);
  const sobre = (o.items || []).find((i) => i.sesion === sid);
  check('El mensaje del cliente llega al outbox', !!sobre && sobre.payload === 'mensaje del cliente',
    sobre ? JSON.stringify(sobre) : 'no está');

  o = await rest('GET', '/api/outbox', null, null);
  check('Outbox sin token rechazado (401)', o.status === 401, `HTTP ${o.status} ${o.reason}`);

  live.ws.close();
  await new Promise((r) => setTimeout(r, 200));

  const failed = results.filter((r) => !r.pass);
  console.log(`\n${results.length - failed.length}/${results.length} sondas correctas.`);
  if (failed.length) {
    console.log('Fallaron: ' + failed.map((f) => f.name).join(' | '));
    process.exit(1);
  }
})();
