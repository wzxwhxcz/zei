FROM golang:1.25-bookworm AS builder
ENV GOPROXY=https://goproxy.cn,direct
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /zai2api ./cmd/main.go

FROM node:20-bookworm
WORKDIR /app
COPY --from=builder /zai2api /app/zai2api
COPY captcha-provider /app/captcha-provider
RUN cd /app/captcha-provider && npm install --omit=dev
COPY . .

# 默认端口；HF Spaces 需覆盖为 7860（app_port 或在 Space 设置 PORT=7860）
ENV PORT=8000
ENV HOST=0.0.0.0
ENV CAPTCHA_PROVIDER_URL=http://127.0.0.1:9876
ENV CAPTCHA_FULL_PROXY_URL=http://127.0.0.1:9876

RUN printf '%s\n' \
  '#!/bin/bash' \
  'echo "===== Application Startup at $(date "+%Y-%m-%d %H:%M:%S") ====="' \
  'echo "Starting Node.js Captcha Provider..."' \
  '(cd /app/captcha-provider && node server.js) & PROVIDER_PID=$!' \
  'for i in $(seq 1 30); do' \
  '  if curl -sf http://127.0.0.1:9876/health >/dev/null 2>&1; then break; fi' \
  '  sleep 1' \
  'done' \
  'echo "Starting Go Proxy on port ${PORT}..."' \
  'cd /app' \
  './zai2api & PROXY_PID=$!' \
  'wait -n $PROVIDER_PID $PROXY_PID' \
  'kill $PROVIDER_PID $PROXY_PID 2>/dev/null' \
  'exit 1' > /app/start.sh && chmod +x /app/start.sh

EXPOSE 8000
CMD ["/app/start.sh"]
