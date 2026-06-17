package main

import (
	"encoding/json"
	"net/http"
	"time"

	"zai-proxy/internal"
)

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Credentials", "true")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}
func loggingMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// 获取客户端 IP
		clientIP := internal.GetClientIP(r)

		next(wrapped, r)

		duration := time.Since(start)
		internal.LogInfo("%s %s %d %v [%s]", r.Method, r.URL.Path, wrapped.statusCode, duration, clientIP)
	}
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	telemetry := internal.GetTelemetryData()

	response := map[string]interface{}{
		"message": "Chat-Glm API",
		"version": "2.0.0",
		"telemetry": map[string]interface{}{
			"uptime":              telemetry.Uptime,
			"total_requests":      telemetry.TotalRequests,
			"rpm":                 telemetry.RPM,
			"total_input_tokens":  telemetry.TotalInputTok,
			"total_output_tokens": telemetry.TotalOutputTok,
			"avg_input_tokens":    telemetry.AvgInputTok,
			"avg_output_tokens":   telemetry.AvgOutputTok,
			"valid_tokens":        telemetry.ValidTokens,
			"multimodal_calls":    telemetry.MultimodalCalls,
			"total_calls":         telemetry.TotalCalls,
			"success_calls":       telemetry.SuccessCalls,
			"success_rate":        telemetry.SuccessRate,
			"model_stats":         telemetry.ModelStats,
		},
	}
	if len(internal.Cfg.Note) > 0 {
		response["note"] = internal.Cfg.Note
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func main() {
	internal.LoadConfig()
	internal.InitLogger()
	if err := internal.InitStorage(); err != nil {
		internal.LogError("存储后端初始化失败，回退到内存模式: %v", err)
	}
	internal.StartUsageLogger()
	internal.InitRedis()

	// Captcha 是 z.ai 的硬性要求：必须配 CAPTCHA_FULL_PROXY_URL（全代理，推荐）
	// 或 CAPTCHA_PROVIDER_URL（老路径）。两个都不设 → 每个请求都会被上游拒。
	if internal.Cfg.CaptchaFullProxyURL == "" && internal.Cfg.CaptchaProviderURL == "" {
		internal.LogError("========================================================")
		internal.LogError("⚠️  未配置任何 Captcha 后端！")
		internal.LogError("   CAPTCHA_FULL_PROXY_URL 和 CAPTCHA_PROVIDER_URL 都为空。")
		internal.LogError("   z.ai 强制校验 captcha，所有请求必将返回 FRONTEND_CAPTCHA_REQUIRED。")
		internal.LogError("   请至少设置一个（推荐 CAPTCHA_FULL_PROXY_URL=http://127.0.0.1:9876）。")
		internal.LogError("========================================================")
	}

	if err := internal.GetTokenManager().Start(); err != nil {
		internal.LogError("TokenManager 启动失败: %v", err)
	}

	internal.StartAnonymousTokenPool()
	internal.StartVersionUpdater()
	internal.StartModelFetcher()
	internal.GetApiKeyManager().StartPeriodicSave()
	http.HandleFunc("/", corsMiddleware(loggingMiddleware(handleRoot)))
	http.HandleFunc("/v1/models", corsMiddleware(loggingMiddleware(internal.HandleModels)))
	http.HandleFunc("/v1/chat/completions", corsMiddleware(loggingMiddleware(internal.HandleChatCompletions)))

	// Admin Web UI
	http.HandleFunc("/admin", corsMiddleware(loggingMiddleware(internal.HandleAdminUI)))
	http.HandleFunc("/admin/", corsMiddleware(loggingMiddleware(internal.HandleAdminUI)))
	http.HandleFunc("/admin/api/login", corsMiddleware(loggingMiddleware(internal.HandleAdminLogin)))
	http.HandleFunc("/admin/api/logout", corsMiddleware(loggingMiddleware(internal.HandleAdminLogout)))
	http.HandleFunc("/admin/api/overview", corsMiddleware(loggingMiddleware(internal.HandleAdminOverview)))
	http.HandleFunc("/admin/api/config", corsMiddleware(loggingMiddleware(internal.HandleAdminConfig)))

	// Tokens：GET 列表 / POST 添加 / DELETE 删除 / POST validate 验证
	http.HandleFunc("/admin/api/tokens", corsMiddleware(loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			internal.HandleAdminTokens(w, r)
		case http.MethodPost:
			internal.HandleAdminTokenAdd(w, r)
		case http.MethodDelete:
			internal.HandleAdminTokenDelete(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})))
	http.HandleFunc("/admin/api/tokens/validate", corsMiddleware(loggingMiddleware(internal.HandleAdminTokenValidate)))

	// API Keys
	http.HandleFunc("/admin/api/keys", corsMiddleware(loggingMiddleware(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			internal.HandleAdminKeysList(w, r)
		case http.MethodPost:
			internal.HandleAdminKeysCreate(w, r)
		case http.MethodDelete:
			internal.HandleAdminKeysDelete(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})))
	http.HandleFunc("/admin/api/keys/toggle", corsMiddleware(loggingMiddleware(internal.HandleAdminKeysToggle)))

	http.HandleFunc("/admin/api/models", corsMiddleware(loggingMiddleware(internal.HandleAdminModels)))
	http.HandleFunc("/admin/api/test", corsMiddleware(loggingMiddleware(internal.HandleAdminTestModel)))
	http.HandleFunc("/admin/api/usage", corsMiddleware(loggingMiddleware(internal.HandleAdminUsage)))


	addr := ":" + internal.Cfg.Port
	internal.LogInfo("Server starting on %s", addr)
	internal.LogInfo("API docs available at http://localhost:%s/v1/models", internal.Cfg.Port)
	if err := http.ListenAndServe(addr, nil); err != nil {
		internal.LogError("Server failed: %v", err)
	}
}
