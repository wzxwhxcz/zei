// chat_proxy.js — JSDOM 全链路 chat 代理
//
// 核心思路（已验证）：在同一个 JSDOM window 内完成
//   initAliyunCaptcha → /api/v1/chats/new → /api/v2/chat/completions
// 避免「拿 captcha 的环境」与「发 chat 的环境」不一致导致阿里云 F019 verify_failed。
//
// 第一版：单 window + 非流式（onload 全量返回）。窗口池/流式后续叠加。
//
// 协议：POST /v1/chat
//   请求 body: {
//     token: "eyJ...",                      // z.ai JWT
//     upstream_model: "glm-4.7",            // 已映射的上游 model id（Go 侧 GetUpstreamConfig 结果）
//     enable_thinking: true,
//     auto_web_search: false,
//     signature_prompt: "用户最后一条文本",  // 用于签名
//     messages: [{role,content}],           // OpenAI 格式（content 已含 fileID 的 image_url/video_url）
//     files: [...],                          // 可选，UpstreamFile 数组（多模态）
//   }
//   响应：直接透传上游 SSE 原文（text/event-stream），Go 用现有 SSE 解析即可。

const { JSDOM, VirtualConsole, CookieJar } = require('jsdom');
const crypto = require('crypto');
const https = require('https');
const http = require('http');
const { URL } = require('url');

// ─── 配置 ───
const SCENE = process.env.CAPTCHA_SCENE || 'didk33e0';
const REGION = process.env.CAPTCHA_REGION || 'sgp';
const PREFIX = process.env.CAPTCHA_PREFIX || 'no8xfe';
const MODE = process.env.CAPTCHA_MODE || 'embed';
const UA = process.env.CAPTCHA_UA || 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36';
const FE_VERSION = process.env.FE_VERSION || 'prod-fe-1.1.62';
const ACCEPT_LANGUAGE = process.env.ACCEPT_LANGUAGE || 'zh-CN';

// ─── 工具 ───
function jwtUserId(t) {
  try {
    const p = t.split('.')[1].replace(/-/g, '+').replace(/_/g, '/');
    return JSON.parse(Buffer.from(p, 'base64').toString('utf8')).id || '';
  } catch { return ''; }
}
function hmacHex(key, data) { return crypto.createHmac('sha256', key).update(data).digest('hex'); }
function genSig(userId, requestId, userContent, ts) {
  const requestInfo = `requestId,${requestId},timestamp,${ts},user_id,${userId}`;
  const contentB64 = Buffer.from(userContent).toString('base64');
  const signData = `${requestInfo}|${contentB64}|${ts}`;
  const period = Math.floor(ts / (5 * 60 * 1000));
  const first = hmacHex('key-@@@@)))()((9))-xxxx&&&%%%%%', String(period));
  return hmacHex(first, signData);
}

// 当前时间，对齐真实抓包的 variables 格式（Asia/Shanghai = UTC+8）
function nowVars() {
  const now = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  // z.ai 前端用的是浏览器本地时间（抓包是 Asia/Shanghai），这里用 UTC+8
  const sh = new Date(now.getTime() + 8 * 3600 * 1000);
  const curDT = `${sh.getUTCFullYear()}-${pad(sh.getUTCMonth()+1)}-${pad(sh.getUTCDate())} ${pad(sh.getUTCHours())}:${pad(sh.getUTCMinutes())}:${pad(sh.getUTCSeconds())}`;
  const weekdays = ['Sunday','Monday','Tuesday','Wednesday','Thursday','Friday','Saturday'];
  return {
    '{{USER_NAME}}': 'Anonymous',
    '{{USER_LOCATION}}': 'Unknown',
    '{{CURRENT_DATETIME}}': curDT,
    '{{CURRENT_DATE}}': curDT.split(' ')[0],
    '{{CURRENT_TIME}}': curDT.split(' ')[1],
    '{{CURRENT_WEEKDAY}}': weekdays[sh.getUTCDay()],
    '{{CURRENT_TIMEZONE}}': 'Asia/Shanghai',
    '{{USER_LANGUAGE}}': 'zh-CN',
  };
}

