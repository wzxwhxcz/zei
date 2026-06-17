// Package storage 提供持久化后端抽象。
//
// Backend 接口统一 token 池 / API Key / 用量记录的持久化；
// 按 env 选择实现：FileBackend（默认，data/ 文件）或 MysqlBackend（DATABASE_URL）。
// 内存（TokenManager/ApiKeyManager）始终是热数据源，后端是持久化镜像。
package storage

import "time"

// TokenRecord Z.AI JWT token 的完整记录（含元数据，文件后端会补全）。
type TokenRecord struct {
	Token       string    `json:"token"`
	Email       string    `json:"email"`
	UserID      string    `json:"user_id"`
	Valid       bool      `json:"valid"`
	UseCount    int64     `json:"use_count"`
	LastChecked time.Time `json:"last_checked"`
	CreatedAt   time.Time `json:"created_at"`
}

// ApiKeyRecord 客户端 API Key 记录。
type ApiKeyRecord struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	LastUsed  int64  `json:"last_used"`
	UseCount  int64  `json:"use_count"`
	Enabled   bool   `json:"enabled"`
}

// UsageRecord 单次请求的用量（append-only 日志）。
type UsageRecord struct {
	TS           time.Time
	ApiKey       string
	Model        string
	InputTok     int64
	OutputTok    int64
	Success      bool
	IsMultimodal bool
	LatencyMs    int
}

// UsageSummary 时间段内的汇总。
type UsageSummary struct {
	From          time.Time         `json:"from"`
	To            time.Time         `json:"to"`
	TotalRequests int64             `json:"total_requests"`
	SuccessCount  int64             `json:"success_count"`
	InputTok      int64             `json:"input_tokens"`
	OutputTok     int64             `json:"output_tokens"`
	ByModel       map[string]int64  `json:"by_model"`     // model -> requests
	ByApiKey      map[string]int64  `json:"by_api_key"`   // api_key -> requests
}

// Backend 持久化后端接口。
// 所有方法应容忍「记录不存在」语义（Delete 不存在返回 nil）。
type Backend interface {
	// Z.AI token 池
	ListTokens() ([]TokenRecord, error)
	UpsertToken(t TokenRecord) error // 新增或更新
	DeleteToken(token string) error

	// API Key
	ListApiKeys() ([]ApiKeyRecord, error)
	UpsertApiKey(k ApiKeyRecord) error
	DeleteApiKey(key string) error

	// 用量记录（append-only 日志 + 汇总查询）
	RecordUsage(u UsageRecord) error
	QueryUsageSummary(from, to time.Time) (UsageSummary, error)

	Close() error
}

// NoopBackend 不做任何持久化（内存模式的兜底，理论上不会被选中）。
type NoopBackend struct{}

func (NoopBackend) ListTokens() ([]TokenRecord, error)                  { return nil, nil }
func (NoopBackend) UpsertToken(TokenRecord) error                       { return nil }
func (NoopBackend) DeleteToken(string) error                            { return nil }
func (NoopBackend) ListApiKeys() ([]ApiKeyRecord, error)                { return nil, nil }
func (NoopBackend) UpsertApiKey(ApiKeyRecord) error                     { return nil }
func (NoopBackend) DeleteApiKey(string) error                           { return nil }
func (NoopBackend) RecordUsage(UsageRecord) error                       { return nil }
func (NoopBackend) QueryUsageSummary(time.Time, time.Time) (UsageSummary, error) {
	return UsageSummary{}, nil
}
func (NoopBackend) Close() error { return nil }
