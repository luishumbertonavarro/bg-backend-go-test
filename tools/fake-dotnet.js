#!/usr/bin/env node
/**
 * Simula la ruta del backend .NET 4.8 a la que Go reenvia las `peticion`.
 *
 * Go hace POST {Sesion, Payload} y ESPERA; esto responde {Sesion, Payload} en el
 * mismo HTTP, que es el contrato del puente sincrono.
 *
 *   node tools/fake-dotnet.js                      # camino feliz
 *   node tools/fake-dotnet.js --delay 8000         # fuerza BACKEND_TIMEOUT
 *   node tools/fake-dotnet.js --status 500         # fuerza BACKEND_ERROR
 *   node tools/fake-dotnet.js --sesion otra-cosa   # fuerza que Go descarte la respuesta
 *   node tools/fake-dotnet.js --sin-sesion         # devuelve Sesion vacia
 *
 * Opciones:
 *   --port         puerto de escucha (def. 9099)
 *   --delay        milisegundos antes de responder (def. 0)
 *   --status       codigo HTTP a devolver (def. 200)
 *   --sesion       Sesion concreta a devolver, en vez de la que llego
 *   --sin-sesion   devolver la Sesion VACIA
 *
 * Por defecto DEVUELVE LA SESION QUE RECIBIO, que es lo que hara el .NET real:
 * la tiene delante, no le cuesta nada. Las otras dos formas existen para probar
 * los caminos raros:
 *
 *   --sin-sesion  Go entiende "entregasela a quien pregunto" y funciona igual.
 *                 Es lo que permite que un Mock Server con respuesta enlatada
 *                 sirva sin conocer la sesion del cliente.
 *   --sesion X    Go DESCARTA la respuesta y avisa con BACKEND_ERROR: nombrar
 *                 una sesion ajena en un round-trip sincrono no tiene sentido
 *                 legitimo, y obedecerla entregaria datos de un usuario a otro.
 */
const http = require('node:http');

const argv = process.argv.slice(2);
const opts = {};
for (let i = 0; i < argv.length; i++) {
  if (!argv[i].startsWith('--')) continue;
  const key = argv[i].slice(2);
  if (key === 'sin-sesion') opts[key] = true;
  else opts[key] = argv[++i];
}

const port = Number(opts.port || 9099);
const delay = Number(opts.delay || 0);
const status = Number(opts.status || 200);
const estados = ['PENDIENTE', 'VALIDADO', 'APROBADO', 'RECHAZADO'];

const server = http.createServer((req, res) => {
  let body = '';
  req.on('data', (c) => (body += c));
  req.on('end', () => {
    let entrada = {};
    try {
      entrada = JSON.parse(body);
    } catch {
      /* se responde igual: interesa el comportamiento de Go, no validar aqui */
    }
    console.log(`<- ${req.method} ${req.url} ${body}`);

    // Un "estado" cualquiera derivado del numero: no es logica de negocio, solo
    // algo que cambie para poder ver el ciclo entero.
    const numero = (entrada.Payload || {}).numero ?? 0;

    // Por defecto se devuelve la Sesion que llego, que es lo natural: el .NET la
    // tiene delante. Vacia y ajena son los dos casos raros, cada uno tras su flag.
    const sesion = opts['sin-sesion'] ? '' : (opts.sesion ?? entrada.Sesion ?? '');

    const salida = JSON.stringify({
      Sesion: sesion,
      Payload: {
        // El numero VUELVE en la respuesta: el frontend necesita saber de que
        // numero es este estado, sobre todo con varias peticiones en vuelo.
        numero,
        estado: estados[Number(numero) % estados.length],
        validadoEn: new Date().toISOString(),
      },
    });

    setTimeout(() => {
      console.log(`-> ${status} ${salida}`);
      res.writeHead(status, { 'Content-Type': 'application/json' });
      res.end(salida);
    }, delay);
  });
});

server.listen(port, () => {
  console.log(`.NET 4.8 simulado en http://127.0.0.1:${port}`);
  console.log(`Pon esto en backend-go/.env:  WS_DOTNET_API_URL=http://127.0.0.1:${port}/calcular`);
  if (delay) console.log(`Retardo de ${delay} ms por respuesta.`);
  if (status !== 200) console.log(`Devolviendo HTTP ${status}.`);
  if (opts['sin-sesion']) console.log('Devolviendo la Sesion VACIA.');
  else if (opts.sesion) console.log(`Devolviendo siempre Sesion="${opts.sesion}".`);
  else console.log('Devolviendo la Sesion que llegue en cada peticion.');
});
