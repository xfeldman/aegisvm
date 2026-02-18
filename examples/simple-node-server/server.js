const http = require('http');

function log(level, msg, extra = {}) {
  const entry = { level, msg, ts: new Date().toISOString(), ...extra };
  console.log(JSON.stringify(entry));
}

const server = http.createServer((req, res) => {
  const secretKey = process.env.SECRET_KEY || '';
  const body = JSON.stringify({
    status: 'ok',
    secret_configured: !!secretKey,
    path: req.url,
    timestamp: new Date().toISOString(),
  });

  res.writeHead(200, { 'Content-Type': 'application/json' });
  res.end(body);
  log('info', 'request served', { path: req.url, method: req.method });
});

const PORT = 80;
log('info', 'starting server', { port: PORT });
server.listen(PORT, '0.0.0.0');