// ─── NodeXHR：基于 Node 原生 https/http 的 XMLHttpRequest 实现 ───
// 劫持 JSDOM 的 window.XMLHttpRequest，绕过 JSDOM XHR 的 CORS 预检/TLS 问题。
// 在 HF Spaces 上，JSDOM 默认 XHR 发请求被阿里云 CDN 返回 405（可能是 CORS 预检
// 触发 OPTIONS 或 TLS 指纹问题）。用 Node 原生 https 直接发，不走 JSDOM 的 XHR。
// 代理支持：通过 proxy_pool 自动抓取免费代理（ProxyScrape）或手动指定 HTTPS_PROXY。
// HF Spaces 数据中心 IP 被阿里云 CDN 风控（Node TLS 指纹 + 数据中心 IP → 405），
// 挂代理用干净 IP 出去即可解决。
const proxyPool = require('./proxy_pool.cjs');

class NodeXHR {
  constructor(cookieJar) {
    this._cookieJar = cookieJar || null; // JSDOM CookieJar，用于存取 z.ai 安全 cookie
    this.readyState = 0;
    this.status = 0;
    this.statusText = '';
    this.responseText = '';
    this.responseType = '';
    this.timeout = 0;
    this._method = 'GET';
    this._url = '';
    this._headers = {};
    this._body = null;
    this._resHeaders = {};
    this.onreadystatechange = null;
    this.onprogress = null;
    this.onload = null;
    this.onerror = null;
    this.ontimeout = null;
    this._req = null;
  }
  open(method, url) { this._method = method; this._url = url; this._fire(1); }
  setRequestHeader(k, v) { this._headers[k] = v; }
  _fire(s) { this.readyState = s; if (this.onreadystatechange) { try { this.onreadystatechange(); } catch {} } }
  getAllResponseHeaders() {
    return Object.entries(this._resHeaders).map(([k,v]) => `${k}: ${v}`).join('\r\n');
  }
  send(body) {
    this._body = body;
    // UPSTREAM_BASE_URL（CF Worker / Supabase 反代）：把 chat.z.ai 的路径编码到 query 参数 _path
    // CF Worker：直接替换域名即可（无 nginx 限制）
    // Supabase：nginx 不允许 POST 带子路径，所以把路径编码到 ?_path= 里，
    //           POST 只打到函数根路径，反代从 _path 读取真实路径转发
    let sendUrl = this._url;
    const upstreamBase = (process.env.UPSTREAM_BASE_URL || '').replace(/\/$/, '');
    if (upstreamBase && sendUrl.includes('chat.z.ai')) {
      const origUrl = new URL(sendUrl);
      const realPath = origUrl.pathname + origUrl.search;
      if (upstreamBase.includes('supabase.co') || upstreamBase.includes('deno.dev')) {
        // Supabase / Deno Deploy：路径编码到 query，POST 打到根路径
        sendUrl = upstreamBase + '/?_path=' + encodeURIComponent(realPath);
      } else {
        // CF Worker：直接替换域名
        sendUrl = upstreamBase + realPath;
      }
    }
    const u = new URL(sendUrl);
    const lib = u.protocol === 'https:' ? https : http;
    const maxProxyRetries = 8;
    let attempt = 0;

    const trySend = () => {
      const finalHeaders = { ...this._headers };
      // User-Agent 兜底：没显式设就用浏览器 UA。
      // Node 原生 https 默认发 "Node.js/x.x" → z.ai CDN 检测到非浏览器 UA 直接 405。
      // 必须显式设成 Chrome UA，否则间歇性 405（CDN 抽样检测）。
      if (!finalHeaders['User-Agent'] && !finalHeaders['user-agent']) {
        finalHeaders['User-Agent'] = UA;
      }
      const opts = {
        method: this._method,
        hostname: u.hostname,
        port: u.port || (u.protocol === 'https:' ? 443 : 80),
        path: u.pathname + u.search,
        headers: finalHeaders,
      };
      // 挂代理
      const proxyEntry = proxyPool.nextAgent();
      if (proxyEntry) opts.agent = proxyEntry.agent;
      this._proxyEntry = proxyEntry;
      opts.headers['Connection'] = 'keep-alive';

      this._req = lib.request(opts, (res) => {
        this.status = res.statusCode || 0;
        this.statusText = res.statusMessage || '';
        this._resHeaders = res.headers || {};
        // 把 Set-Cookie 存进 CookieJar（z.ai 的 acw_tc/cdn_sec_tc 安全 cookie）。
        // 后续请求（completions）从 jar 提取这些 cookie 带上，ESA 才放行。
        if (this._cookieJar && res.headers['set-cookie']) {
          for (const sc of res.headers['set-cookie']) {
            try { this._cookieJar.setCookieSync(sc, 'https://chat.z.ai/'); } catch {}
          }
        }
        this._fire(2);
        this._fire(3);
        let chunks = [];
        // 流式 UTF-8 解码器：正确处理 TCP 分包把多字节字符拆到两个 chunk 的情况。
        // 之前用 chunk.toString('utf8') 逐块累加，会把被拆开的 UTF-8 序列（如中文
        // 「期」的 3 字节被拆成 2+1）替换成 U+FFFD → 思考链里出现乱码。
        // TextDecoder('utf-8', { stream: true }) 会缓冲不完整的尾字节，跨 chunk 拼接。
        const decoder = new TextDecoder('utf-8');
        res.on('data', (chunk) => {
          chunks.push(chunk);
          this.responseText += decoder.decode(chunk, { stream: true });
          if (this.onprogress) { try { this.onprogress(); } catch {} }
          this._fire(3);
        });
        res.on('end', () => {
          // flush 解码器剩余缓冲（应为空，保险起见）
          const tail = decoder.decode();
          if (tail) this.responseText += tail;
          // 最终用 Buffer.concat 重算一次，保证 responseText 完全正确（覆盖流式累加）
          this.responseText = Buffer.concat(chunks).toString('utf8');
          this._fire(4);
          if (this.onload) { try { this.onload(); } catch {} }
        });
      });

      this._req.on('error', (e) => {
        // 代理失败：标记淘汰，换下一个代理重试
        if (this._proxyEntry) proxyPool.markBad(this._proxyEntry);
        attempt++;
        if (attempt < maxProxyRetries) {
          console.log(`[NodeXHR] 代理失败(${attempt}/${maxProxyRetries})，换下一个: ${e.message}`);
          this.responseText = ''; // 清空之前的数据
          trySend(); // 重试
        } else {
          if (this.onerror) { try { this.onerror(e); } catch {} }
        }
      });

      if (this.timeout > 0) this._req.setTimeout(this.timeout, () => {
        if (this._proxyEntry) proxyPool.markBad(this._proxyEntry);
        this._req.destroy();
        attempt++;
        if (attempt < maxProxyRetries) {
          console.log(`[NodeXHR] 代理超时(${attempt}/${maxProxyRetries})，换下一个`);
          this.responseText = '';
          trySend();
        } else {
          if (this.ontimeout) { try { this.ontimeout(); } catch {} }
        }
      });

      if (this._body) this._req.write(Buffer.from(this._body, 'utf8'));
      this._req.end();
    };

    trySend();
  }
  abort() { if (this._req) this._req.destroy(); }
}

