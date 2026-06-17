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
    beforeParse(window) {
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
const POOL_SIZE = Number(process.env.WINDOW_POOL_SIZE || 2);
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

// 用 window 的 XHR 发请求，返回 {status, body, headers}
function xhrSend(window, method, url, headers, body, timeoutMs = 60000) {
  return new Promise((resolve, reject) => {
    const xhr = new window.XMLHttpRequest();
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
function xhrSendStream(window, method, url, headers, body, { onChunk, onHeaders, timeoutMs = 300000 } = {}) {
  return new Promise((resolve, reject) => {
    const xhr = new window.XMLHttpRequest();
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
  const { token, upstream_model, enable_thinking, auto_web_search, reasoning_effort, signature_prompt, messages, files, extra_body } = req;
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
      { 'Authorization': `Bearer ${token}`, 'Content-Type': 'application/json', 'Accept-Language': ACCEPT_LANGUAGE, 'X-FE-Version': FE_VERSION },
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

    const body = {
      stream: true,
      model: upstream_model,
      messages,
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
      },
      JSON.stringify(body),
      { onChunk: callbacks.onChunk, onHeaders: callbacks.onHeaders, timeoutMs: 300000 });

    return resp;
  } finally {
    // window 用完即弃：移除引用让 GC 回收（池在 acquire 时已后台补新 window）
    // 不主动 dom.window.close()，避免影响正在进行的异步；GC 会处理。
  }
}

module.exports = { handleChat, initPool, createWindow };
