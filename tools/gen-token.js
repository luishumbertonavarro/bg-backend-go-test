#!/usr/bin/env node
/**
 * Generador de JWT HS256 para el POC. Sin dependencias: usa node:crypto.
 *
 *   node tools/gen-token.js                 -> token válido (60 min)
 *   node tools/gen-token.js --kind expired  -> token ya expirado  (prueba 4003)
 *   node tools/gen-token.js --kind badsig   -> firma corrupta     (prueba 4002)
 *   node tools/gen-token.js --kind badiss   -> issuer incorrecto  (prueba 4004)
 *   node tools/gen-token.js --kind badaud   -> audience incorrecta(prueba 4004)
 *   node tools/gen-token.js --all           -> imprime los cinco en JSON
 *
 * Opciones: --sub <id>  --ttl <segundos>  --sid <sesion>
 *
 * El claim `sid` identifica la sesion a la que el backend enruta los push. Se
 * genera unico por token; usa --sid para fijarlo y poder empujar a una sesion
 * conocida con push.js.
 */
const crypto = require('node:crypto');
const fs = require('node:fs');
const path = require('node:path');

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

const b64url = (buf) =>
  Buffer.from(buf).toString('base64').replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');

function sign(payload, secret) {
  const header = b64url(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body = b64url(JSON.stringify(payload));
  const data = `${header}.${body}`;
  const sig = b64url(crypto.createHmac('sha256', secret).update(data).digest());
  return `${data}.${sig}`;
}

function build(kind, opts, env) {
  const now = Math.floor(Date.now() / 1000);
  const ttl = Number(opts.ttl || 3600);
  const payload = {
    sub: opts.sub || 'poc-user-1',
    // Unico por token: dos pestañas del mismo usuario son dos sesiones distintas.
    sid: opts.sid || `s-${crypto.randomUUID().slice(0, 8)}`,
    iss: env.WS_JWT_ISSUER || 'ws-poc-issuer',
    aud: env.WS_JWT_AUDIENCE || 'ws-poc-clients',
    iat: now,
    exp: now + ttl,
  };
  const secret = env.WS_JWT_SECRET || '';

  switch (kind) {
    case 'expired':
      payload.iat = now - 7200;
      payload.exp = now - 3600;
      return sign(payload, secret);
    case 'badiss':
      payload.iss = 'attacker-issuer';
      return sign(payload, secret);
    case 'badaud':
      payload.aud = 'some-other-audience';
      return sign(payload, secret);
    case 'badsig': {
      const t = sign(payload, secret).split('.');
      // Altera un carácter de la firma manteniendo el formato base64url.
      const s = t[2];
      t[2] = (s[0] === 'A' ? 'B' : 'A') + s.slice(1);
      return t.join('.');
    }
    case 'valid':
    default:
      return sign(payload, secret);
  }
}

const argv = process.argv.slice(2);
const opts = {};
for (let i = 0; i < argv.length; i++) {
  if (argv[i].startsWith('--')) {
    const key = argv[i].slice(2);
    if (key === 'all') opts.all = true;
    else opts[key] = argv[++i];
  }
}

const env = loadEnv();
if (!env.WS_JWT_SECRET) {
  console.error('Falta WS_JWT_SECRET (revisa el .env de la raíz).');
  process.exit(1);
}

if (opts.all) {
  const kinds = ['valid', 'expired', 'badsig', 'badiss', 'badaud'];
  const out = {};
  for (const k of kinds) out[k] = build(k, opts, env);
  console.log(JSON.stringify(out, null, 2));
} else {
  console.log(build(opts.kind || 'valid', opts, env));
}