// ─── JSDOM window 工厂 ───
function createWindow() {
  const vc = new VirtualConsole();
  // 静默 jsdom 噪声，只透传 error（便于排错）
  vc.on('jsdomError', () => {});

  const html = `<!DOCTYPE html><html><head></head><body>
<div id="cap" style="width:320px;height:200px"></div><button id="btn"></button>
<script src="https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"></script>
</body></html>`;

  const dom = new JSDOM(html, {
    url: 'https://chat.z.ai/',
    runScripts: 'dangerously',
    resources: 'usable',
    pretendToBeVisual: true,
    virtualConsole: vc,
    cookieJar: new CookieJar(),
    userAgent: UA,
    // 暴露 cookieJar 到 window，供 NodeXHR 提取阿里云安全 cookie（acw_tc/cdn_sec_tc）。
    // 直连 z.ai 时 NodeXHR 不带这些 cookie → ESA 抽样拦截 405。
    beforeParse(window) {
      window._cookieJar = dom.cookieJar;
      // 注意：不全局劫持 XMLHttpRequest。AliyunCaptcha SDK 依赖 JSDOM 原生 XHR 的完整实现，
      // 全局替换会导致 SDK 崩溃。只在 xhrSend/xhrSendStream 里用 NodeXHR 发 z.ai 请求。
      window.matchMedia = () => ({ matches:false, media:'', onchange:null, addListener(){}, removeListener(){}, addEventListener(){}, removeEventListener(){}, dispatchEvent(){return false;} });
      Object.defineProperty(window.navigator, 'webdriver', { get: () => false });
      Object.defineProperty(window.navigator, 'plugins', { get: () => [1, 2, 3, 4, 5] });
      Object.defineProperty(window.navigator, 'languages', { get: () => ['zh-CN', 'zh', 'en-US', 'en'] });
      window.chrome = { runtime: {}, loadTimes: () => ({}) };
      const proto = window.HTMLCanvasElement.prototype;
      proto.getContext = function (type) {
        if (/webgl/i.test(type)) return { canvas:this, getParameter:()=>'Intel', getExtension:()=>null, getSupportedExtensions:()=>['WEBGL_debug_renderer_info'], getContextAttributes:()=>({}), getShaderPrecisionFormat:()=>({precision:23,rangeMin:127,rangeMax:127}) };
        return { canvas:this, fillRect(){}, clearRect(){}, getImageData:(x,y,w=1,h=1)=>({data:new Uint8ClampedArray(w*h*4)}), putImageData(){}, createImageData:(w=1,h=1)=>({data:new Uint8ClampedArray(w*h*4)}), setTransform(){}, transform(){}, drawImage(){}, save(){}, restore(){}, beginPath(){}, moveTo(){}, lineTo(){}, bezierCurveTo(){}, quadraticCurveTo(){}, closePath(){}, clip(){}, stroke(){}, fill(){}, arc(){}, rect(){}, ellipse(){}, translate(){}, scale(){}, rotate(){}, fillText(){}, strokeText(){}, measureText:(t)=>({width:(''+t).length*8}), createLinearGradient:()=>({addColorStop(){}}), createRadialGradient:()=>({addColorStop(){}}), createPattern:()=>({}), isPointInPath:()=>false, font:'10px sans-serif', textBaseline:'alphabetic', textAlign:'start', fillStyle:'#000', strokeStyle:'#000', globalAlpha:1, lineWidth:1, shadowBlur:0, shadowColor:'' };
      };
      proto.toDataURL = () => 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';
      proto.toBlob = (cb) => cb && cb(null);
      window.Worker = class { constructor(){} postMessage(){} terminate(){} addEventListener(){} removeEventListener(){} onmessage=null; onerror=null; };
      window.OffscreenCanvas = window.OffscreenCanvas || class { constructor(w,h){this.width=w;this.height=h;} getContext(){return proto.getContext.call(this);} };
    },
  });
  return dom.window;
}

