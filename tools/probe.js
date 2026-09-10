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
  check('Eco correcto', s.received.some((m) => m.type === 'echo' && m.Payload === 'hola'), JSON.stringify(s.received[0] ?? {}));

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

  // --- Peticion al .NET 4.8 -----------------------------------------------
  //
  // Estas sondas valen este el .NET levantado o no: lo que comprueban es el
  // contrato del canal, no la logica de negocio. Cuando no hay backend detras,
  // la peticion vuelve como `error` con un motivo BACKEND_*, que es exactamente
  // el comportamiento que se quiere verificar.
  const sid = `probe-${Date.now().toString(36)}`;
  const live = await openSession(sid);

  const preguntar = async (id, payload, ms = 7000) => {
    live.ws.send(JSON.stringify({ type: 'peticion', id, ts: Date.now(), payload }));
    const limite = Date.now() + ms;
    while (Date.now() < limite) {
      const m = live.received.find((x) => x.id === id);
      if (m) return m;
      await new Promise((r) => setTimeout(r, 50));
    }
    return null;
  };

  const r1 = await preguntar('pet1', { numero: 7 });
  check('Una peticion recibe respuesta con SU id',
    !!r1 && (r1.type === 'respuesta' || r1.type === 'error'),
    r1 ? `type=${r1.type}${r1.reason ? ' ' + r1.reason : ''}` : 'no llego nada');
  check('La respuesta lleva la sesion del token',
    !!r1 && r1.Sesion === sid, r1 ? `Sesion=${r1.Sesion}` : '-');

  // Dos a la vez: cada respuesta tiene que ir a su pregunta. Es lo unico que
  // hace utilizable el canal con mas de una peticion en vuelo.
  live.ws.send(JSON.stringify({ type: 'peticion', id: 'dosA', ts: Date.now(), payload: { numero: 1 } }));
  live.ws.send(JSON.stringify({ type: 'peticion', id: 'dosB', ts: Date.now(), payload: { numero: 2 } }));
  await new Promise((r) => setTimeout(r, 3000));
  const a = live.received.find((m) => m.id === 'dosA');
  const b = live.received.find((m) => m.id === 'dosB');
  check('Dos peticiones simultaneas vuelven cada una con su id', !!a && !!b,
    `dosA=${a ? a.type : 'no'} dosB=${b ? b.type : 'no'}`);

  // Un fallo del .NET no puede costar la conexion: falla la peticion, no la sesion.
  live.ws.send(JSON.stringify({ type: 'ping', id: 'sigo-vivo', ts: Date.now(), payload: '' }));
  await new Promise((r) => setTimeout(r, 400));
  check('El canal sigue vivo despues de las peticiones',
    live.received.some((m) => m.id === 'sigo-vivo' && m.type === 'pong'), '-');

  // El payload de una peticion se sanea igual que el resto: acaba en el DOM.
  let s2 = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'peticion', id: 'xss', ts: Date.now(), payload: { x: '<script>alert(1)</script>' } })),
  );
  check('Peticion con <script> cierra 4010', s2.code === 4010, `code=${s2.code}`);

  // Sin payload no hay nada que preguntarle al .NET.
  s2 = await session(TOKENS.valid, (ws) =>
    ws.send(JSON.stringify({ type: 'peticion', id: 'vacia', ts: Date.now() })),
  );
  check('Peticion sin payload cierra 4010', s2.code === 4010, `code=${s2.code}`);

  live.ws.close();
  await new Promise((r) => setTimeout(r, 200));

  const failed = results.filter((r) => !r.pass);
  console.log(`\n${results.length - failed.length}/${results.length} sondas correctas.`);
  if (failed.length) {
    console.log('Fallaron: ' + failed.map((f) => f.name).join(' | '));
    process.exit(1);
  }
})();
