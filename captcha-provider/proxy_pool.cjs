// proxy_pool.js — ProxyScrape 免费代理池，自动抓取 + 轮换 + 失败淘汰
//
// 用法：
//   const pool = require('./proxy_pool.cjs');
//   pool.start();  // 启动后台抓取
//   const agent = pool.nextAgent();  // 取一个代理 agent（http/socks5 都支持）
//   pool.markBad(agent);  // 标记失败（淘汰）
//
// 配置（env）：
//   PROXY_PROTOCOL: socks5（默认，最快）| http | all
//   PROXY_COUNTRY: all（默认）| cn,us,hk,...
//   PROXY_TIMEOUT: 5000（默认，最大延迟 ms）
//   PROXY_LIST_URL: 自定义代理列表 URL（覆盖 ProxyScrape）
//   HTTPS_PROXY: 手动指定固定代理（设了就不用池，直接用这个）

const https = require('https');
const http = require('http');

let agents = [];        // [{ url, agent, fails: 0, lastUsed: 0 }]
let badSet = new Set(); // 失败过的代理 URL
let lastFetch = 0;
let started = false;

const FETCH_INTERVAL = 5 * 60 * 1000; // 5 分钟刷新一次列表
const MAX_FAILS = 2;                   // 失败 2 次淘汰

function buildListUrl() {
  if (process.env.PROXY_LIST_URL) return process.env.PROXY_LIST_URL;
  return ''; // 默认不使用 ProxyScrape，改由 PROXY_POOL_API 从 go_proxy_pool 取
}

// fetchFromProxyPoolAPI 从 go_proxy_pool 等 API 取代理（JSON 格式）。
// 设了 PROXY_POOL_API 时优先用这个（质量比 ProxyScrape 高，有存活验证）。
async function fetchFromProxyPoolAPI() {
  const apiUrl = process.env.PROXY_POOL_API || '';
  if (!apiUrl) return [];
  return new Promise((resolve) => {
    const lib = apiUrl.startsWith('https:') ? https : http;
    const req = lib.get(apiUrl, { timeout: 15000 }, (res) => {
      let data = '';
      res.on('data', c => data += c);
      res.on('end', () => {
        try {
          const parsed = JSON.parse(data);
          console.log(`[proxy-pool] API 原始响应前100字符: ${data.substring(0, 100)}`);
          // go_proxy_pool 格式：count=1 返回单个对象，count>1 返回数组
          const arr = Array.isArray(parsed) ? parsed : [parsed];
          const urls = arr
            .filter(p => p.Ip && p.Port)
            .map(p => {
              // HTTPS/SOCKET5 类型用对应协议前缀
              const proto = (p.Type || 'HTTP').includes('SOCKET') ? 'socks5' : 'http';
              return `${proto}://${p.Ip}:${p.Port}`;
            });
          console.log(`[proxy-pool] 从代理池API获取 ${urls.length} 个代理（原始 ${arr.length} 条）`);
          if (urls.length === 0) console.error(`[proxy-pool] 代理池API返回但过滤后为空！第一条数据: ${JSON.stringify(arr[0])}`);
          resolve(urls);
        } catch (e) {
          console.error(`[proxy-pool] 代理池API解析失败: ${e.message}`);
          resolve([]);
        }
      });
    });
    req.on('error', (e) => {
      console.error(`[proxy-pool] 代理池API请求失败: ${e.message}`);
      resolve([]);
    });
    req.on('timeout', () => { req.destroy(); resolve([]); });
  });
}

// 检测 socks-proxy-agent 是否可用（某些环境 npm install 会因 peer dep 冲突漏装）
let _socksAvailable = null; // null=未检测, true/false
function isSocksAvailable() {
  if (_socksAvailable !== null) return _socksAvailable;
  try {
    require('socks-proxy-agent');
    _socksAvailable = true;
  } catch {
    _socksAvailable = false;
    console.warn('[proxy-pool] socks-proxy-agent 不可用，SOCKS 代理将被跳过（HTTP 代理正常）');
  }
  return _socksAvailable;
}

function createAgent(proxyUrl) {
  // socks5:// → SocksProxyAgent；http:// → HttpsProxyAgent
  try {
    if (proxyUrl.startsWith('socks')) {
      if (!isSocksAvailable()) return null; // SOCKS 模块不可用，跳过
      const { SocksProxyAgent } = require('socks-proxy-agent');
      return new SocksProxyAgent(proxyUrl);
    } else {
      const { HttpsProxyAgent } = require('https-proxy-agent');
      return new HttpsProxyAgent(proxyUrl);
    }
  } catch (e) {
    console.error(`[proxy-pool] createAgent 失败 ${proxyUrl}: ${e.message}`);
    return null;
  }
}