// ─── 窗口池：预热 N 个 armed window，每次请求用一个、用完丢弃、后台补一个 ───
// 每个 window 只用一次（避免在同 window 上重复 initAliyunCaptcha 的兼容性问题），
// 用完丢弃让 GC 回收，同时后台预热线程补一个新 window 进池。
// 并发上限 = POOL_SIZE。每个 JSDOM window ~50-80MB 内存，按机器内存调整。
const POOL_SIZE = Number(process.env.WINDOW_POOL_SIZE || 4);
const pool = [];          // armed window 队列
const waiters = [];       // 排队等待 window 的请求（promise resolve）
let warming = 0;          // 正在预热中的数量（防止过度补窗）

function waitFor(cond, t = 30000) {
  return new Promise((res, rej) => {
    const s = Date.now();
    const i = setInterval(() => { let ok=false; try{ok=cond();}catch{} if(ok){clearInterval(i);res();} else if(Date.now()-s>t){clearInterval(i);rej(new Error('timeout'));} }, 80);
  });
}

// 创建并预热一个 window（等 AliyunCaptcha SDK 就绪）
async function warmWindow() {
  const w = createWindow();
  await waitFor(() => typeof w.initAliyunCaptcha === 'function', 30000);
  return w;
}

// 维持池子处于 POOL_SIZE 个 armed window。低于阈值就后台补。
async function refillPool() {
  // 目标 armed = POOL_SIZE；已 armed(pool) + 正在 warming 的，少于目标就补
  while (pool.length + warming < POOL_SIZE) {
    warming++;
    warmWindow().then((w) => {
      warming--;
      // 优先满足排队等待者，否则入池
      if (waiters.length > 0) {
        waiters.shift()(w);
      } else {
        pool.push(w);
      }
    }).catch((e) => {
      warming--;
      console.error('[chat-proxy] warmWindow failed:', e.message);
    });
  }
}

