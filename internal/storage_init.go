package internal

import (
	"zai-proxy/internal/storage"
)

// InitStorage 按配置初始化存储后端（文件或 MySQL）。
// DATABASE_URL 设了用 MySQL，否则用文件（data/）。失败不致命，回退 NoopBackend。
func InitStorage() error {
	b, err := storage.InitBackend(Cfg.DatabaseURL)
	if err != nil {
		// 降级：保证服务能起来
		storage.Set(storage.NoopBackend{})
		LogError("InitStorage 失败，使用内存兜底（数据不持久化）: %v", err)
		return err
	}
	if Cfg.DatabaseURL != "" {
		LogInfo("存储后端: MySQL (%s)", maskDSN(Cfg.DatabaseURL))
	} else {
		LogInfo("存储后端: 文件 (data/)")
	}
	_ = b
	return nil
}

// maskDSN 隐藏 DSN 里的密码（日志脱敏）。
func maskDSN(dsn string) string {
	// 形如 user:pass@tcp(host)/db?params
	at := -1
	for i, c := range dsn {
		if c == '@' {
			at = i
			break
		}
	}
	if at == -1 {
		return dsn
	}
	colon := -1
	for i := 0; i < at; i++ {
		if dsn[i] == ':' {
			colon = i
		}
	}
	if colon == -1 {
		return dsn[at:]
	}
	return dsn[:colon+1] + "***" + dsn[at:]
}

// storageBackend 返回当前存储后端（供 token_manager/api_key_manager/telemetry 使用）。
func storageBackend() storage.Backend {
	return storage.Get()
}

// 类型别名，避免各调用方直接 import storage 包时的冗长。
type (
	storageTokenRecord = storage.TokenRecord
	storageApiKeyRecord = storage.ApiKeyRecord
	storageUsageRecord  = storage.UsageRecord
)