async function fetchList() {
  const url = buildListUrl();
  console.log(`[proxy-pool] 抓取代理列表: ${url.substring(0, 80)}...`);
  return new Promise((resolve) => {
    const lib = url.startsWith('https:') ? https : http;
    const req = lib.get(url, { timeout: 15000 }, (res) => {
      let data = '';
      res.on('data', c => data += c);
      res.on('end', () => {
        const lines = data.split('\n').map(s => s.trim()).filter(s => s && !s.startsWith('#'));
        console.log(`[proxy-pool] 获取 ${lines.length} 个代理`);
        resolve(lines);
      });
    });
    req.on('error', (e) => {
      console.error(`[proxy-pool] 抓取失败: ${e.message}`);
      resolve([]);
    });
    req.on('timeout', () => { req.destroy(); resolve([]); });
  });
}

function refreshAgents(proxyUrls) {
  console.log(`[proxy-pool] refreshAgents: 输入 ${proxyUrls.length} 个 URL: ${proxyUrls.slice(0,3).join(', ')}`);
  const newAgents = [];
  let failCount = 0;
  for (const url of proxyUrls) {
    if (badSet.has(url)) continue; // 跳过已淘汰的
    const agent = createAgent(url);
    if (agent) {
      newAgents.push({ url, agent, fails: 0, lastUsed: 0 });
    } else {
      failCount++;
    }
  }
  console.log(`[proxy-pool] refreshAgents: createAgent 成功 ${newAgents.length}/${proxyUrls.length}`);
  if (proxyUrls.length > 0 && newAgents.length === 0) {
    console.error(`[proxy-pool] 全部 ${proxyUrls.length} 个代理 createAgent 失败！前3个原始格式: ${proxyUrls.slice(0, 3).join(', ')}`);
  }
  // 保留之前还活着的（避免频繁重建 agent 对象）
  const oldAlive = agents.filter(a => a.fails < MAX_FAILS && !badSet.has(a.url));
  const seen = new Set(newAgents.map(a => a.url).concat(oldAlive.map(a => a.url)));
  const merged = [];
  for (const a of oldAlive) merged.push(a);
  for (const a of newAgents) if (!seen.has(a.url)) { merged.push(a); seen.add(a.url); }
  agents = merged;
  console.log(`[proxy-pool] 可用代理: ${agents.length} 个（淘汰 ${badSet.size} 个）`);
}

async function start() {
  if (started) return;
  started = true;
  // 手动固定代理模式：设了 HTTPS_PROXY 就不用池
  const fixed = process.env.HTTPS_PROXY || process.env.HTTP_PROXY || process.env.ALL_PROXY || '';
  if (fixed) {
    console.log(`[proxy-pool] 固定代理模式: ${fixed.replace(/\/\/.*@/, '//***@')}`);
    const agent = createAgent(fixed);
    if (agent) agents = [{ url: fixed, agent, fails: 0, lastUsed: 0 }];
    return;
  }

  // 取代理的统一函数：优先 PROXY_POOL_API（go_proxy_pool），回退 ProxyScrape
  const fetchProxies = async () => {
    if (process.env.PROXY_POOL_API) {
      return await fetchFromProxyPoolAPI();
    }
    return await fetchList();
  };

  // 池模式：抓取 + 定时刷新
  const list = await fetchProxies();
  refreshAgents(list);
  setInterval(async () => {
    if (agents.filter(a => a.fails < MAX_FAILS).length < 5) {
      // 活着的太少，重新抓
      const l = await fetchProxies();
      refreshAgents(l);
      badSet.clear(); // 给被淘汰的第二次机会
      lastFetch = Date.now();
    }
  }, FETCH_INTERVAL);
}

let rrIndex = 0;
function nextAgent() {
  if (agents.length === 0) return null;
  // 轮询取一个没失败的
  for (let i = 0; i < agents.length; i++) {
    const idx = (rrIndex + i) % agents.length;
    const a = agents[idx];
    if (a.fails < MAX_FAILS) {
      rrIndex = idx + 1;
      a.lastUsed = Date.now();
      return a;
    }
  }
  return null; // 全失败了
}

function markBad(agentEntry) {
  if (!agentEntry) return;
  agentEntry.fails++;
  if (agentEntry.fails >= MAX_FAILS) {
    badSet.add(agentEntry.url);
    console.log(`[proxy-pool] 淘汰代理: ${agentEntry.url}（失败 ${agentEntry.fails} 次）`);
  }
}

function stats() {
  return {
    total: agents.length,
    alive: agents.filter(a => a.fails < MAX_FAILS).length,
    bad: badSet.size,
  };
}

module.exports = { start, nextAgent, markBad, stats };