// 取一个 armed window；池空则排队等后台补的 window
function acquireWindow(timeoutMs = 120000) {
  if (pool.length > 0) {
    const w = pool.shift();
    refillPool(); // 取走后立刻补
    return Promise.resolve(w);
  }
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('acquire window timeout')), timeoutMs);
    waiters.push((w) => { clearTimeout(timer); resolve(w); });
    refillPool();
  });
}

// 启动时预热池
async function initPool() {
  console.log(`[chat-proxy] warming ${POOL_SIZE} JSDOM windows...`);
  await refillPool();
}

// 从 JSDOM window 的 CookieJar 提取 cookie 字符串（异步）。
// 预热阶段访问 z.ai 会拿到阿里云安全 cookie（acw_tc/cdn_sec_tc/ssxmod_itna），
// 这些 cookie 对 ESA 通过风控至关重要。NodeXHR 绕过了 JSDOM 的 XHR（用原生 https），
// 不会自动带 cookieJar 的 cookie，必须手动提取注入。
function getCookiesForUrl(window, url) {
  return new Promise((resolve) => {
    const jar = window._cookieJar;
    if (!jar || typeof jar.getCookies !== 'function') {
      resolve('');
      return;
    }
    // cookie 是 chat.z.ai 域的，即使请求经 CF Worker 转发，cookie 也应带上
    // （CF Worker 透传 Cookie 头给 z.ai）。
    try {
      jar.getCookies(url, (err, cookies) => {
        if (err || !cookies || !cookies.length) {
          resolve('');
          return;
        }
        resolve(cookies.map(c => `${c.key}=${c.value}`).join('; '));
      });
    } catch {
      resolve('');
    }
  });
}

// 用 window 的 XHR 发请求，返回 {status, body, headers}
async function xhrSend(window, method, url, headers, body, timeoutMs = 60000) {
  // 注入 CookieJar 的 cookie（阿里云安全 cookie）
  const cookieStr = await getCookiesForUrl(window, 'https://chat.z.ai/');
  const finalHeaders = { ...(headers || {}) };
  if (cookieStr && !finalHeaders['Cookie'] && !finalHeaders['cookie']) {
    finalHeaders['Cookie'] = cookieStr;
  }
  headers = finalHeaders;
  return new Promise((resolve, reject) => {
    const xhr = new NodeXHR(window._cookieJar);
    xhr.open(method, url);
    if (headers) for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
    xhr.onload = () => {
      const hdrs = {};
      try { xhr.getAllResponseHeaders().split('\r\n').forEach(l => { if(l){const idx=l.indexOf(': '); if(idx>0) hdrs[l.slice(0,idx).toLowerCase()] = l.slice(idx+2);} }); } catch {}
      resolve({ status: xhr.status, body: xhr.responseText, headers: hdrs });
    };
    xhr.onerror = () => reject(new Error('xhr onerror'));
    xhr.ontimeout = () => reject(new Error('xhr timeout'));
    xhr.timeout = timeoutMs;
    xhr.send(body);
  });
}

