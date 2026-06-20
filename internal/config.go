package internal

import (
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	// Server
	Port string

	// API Configuration
	APIEndpoint         string
	BrowserBridgeURL    string
	BrowserBridgeSecret string
	AuthTokens          []string // 支持多个 API Key（逗号分隔）
	BackupTokens        []string // 支持多个 Backup Token（用于多模态，逗号分隔）

	// Captcha Provider
	// CaptchaProviderURL 老路径：Go 直连时从这里取 captcha token 注入请求。
	// 日常用 CaptchaFullProxyURL（全代理），此项只在 /admin 健康检查 + 回退时用到。
	CaptchaProviderURL string // e.g. http://127.0.0.1:9876
	// CaptchaFullProxyURL 指向「JSDOM 全链路 chat 代理」provider。
	// 设了就走全代理：整个 chat 请求转给 provider（同 JSDOM 环境拿 captcha+建会话+发 completions），
	// 彻底绕开跨进程环境不一致导致的 F019 verify_failed。设了之后 CaptchaProviderURL 不再参与请求。
	CaptchaFullProxyURL string
	// FullProxyConcurrency 限制到 captcha-provider 的并发请求数（应 = provider 的 WINDOW_POOL_SIZE）。
	// 超出的请求在 Go 侧排队（带缓冲 channel 信号量），避免 provider 窗口池耗尽后大面积超时。
	FullProxyConcurrency int

	// 持久化后端（可选，不设=文件存储 data/）
	DatabaseURL string // MySQL DSN，如 user:pass@tcp(127.0.0.1:3306)/zai2api?parseTime=true
	RedisURL    string // Redis URL，如 redis://127.0.0.1:6379/0

	// Feature Configuration
	DebugLogging            bool
	ToolSupport             bool
	ForceToolChoiceRequired bool
	UseAgentMode            bool // 已废弃：z.ai 内部 agent 模式不返回 tool_calls，无用，保留兼容
	RetryCount              int
	SkipAuthToken           bool
	ScanLimit               int
	LogLevel                string

	// 多轮上下文处理。z.ai 后端不读 messages 数组里的历史（只看 chat_id 的服务端历史），
	// 而我们每次新建 chat_id，所以多轮上下文需要自己补。两种策略：
	//   1) 优先把完整对话历史上传成 .txt 文件（z.ai 文件接口），附到请求 files 数组里让模型读取；
	//   2) 上传失败/超阈值时，回退到「把历史合并到最后一条 user message」（chat_proxy.cjs 里的旧逻辑）。
	// 思路参考 CJackHwang/ds2api 的 current_input_file（DS2API_HISTORY.txt）。
	ContextFileUpload  bool // CONTEXT_FILE_UPLOAD，默认开启
	ContextFileMaxBytes int  // CONTEXT_FILE_MAX_BYTES，历史文件上限，超过则回退合并

	// 匿名 token 池（无 TokenManager / BACKUP_TOKEN 时启用；已配置上游 token 时不使用池）
	AnonymousPoolSize               int
	AnonymousTokenTTLSeconds        int
	AnonymousRefreshIntervalSeconds int
	AnonymousFetchMaxRetries        int

	// Display
	Note []string // 多行备注，在 / 显示
}

var Cfg *Config

func getEnvString(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return val == "true" || val == "1" || val == "yes"
}

func getEnvInt(key string, defaultVal int) int {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	if i, err := strconv.Atoi(val); err == nil {
		return i
	}
	return defaultVal
}

// getEnvStringSlice 解析逗号分隔的字符串为切片
func getEnvStringSlice(key string) []string {
	val := os.Getenv(key)
	if val == "" {
		return nil
	}
	parts := strings.Split(val, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// parseNoteLines 解析多行备注，支持 \n 换行和 | 分隔
func parseNoteLines(note string) []string {
	if note == "" {
		return nil
	}
	// 支持 \n 和 | 作为换行符
	note = strings.ReplaceAll(note, "\\n", "\n")
	note = strings.ReplaceAll(note, "|", "\n")
	lines := strings.Split(note, "\n")
	var result []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

func LoadConfig() {
	godotenv.Load()

	Cfg = &Config{
		// Server
		Port: getEnvString("PORT", "8000"),

		// API Configuration
		APIEndpoint:         getEnvString("API_ENDPOINT", "https://chat.z.ai/api/v2/chat/completions"),
		BrowserBridgeURL:    getEnvString("BROWSER_BRIDGE_URL", ""),
		BrowserBridgeSecret: getEnvString("BROWSER_BRIDGE_SECRET", ""),
		AuthTokens:          getEnvStringSlice("AUTH_TOKEN"),
		BackupTokens:        getEnvStringSlice("BACKUP_TOKEN"),
		CaptchaProviderURL:  getEnvString("CAPTCHA_PROVIDER_URL", ""),
		CaptchaFullProxyURL: getEnvString("CAPTCHA_FULL_PROXY_URL", ""),
		FullProxyConcurrency: getEnvInt("FULL_PROXY_CONCURRENCY", 4),
		DatabaseURL:         getEnvString("DATABASE_URL", ""),
		RedisURL:            getEnvString("REDIS_URL", ""),

		// Feature Configuration
		DebugLogging:            getEnvBool("DEBUG_LOGGING", false),
		ToolSupport:             getEnvBool("TOOL_SUPPORT", true),
		ForceToolChoiceRequired: getEnvBool("FORCE_TOOL_CHOICE_REQUIRED", false),
		UseAgentMode:            getEnvBool("USE_AGENT_MODE", false), // 已废弃，默认关闭
		RetryCount:              getEnvInt("RETRY_COUNT", 5),
		SkipAuthToken:           getEnvBool("SKIP_AUTH_TOKEN", false),
		ScanLimit:               getEnvInt("SCAN_LIMIT", 200000),
		LogLevel:                getEnvString("LOG_LEVEL", "info"),

		ContextFileUpload:   getEnvBool("CONTEXT_FILE_UPLOAD", true),
		ContextFileMaxBytes: getEnvInt("CONTEXT_FILE_MAX_BYTES", 200000), // ~200KB 上限，超了回退合并

		AnonymousPoolSize:               getEnvInt("ANONYMOUS_POOL_SIZE", 4),
		AnonymousTokenTTLSeconds:        getEnvInt("ANONYMOUS_TOKEN_TTL_SECONDS", 1200),
		AnonymousRefreshIntervalSeconds: getEnvInt("ANONYMOUS_REFRESH_INTERVAL_SECONDS", 90),
		AnonymousFetchMaxRetries:        getEnvInt("ANONYMOUS_FETCH_MAX_RETRIES", 3),

		// Display
		Note: parseNoteLines(getEnvString("NOTE", "")),
	}
}

func ValidateAuthToken(token string) bool {
	if Cfg.SkipAuthToken {
		return true
	}
	if len(Cfg.AuthTokens) == 0 && len(GetApiKeyManager().List()) == 0 {
		LogWarn("既未配置 AUTH_TOKEN 也没有用户创建的 API Key，拒绝所有请求")
		return false
	}
	return ValidateAnyApiKey(token)
}

var backupTokenIndex int

func GetBackupToken() string {
	if len(Cfg.BackupTokens) == 0 {
		return ""
	}
	token := Cfg.BackupTokens[backupTokenIndex%len(Cfg.BackupTokens)]
	backupTokenIndex++
	return token
}
