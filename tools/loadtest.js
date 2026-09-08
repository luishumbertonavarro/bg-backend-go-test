#!/usr/bin/env node
/**
 * Prueba de carga independiente del navegador (100+ conexiones concurrentes).
 *
 *   node tools/loadtest.js
 *   node tools/loadtest.js --url ws://localhost:8084/ws --conns 150 --msgs 20
 *
 * Opciones:
 *   --url     URL completa (por defecto ws://localhost:<WS_PORT_GO>/ws)
 *   --conns   nº de conexiones concurrentes            (def. 120)
 *   --msgs    mensajes de eco por conexión             (def. 10)
 *   --rate    mensajes/seg por conexión                (def. 10, bajo el límite de 20)
 *   --origin  cabecera Origin a enviar   (def. http://localhost:4200)
 *   --token   JWT a usar (def. uno válido generado al vuelo)
 *   --json    imprime solo el resultado en JSON
 *
 * Mide: aceptadas vs rechazadas (con motivo), handshake, RTT (p50/p95/max),
 * throughput global, y una comprobación de salud DESPUÉS de la tormenta para
 * detectar si el servidor quedó colgado o se recuperó solo.
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
  if (argv[i].startsWith('--')) {
    const key = argv[i].slice(2);
    if (key === 'json') opts.json = true;
    else opts[key] = argv[++i];
  }
}

const env = loadEnv();
const stack = 'go';
const baseUrl = opts.url || `ws://localhost:${env.WS_PORT_GO || '8084'}/ws`;
const CONNS = Number(opts.conns || 120);
const MSGS = Number(opts.msgs || 10);
const RATE = Number(opts.rate || 10);
const ORIGIN = opts.origin || 'http://localhost:4200';
const TOKEN =
  opts.token || execFileSync(process.execPath, [path.join(__dirname, 'gen-token.js')], { encoding: 'utf8' }).trim();

const log = (...a) => { if (!opts.json) console.log(...a); };

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const pct = (arr, p) => {
  if (!arr.length) return null;
  const s = [...arr].sort((a, b) => a - b);
  return +s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))].toFixed(2);
};

/** Consulta el endpoint de diagnóstico para saber por qué se rechazó una conexión. */
function diagnose(wsUrl, token, origin) {
  return new Promise((resolve) => {
    const u = new URL(wsUrl.replace(/^ws/, 'http'));
    u.searchParams.set('token', token);
    const req = http.get(u, { headers: { Origin: origin } }, (res) => {
      let body = '';
      res.on('data', (c) => (body += c));
      res.on('end', () => {
        try {
          const j = JSON.parse(body);
          resolve(j.reason || `HTTP_${res.statusCode}`);
        } catch {
          resolve(`HTTP_${res.statusCode}`);
        }
      });
    });
    req.on('error', () => resolve('UNREACHABLE'));
    req.setTimeout(3000, () => { req.destroy(); resolve('DIAG_TIMEOUT'); });
  });
}

/** Una conexión: handshake, N ecos midiendo RTT, cierre limpio. */
function runConnection(i, stats) {
  return new Promise((resolve) => {
    const url = `${baseUrl}?token=${encodeURIComponent(TOKEN)}`;
    const t0 = performance.now();
    let ws;
    try {
      ws = new WebSocket(url, { origin: ORIGIN, maxPayload: 1 << 20, handshakeTimeout: 10000 });
    } catch (e) {
      stats.rejected.push({ i, reason: 'CLIENT_ERROR', detail: String(e) });
      return resolve();
    }

    const pending = new Map();
    let sent = 0;
    let opened = false;
    let timer = null;
    let closeCode = null;

    const finish = async () => {
      if (timer) clearInterval(timer);
      if (!opened) {
        const reason = await diagnose(baseUrl, TOKEN, ORIGIN);
        stats.rejected.push({ i, reason, code: closeCode });
      } else if (closeCode && closeCode >= 4000) {
        stats.closedByServer.push({ i, code: closeCode });
      }
      resolve();
    };

    ws.on('open', () => {
      opened = true;
      stats.handshakes.push(performance.now() - t0);
      stats.accepted++;
      timer = setInterval(() => {
        if (sent >= MSGS) {
          clearInterval(timer);
          // Espera a que lleguen los ecos pendientes y cierra.
          setTimeout(() => ws.readyState === WebSocket.OPEN && ws.close(1000, 'done'), 1500);
          return;
        }
        const id = `c${i}-m${sent}`;
        pending.set(id, performance.now());
        try {
          ws.send(JSON.stringify({ type: 'echo', id, ts: Date.now(), payload: `carga-${i}-${sent}` }));
          sent++;
          stats.sent++;
        } catch {
          clearInterval(timer);
        }
      }, Math.max(1, Math.floor(1000 / RATE)));
    });

    let instanceSeen = null;
    ws.on('message', (buf) => {
      let msg;
      try { msg = JSON.parse(buf.toString()); } catch { stats.protocolErrors++; return; }
      const t = pending.get(msg.id);
      if (t !== undefined) {
        pending.delete(msg.id);
        stats.rtts.push(performance.now() - t);
        stats.received++;
      }
      if (msg.instance && !instanceSeen) {
        instanceSeen = msg.instance;
        stats.byInstance[msg.instance] = (stats.byInstance[msg.instance] || 0) + 1;
      }
      if (msg.type === 'error') stats.serverErrors.push(msg.reason);
    });

    ws.on('close', (code) => { closeCode = code; finish(); });
    ws.on('error', () => { /* el motivo real lo aporta diagnose() en close */ });
  });
}