// 流式 XHR：onprogress 时把 responseText 的增量切片实时回调出去。
// onChunk(chunk)、onHeaders(status, headers) 在首个 progress/readyState=2 时触发一次，
// resolve({status, headers, full}) 在 onload 时触发（full = 完整拼接）。
// 适合上游 SSE：边收边 pipe 给 HTTP 响应，降低 time-to-first-token。
async function xhrSendStream(window, method, url, headers, body, { onChunk, onHeaders, timeoutMs = 90000 } = {}) {
  // 注入 CookieJar 的 cookie（阿里云安全 cookie）
  const cookieStr = await getCookiesForUrl(window, 'https://chat.z.ai/');
  if (cookieStr) {
    headers = { ...(headers || {}), Cookie: cookieStr };
  }
  return new Promise((resolve, reject) => {
    const xhr = new NodeXHR(window._cookieJar);
    xhr.open(method, url);
    if (headers) for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
    let sent = 0;            // 已 flush 的 responseText 长度
    let headersSent = false;
    const emitHeaders = () => {
      if (headersSent) return;
      headersSent = true;
      const hdrs = {};
      try { xhr.getAllResponseHeaders().split('\r\n').forEach(l => { if(l){const idx=l.indexOf(': '); if(idx>0) hdrs[l.slice(0,idx).toLowerCase()] = l.slice(idx+2);} }); } catch {}
      if (onHeaders) onHeaders(xhr.status, hdrs);
    };
    const pump = () => {
      if (xhr.readyState >= 2) emitHeaders();
      if (xhr.readyState === 3 || xhr.readyState === 4) {
        const total = xhr.responseText || '';
        if (total.length > sent) {
          const chunk = total.slice(sent);
          sent = total.length;
          if (onChunk) onChunk(chunk);
        }
      }
    };
    xhr.onprogress = pump;
    xhr.onreadystatechange = pump;
    xhr.onload = () => {
      pump();
      const hdrs = {};
      try { xhr.getAllResponseHeaders().split('\r\n').forEach(l => { if(l){const idx=l.indexOf(': '); if(idx>0) hdrs[l.slice(0,idx).toLowerCase()] = l.slice(idx+2);} }); } catch {}
      resolve({ status: xhr.status, headers: hdrs, full: xhr.responseText || '' });
    };
    xhr.onerror = () => reject(new Error('xhr onerror'));
    xhr.ontimeout = () => reject(new Error('xhr timeout'));
    xhr.timeout = timeoutMs;
    xhr.send(body);
  });
}

