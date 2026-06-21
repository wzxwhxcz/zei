// Cloudflare Worker — chat.z.ai 反向代理
//
// 部署：Cloudflare Dashboard → Workers → Create Worker → 粘贴此代码 → Deploy
// 然后把 Worker 的域名（如 https://zai-proxy.your-name.workers.dev）设为
// captcha-provider 的 UPSTREAM_BASE_URL 环境变量。
//
// 原理：HF Spaces → CF Worker → chat.z.ai
// CF 的 IP 不被阿里云 CDN 风控，解决 HF 数据中心 IP 405 问题。

const UPSTREAM = 'https://chat.z.ai';

export default {
  async fetch(request) {
    const url = new URL(request.url);
    const upstreamUrl = UPSTREAM + url.pathname + url.search;

    // 转发请求，保留 method/headers/body
    const headers = new Headers(request.headers);
    // CF Worker 会加一些自己的 header，去掉避免上游识别
    headers.delete('cf-connecting-ip');
    headers.delete('cf-ipcountry');
    headers.delete('cf-ray');
    headers.delete('cf-visitor');
    headers.delete('x-forwarded-for');
    headers.delete('x-forwarded-proto');
    headers.delete('x-real-ip');

    const upstreamReq = new Request(upstreamUrl, {
      method: request.method,
      headers: headers,
      body: request.method !== 'GET' && request.method !== 'HEAD' ? request.body : null,
      redirect: 'follow',
    });

    const resp = await fetch(upstreamReq);

    // 返回响应，去掉 CF 加的 header
    const respHeaders = new Headers(resp.headers);
    respHeaders.delete('cf-ray');
    respHeaders.delete('cf-cache-status');
    // 关键：CF Worker 的 fetch() 会自动解压 br/gzip 响应体，resp.body 已是明文。
    // 但 resp.headers 里的 Content-Encoding 仍保留（如 "br"），下游（provider 的
    // NodeXHR）看到这个头会误以为响应还是压缩的，可能尝试再次解压明文 → 字节损坏
    // （表现：思考链 reasoning 里个别中文字符变成 U+FFFD 乱码）。
    // 删掉这个头，让下游把 body 当明文处理。
    respHeaders.delete('content-encoding');
    // 同理 Content-Length 是压缩后的长度，跟解压后的明文长度不符，删掉避免下游截断。
    respHeaders.delete('content-length');

    return new Response(resp.body, {
      status: resp.status,
      statusText: resp.statusText,
      headers: respHeaders,
    });
  },
};
