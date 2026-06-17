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
// 用独立的 PROVIDER_PORT/PROVIDER_HOST，绝不读全局 PORT/HOST
// （HF Spaces 设 PORT=7860 给 Go proxy，provider 用 9876 内部通信即可，
//  否则两个进程抢同一个端口 → "bind: address already in use"）。
const PORT = Number(process.env.PROVIDER_PORT || 9876);
const HOST = process.env.PROVIDER_HOST || '127.0.0.1';
const SECRET = process.env.SECRET || '';
// 老路径 /token 用的 scene（solver.cjs 的 popup/traceless 模式）。
// 注意：36qgs6xb（前端 embed 主配置）在 JSDOM 里跑不通（network error），
// 只有 didk33e0 能出 token。F019 是跨进程环境不一致导致，非 scene 问题。
// 全代理路径(CAPTCHA_FULL_PROXY_URL)在 chat_proxy.cjs 内部用自己的 scene，与此无关。
const SCENE_ID = process.env.SCENE_ID || 'didk33e0';

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
    // 老路径 token 池惰性启动（首次访问 /token 才填，避免启动刷屏）
    if (typeof server._ensureTokenPool === 'function') server._ensureTokenPool();
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
          // 禁用 Nagle 算法：让每个 chunk 立即发送，不被 TCP 攒批（伪流式根因）
          if (res.socket && typeof res.socket.setNoDelay === 'function') {
            res.socket.setNoDelay(true);
          }
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
  console.log(`[provider] captcha token pool: ${POOL_SIZE}, TTL: ${TOKEN_TTL}ms (老路径 /token，按需启用)`);
  // 老路径 captcha token 池改为惰性填充：
  // 全代理路径(CAPTCHA_FULL_PROXY_URL)才是主路径，老 /token 池多数情况用不到。
  // 启动时不再狂填（避免 solver 报错刷屏），首次访问 /token 时才填 + 起定时器。
  let tokenPoolStarted = false;
  function ensureTokenPool() {
    if (tokenPoolStarted) return;
    tokenPoolStarted = true;
    console.log('[provider] 首次访问 /token，启动老路径 token 池...');
    refillPool().catch(() => {});
    setInterval(refillPool, REFILL_INTERVAL);
  }
  // 暴露给 /token 路由用
  server._ensureTokenPool = ensureTokenPool;

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