// ─── 全链路处理（流式）───
// handleChat(req, callbacks):
//   callbacks.onHeaders(status, headers) — 上游响应头到达时调一次
//   callbacks.onChunk(sseString)          — 上游 SSE 增量，实时 pipe
// 返回 { status, headers, full }（onload 时，full 为完整拼接）
async function handleChat(req, callbacks = {}) {
  const { token, upstream_model, enable_thinking, auto_web_search, reasoning_effort, signature_prompt, messages, files, extra_body, context_file_uploaded } = req;
  if (!token) throw new Error('missing token');
  if (!upstream_model) throw new Error('missing upstream_model');

  // reasoning_effort 映射：z.ai 前端只接受 "high" / "max"（默认 max）。
  // OpenAI 风格取值 low/minimal/medium → high，high/max → max。
  // 仅在 enable_thinking 时有意义。
  function mapEffort(v) {
    if (!v) return '';
    const lv = String(v).toLowerCase();
    if (lv === 'max') return 'max';
    if (lv === 'high') return 'max';
    // low / medium / minimal / 其他 → high（z.ai 最低档）
    return 'high';
  }
  const effort = enable_thinking ? mapEffort(reasoning_effort) : '';

  const userId = jwtUserId(token);
  const window = await acquireWindow(); // 每个 window 只用一次

  try {
    // 1) 拿 captcha（embed traceless；network error 可忽略，success 仍触发）
    const captchaParam = await new Promise((resolve, reject) => {
      const settle = (fn) => { try { fn(); } catch (e) { reject(e); } };
      window.initAliyunCaptcha({
        SceneId: SCENE, mode: MODE, region: REGION, prefix: PREFIX,
        element: '#cap', button: '#btn', captchaLogoImg: '', showErrorTip: false,
        getInstance: (inst) => settle(() => (inst.startTracelessVerification || inst.verify || inst.show).call(inst)),
        success: (param) => resolve(param),
        fail: (r) => reject(new Error('captcha fail: ' + JSON.stringify(r))),
        onError: (e) => reject(new Error('captcha onError: ' + JSON.stringify(e))),
      });
      setTimeout(() => reject(new Error('captcha timeout')), 35000);
    });

    // 2) 预热 cookie + 建会话
    await xhrSend(window, 'GET', 'https://chat.z.ai/', null, null, 15000).catch(() => {});
    const newChat = await xhrSend(window, 'POST', 'https://chat.z.ai/api/v1/chats/new',
      { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json', 'Accept-Language': ACCEPT_LANGUAGE, 'X-FE-Version': FE_VERSION, 'User-Agent': UA },
      JSON.stringify({ chat: { title: (signature_prompt || 'hi').slice(0, 40) } }), 15000);
    let chatId = crypto.randomUUID();
    try { const p = JSON.parse(newChat.body); chatId = p.id || (p.data && p.data.id) || chatId; } catch {}

    // 3) 组装 completions body
    const ts = Date.now();
    const requestId = crypto.randomUUID();
    const userMsgId = crypto.randomUUID();
    const sig = genSig(userId, requestId, signature_prompt || '', ts);

    const qs = new window.URLSearchParams({
      timestamp: String(ts), requestId, user_id: userId, version: '0.0.1', platform: 'web',
      token,
      user_agent: UA, language: 'zh-CN', languages: 'zh-CN,en-US,en', timezone: 'Asia/Shanghai', cookie_enabled: 'true',
      screen_width: '2400', screen_height: '1080', screen_resolution: '2400x1080',
      viewport_height: '945', viewport_width: '1845', viewport_size: '1845x945',
      color_depth: '32', pixel_ratio: '1',
      current_url: `https://chat.z.ai/c/${chatId}`, pathname: `/c/${chatId}`,
      search: '', hash: '', host: 'chat.z.ai', hostname: 'chat.z.ai', protocol: 'https:',
      referrer: '', title: 'Z.ai - Advanced AI Chatbot & Agent powered by GLM-5.2',
      timezone_offset: String(new Date().getTimezoneOffset()),
      local_time: new Date(ts).toISOString(),
      utc_time: new Date(ts).toUTCString(),
      is_mobile: 'false', is_touch: 'false', max_touch_points: '10',
      browser_name: 'Chrome', os_name: 'Windows', signature_timestamp: String(ts),
    }).toString();

    // z.ai 后端不读 messages 数组里的历史（只看 chat_id 的服务端历史）。
    // 我们每次新建 chat_id，所以多轮上下文需要自己补。两种策略（Go 侧决定）：
    //   1) 优先：Go 已把完整历史上传成 .txt 文件附到 files 数组（context_file_uploaded=true），
    //      此时无需再合并，直接透传 messages（历史 z.ai 会忽略，模型读文件即可）。
    //   2) 兜底：上传失败/未启用时（context_file_uploaded=false），把历史合并到最后一条 user message。
    let upstreamMessages = messages;
    const ctxFileUploaded = !!req.context_file_uploaded;
    if (!ctxFileUploaded && messages && messages.length > 1) {
      const lastMsg = messages[messages.length - 1];
      if (lastMsg.role === 'user') {
        // 把前面的对话作为上下文拼到最后一条 user message 里
        const history = messages.slice(0, -1).map(m => {
          const role = m.role === 'assistant' ? 'AI' : (m.role === 'system' ? '系统' : '用户');
          return `${role}: ${typeof m.content === 'string' ? m.content : JSON.stringify(m.content)}`;
        }).join('\n');
        const lastContent = typeof lastMsg.content === 'string' ? lastMsg.content : JSON.stringify(lastMsg.content);
        upstreamMessages = [{
          role: 'user',
          content: `以下是之前的对话历史：\n${history}\n\n用户最新消息：${lastContent}`,
        }];
      }
    }

    const body = {
      stream: true,
      model: upstream_model,
      messages: upstreamMessages,
      signature_prompt: signature_prompt || '',
      params: {}, extra: {},
      features: {
        image_generation: false,
        // 前端硬编码 web_search: false，真正的搜索开关是 auto_web_search
        web_search: false,
        auto_web_search: !!auto_web_search,
        preview_mode: !!enable_thinking,
        flags: [],
        vlm_tools_enable: false, vlm_web_search_enable: false, vlm_website_mode: false,
        enable_thinking: !!enable_thinking,
        // GLM-5.2 思考强度：z.ai 前端只接受 "high" / "max"。
        // 仅在 enable_thinking 且非搜索模式时传（前端逻辑：capabilities.reasoning_effort && thinking && !search）。
        ...(effort && !auto_web_search ? { reasoning_effort: effort } : {}),
      },
      variables: nowVars(),
      chat_id: chatId,
      id: crypto.randomUUID(),
      current_user_message_id: userMsgId,
      current_user_message_parent_id: null,
      background_tasks: { title_generation: true, tags_generation: true },
      captcha_verify_param: captchaParam,
    };
    // extra_body：透传额外的 body 字段（如原生 tools/tool_choice 实验用），覆盖同名默认字段
    if (extra_body && typeof extra_body === 'object') {
      Object.assign(body, extra_body);
    }
    if (files && files.length > 0) {
      body.files = files;
      body.current_user_message_id = userMsgId;
    }

    // 4) 发 completions（流式：onprogress 增量实时回调给 onChunk）
    const resp = await xhrSendStream(window, 'POST', `https://chat.z.ai/api/v2/chat/completions?${qs}`,
      {
        'Authorization': `Bearer ${token}`,
        'X-FE-Version': FE_VERSION,
        'X-Signature': sig,
        'Content-Type': 'application/json',
        'Accept-Language': ACCEPT_LANGUAGE,
        'X-Region': 'overseas',
        'Accept': '*/*',
        'Origin': 'https://chat.z.ai',
        'Referer': `https://chat.z.ai/c/${chatId}`,
        'User-Agent': UA,
        'Sec-Fetch-Dest': 'empty',
        'Sec-Fetch-Mode': 'cors',
        'Sec-Fetch-Site': 'same-origin',
        'sec-ch-ua': '"Google Chrome";v="149", "Chromium";v="149", "Not)A;Brand";v="24"',
        'sec-ch-ua-mobile': '?0',
        'sec-ch-ua-platform': '"Windows"',
      },
      JSON.stringify(body),
      // completions 超时：之前 300000(5分钟) 太长。如果 z.ai 卡住不返回任何字节，
      // 这个请求会占用 JSDOM 窗口 5 分钟才超时释放。WINDOW_POOL_SIZE=3，3 个请求
      // 同时卡住 → 后续请求全部排队等窗口 → 整个服务卡死（"运行久了卡死"根因）。
      // GLM-5.2 思考+回复通常 30-60s，90s 足够；超时则快速失败让 Go 重试换 token。
      { onChunk: callbacks.onChunk, onHeaders: callbacks.onHeaders, timeoutMs: 90000 });

    return resp;
  } finally {
    // window 用完即弃：移除引用让 GC 回收（池在 acquire 时已后台补新 window）
    // 不主动 dom.window.close()，避免影响正在进行的异步；GC 会处理。
  }
}

// 方案二：只拿 captcha token + chatId（Go tls-client 自己发 chat）。
// 适用于 Node 直连 z.ai 被风控的场景：JSDOM 拿 captcha，Go tls-client 发 chat。
// 返回 { ok, captcha_verify_param, chat_id, user_msg_id }
async function getCaptchaAndChatId(req) {
  const { token } = req;
  if (!token) throw new Error('missing token');

  const window = await acquireWindow();
  try {
    // 1) 拿 captcha
    const captchaParam = await new Promise((resolve, reject) => {
      const settle = (fn) => { try { fn(); } catch (e) { reject(e); } };
      window.initAliyunCaptcha({
        SceneId: SCENE, mode: MODE, region: REGION, prefix: PREFIX,
        element: '#cap', button: '#btn', captchaLogoImg: '', showErrorTip: false,
        getInstance: (inst) => settle(() => (inst.startTracelessVerification || inst.verify || inst.show).call(inst)),
        success: (param) => resolve(param),
        fail: (r) => reject(new Error('captcha fail: ' + JSON.stringify(r))),
        onError: (e) => reject(new Error('captcha onError: ' + JSON.stringify(e))),
      });
      setTimeout(() => reject(new Error('captcha timeout')), 35000);
    });

    // 2) 建 chatId（用 NodeXHR，只是 chats/new 不走风控）
    const newChat = await xhrSend(window, 'POST', 'https://chat.z.ai/api/v1/chats/new',
      { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json', 'Accept-Language': ACCEPT_LANGUAGE, 'X-FE-Version': FE_VERSION, 'User-Agent': UA },
      JSON.stringify({ chat: { title: 'hi' } }), 15000);
    let chatId = crypto.randomUUID();
    try { const p = JSON.parse(newChat.body); chatId = p.id || (p.data && p.data.id) || chatId; } catch {}

    const userMsgId = crypto.randomUUID();
    return { ok: true, captcha_verify_param: captchaParam, chat_id: chatId, user_msg_id: userMsgId };
  } finally {
    // window 用完即弃
  }
}

module.exports = { handleChat, initPool, createWindow, getCaptchaAndChatId };
