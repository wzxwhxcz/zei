import http from 'node:http';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';

const execFileAsync = promisify(execFile);
const __dirname = path.dirname(fileURLToPath(import.meta.url));
// ESM 里 require CommonJS 模块（chat_proxy.js）
const require = createRequire(import.meta.url);
const chatProxy = require('./chat_proxy.cjs');

// ─── Config ───
const PORT = Number(process.env.PORT || 9876);
const HOST = process.env.HOST || '127.0.0.1';
const SECRET = process.env.SECRET || '';
// 真实配置来自 prod-fe-1.1.61 前端 Grt：REGION=sgp, PREFIX=no8xfe, SCENE_ID=36qgs6xb, MODE=embed
// （旧的 didk33e0 / popup 路径已被上游废弃，返回 verify_code F019）
const SCENE_ID = process.env.SCENE_ID || '36qgs6xb';

// Token 预取池配置
const POOL_SIZE = Number(process.env.POOL_SIZE || 5);       // 池中保持的 token 数量
const TOKEN_TTL = Number(process.env.TOKEN_TTL || 240000);  // token 有效期 4 分钟
const REFILL_INTERVAL = Number(process.env.REFILL_INTERVAL || 3000); // 补充间隔 3s

// ─── State ───
let ready = true; // No browser to wait for
let lastError = '';
let stats = { served: 0, errors: 0, refills: 0 };

// Token 池：{ token, createdAt }
const tokenPool = [];
let refilling = false;

// ─── Token acquisition ───
async function acquireToken() {
  const REGION = 'sgp';
  const PREFIX = 'no8xfe';
  const scriptPath = path.join(__dirname, 'solver.cjs');

  try {
    const { stdout, stderr } = await execFileAsync(process.execPath, [scriptPath, SCENE_ID, REGION, PREFIX], { timeout: 40000 });
    const match = stdout.match(/VERIFY_PARAM=(.+)/);
    if (match) {
      return match[1].trim();
    }
    throw new Error(`Failed to extract token. Output: ${stdout.substring(0, 100)}... Err: ${stderr.substring(0, 100)}`);
  } catch (err) {
    if (err.code === 'ETIMEDOUT' || err.killed) {
      throw new Error('captcha timeout');
    }
    throw new Error(`Solver error: ${err.message}`);
  }
}

// ─── Token pool management ───

function getValidToken() {
  const now = Date.now();
  // 清理过期 token
  while (tokenPool.length > 0 && (now - tokenPool[0].createdAt) > TOKEN_TTL) {
    tokenPool.shift();
  }
  if (tokenPool.length > 0) {
    return tokenPool.shift().token;
  }
  return null;
}

async function refillPool() {
  if (refilling) return;
  refilling = true;
  try {
    const now = Date.now();
    // 清理过期的
    while (tokenPool.length > 0 && (now - tokenPool[0].createdAt) > TOKEN_TTL) {
      tokenPool.shift();
    }
    // 每次只补充一个，避免连续调用触发阿里云频率限制
    if (tokenPool.length < POOL_SIZE) {
      try {
        const token = await acquireToken();
        tokenPool.push({ token, createdAt: Date.now() });
        stats.refills++;
      } catch (err) {
        lastError = err.message;
        stats.errors++;
        // 超时不打日志刷屏，只记录非超时错误
        if (!err.message.includes('captcha timeout')) {
          console.error(`[pool] Refill error: ${err.message}`);
        }
      }
    }
  } finally {
    refilling = false;
  }
}

// ─── HTTP Server ───

function sendJson(res, status, data) {
  const body = JSON.stringify(data);
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Content-Length': Buffer.byteLength(body),
  });
  res.end(body);
}