/** Tras la tormenta: ¿el servidor sigue aceptando y respondiendo con normalidad? */
function healthCheck() {
  return new Promise((resolve) => {
    const t0 = performance.now();
    const ws = new WebSocket(`${baseUrl}?token=${encodeURIComponent(TOKEN)}`, { origin: ORIGIN });
    const done = (r) => { try { ws.close(); } catch {} resolve(r); };
    const to = setTimeout(() => done({ ok: false, detail: 'timeout 5s' }), 5000);
    ws.on('open', () => ws.send(JSON.stringify({ type: 'ping', id: 'health', ts: Date.now(), payload: '' })));
    ws.on('message', (b) => {
      clearTimeout(to);
      let m; try { m = JSON.parse(b.toString()); } catch { return done({ ok: false, detail: 'respuesta ilegible' }); }
      done({ ok: m.type === 'pong', rttMs: +(performance.now() - t0).toFixed(2), detail: m.type });
    });
    ws.on('error', (e) => { clearTimeout(to); done({ ok: false, detail: String(e.message || e) }); });
  });
}

(async () => {
  log(`\n=== Prueba de carga :: stack=${stack} url=${baseUrl}`);
  log(`    ${CONNS} conexiones x ${MSGS} mensajes @ ${RATE} msg/s por conexión\n`);

  const stats = {
    accepted: 0, sent: 0, received: 0, protocolErrors: 0,
    handshakes: [], rtts: [], rejected: [], closedByServer: [], serverErrors: [],
    // Conexiones atendidas por cada replica. Detras de un balanceador es lo que
    // revela el reparto real; con un solo proceso siempre sale una sola clave.
    byInstance: {},
  };

  const t0 = performance.now();
  await Promise.all(Array.from({ length: CONNS }, (_, i) => runConnection(i, stats)));
  const totalMs = performance.now() - t0;

  log('Tormenta terminada. Esperando 2 s antes de comprobar la salud del servidor...');
  await sleep(2000);
  const health = await healthCheck();

  const byReason = {};
  for (const r of stats.rejected) byReason[r.reason] = (byReason[r.reason] || 0) + 1;

  const result = {
    stack, url: baseUrl, conns: CONNS, msgsPerConn: MSGS,
    accepted: stats.accepted,
    rejected: stats.rejected.length,
    rejectedByReason: byReason,
    closedByServerMidTest: stats.closedByServer.length,
    messagesSent: stats.sent,
    messagesEchoed: stats.received,
    lostMessages: stats.sent - stats.received,
    handshakeMs: { p50: pct(stats.handshakes, 50), p95: pct(stats.handshakes, 95), max: pct(stats.handshakes, 100) },
    rttMs: { p50: pct(stats.rtts, 50), p95: pct(stats.rtts, 95), max: pct(stats.rtts, 100) },
    totalSeconds: +(totalMs / 1000).toFixed(2),
    avgMsgsPerSec: +((stats.received / (totalMs / 1000)) || 0).toFixed(1),
    serverStillHealthyAfterLoad: health,
    connectionsByInstance: stats.byInstance,
  };

  if (opts.json) { console.log(JSON.stringify(result, null, 2)); return; }

  console.log('\n--- Resultado ---');
  console.log(`Conexiones aceptadas ....... ${result.accepted}/${CONNS}`);
  console.log(`Conexiones rechazadas ...... ${result.rejected}  ${JSON.stringify(byReason)}`);
  console.log(`Cerradas por el servidor ... ${result.closedByServerMidTest} (rate limit / idle / etc.)`);
  console.log(`Mensajes enviados/eco ...... ${result.messagesSent} / ${result.messagesEchoed}  (perdidos: ${result.lostMessages})`);
  console.log(`Handshake ms ............... p50 ${result.handshakeMs.p50}  p95 ${result.handshakeMs.p95}  max ${result.handshakeMs.max}`);
  console.log(`RTT ms ..................... p50 ${result.rttMs.p50}  p95 ${result.rttMs.p95}  max ${result.rttMs.max}`);
  console.log(`Duración total ............. ${result.totalSeconds} s`);
  console.log(`Throughput medio ........... ${result.avgMsgsPerSec} msg/s`);
  console.log(`Servidor sano tras la carga  ${health.ok ? 'SÍ' : 'NO'} (${health.detail}${health.rttMs ? `, ${health.rttMs} ms` : ''})`);

  const instances = Object.entries(result.connectionsByInstance).sort((a, b) => b[1] - a[1]);
  if (instances.length) {
    console.log(`Reparto por instancia ...... ${instances.length} instancia(s)`);
    for (const [name, n] of instances) {
      const pctShare = ((n / result.accepted) * 100).toFixed(1);
      console.log(`  ${name.padEnd(34)} ${String(n).padStart(4)} conexiones  (${pctShare}%)`);
    }
  }
  console.log('');
})();