const server = http.createServer(async (req, res) => {
  if (SECRET && req.headers['x-secret'] !== SECRET) {
    return sendJson(res, 401, { error: 'unauthorized' });
  }

  if (req.method === 'GET' && req.url === '/health') {
    return sendJson(res, 200, {
      ok: ready,
      pool: tokenPool.length,
      stats,
      lastError,
    });
  }

  if (req.method === 'GET' && req.url === '/token') {
    if (!ready) {
      return sendJson(res, 503, { error: 'not ready', lastError });
    }

    // 先从池里取
    const cached = getValidToken();
    if (cached) {
      stats.served++;
      console.log(`[provider] Served cached token (pool: ${tokenPool.length})`);
      return sendJson(res, 200, { ok: true, token: cached, cached: true });
    }

    // 池空了，实时获取
    try {
      const started = Date.now();
      const token = await acquireToken();
      const elapsed = Date.now() - started;
      stats.served++;
      console.log(`[provider] Served fresh token in ${elapsed}ms (pool: ${tokenPool.length})`);
      return sendJson(res, 200, { ok: true, token, cached: false, elapsed_ms: elapsed });
    } catch (err) {
      lastError = err.message;
      stats.errors++;
      console.error(`[provider] Token error: ${err.message}`);
      return sendJson(res, 500, { ok: false, error: err.message });
    }
  }

  // ─── 全链路 chat 代理：同一 JSDOM window 内 captcha → chats/new → completions ───
  if (req.method === 'POST' && req.url === '/v1/chat') {
    let bodyBuf = '';
    for await (const chunk of req) bodyBuf += chunk;
    let payload;
    try {
      payload = JSON.parse(bodyBuf);
    } catch (e) {
      return sendJson(res, 400, { ok: false, error: 'invalid json: ' + e.message });
    }
    const started = Date.now();
    let headSent = false;
    try {
      const result = await chatProxy.handleChat(payload, {
        // 上游响应头到达：立刻 writeHead，开始流式
        onHeaders: (status, headers) => {
          if (headSent) return;
          headSent = true;
          res.writeHead(status || 200, {
            'Content-Type': (headers && headers['content-type']) || 'text/event-stream; charset=utf-8',
            'Cache-Control': 'no-cache',
            'Connection': 'keep-alive',
            'X-Accel-Buffering': 'no',
          });
        },
        // 上游 SSE 增量：实时 pipe 给客户端
        onChunk: (chunk) => {
          if (!headSent) return; // 头还没到，丢弃（理论上 onHeaders 先于 onChunk）
          res.write(chunk);
        },
      });
      const elapsed = Date.now() - started;
      // 若上游全程没发头（异常路径），补一个 200 头再结束
      if (!headSent) {
        res.writeHead(result.status || 200, {
          'Content-Type': (result.headers && result.headers['content-type']) || 'text/event-stream; charset=utf-8',
        });
        if (result.full) res.write(result.full);
      }
      res.end();
      console.log(`[chat-proxy] /v1_chat done in ${elapsed}ms, upstream status=${result.status}`);
    } catch (err) {
      console.error(`[chat-proxy] /v1_chat error: ${err.message}`);
      if (!headSent) {
        return sendJson(res, 502, { ok: false, error: err.message });
      }
      // 头已发：在 SSE 流里补一个错误标记再结束
      res.end(`data: {"data":{"content":"","done":true,"error":{"code":"PROVIDER_ERROR","detail":${JSON.stringify(err.message)}}},"type":"chat:completion"}\n\ndata: [DONE]\n\n`);
    }
    return;
  }

  sendJson(res, 404, { error: 'Use GET /token, GET /health, or POST /v1/chat' });
});

// ─── Start ───

server.listen(PORT, HOST, async () => {
  console.log(`[provider] zai-captcha-provider (JSDOM/execFile) listening on http://${HOST}:${PORT}`);
  console.log(`[provider] captcha token pool: ${POOL_SIZE}, TTL: ${TOKEN_TTL}ms`);
  try {
    // 初始填充 captcha token 池
    await refillPool();
    console.log(`[provider] Initial captcha token pool filled: ${tokenPool.length}`);
    // 定期补充
    setInterval(refillPool, REFILL_INTERVAL);
  } catch (err) {
    console.error('[provider] Startup error:', err.message);
    lastError = err.message;
  }
  // 预热 JSDOM 全链路 chat 窗口池（非阻塞，后台补）
  chatProxy.initPool().then(() => {
    console.log('[chat-proxy] window pool ready');
  }).catch((e) => {
    console.error('[chat-proxy] window pool init failed:', e.message);
  });
});

process.on('SIGINT', async () => {
  console.log('[provider] Shutting down...');
  process.exit(0);
});

process.on('SIGTERM', async () => {
  process.exit(0);
});
