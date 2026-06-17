package storage

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// MysqlBackend 用 MySQL 存储 token 池 / API Key / 用量。
// 启动时自动建表（CREATE TABLE IF NOT EXISTS）。parseTime=true 让 DATETIME 扫描成 time.Time。
type MysqlBackend struct {
	db *sql.DB
}

// NewMysqlBackend 连接 MySQL、建表、测试连接。
//
// DSN 规范化（用户给的 DATABASE_URL 可能缺参数）：
//   - 自动补 parseTime=true（DATETIME 扫描成 time.Time 必需）
//   - 自动补 tls=skip-verify：TiDB Cloud Serverless / PlanetScale / 大多数云 MySQL 强制 TLS，
//     不带 tls 会报 "Connections using insecure transport are prohibited"。
//     skip-verify = 走 TLS 但不校验证书 CN（云 DB 自签证书也能连）。
//     用户若在 DSN 里显式带了 tls=xxx 则尊重用户设置。
func NewMysqlBackend(dsn string) (*MysqlBackend, error) {
	dsn = ensureDSNParam(dsn, "parseTime", "true")
	dsn = ensureDSNParam(dsn, "tls", "skip-verify")

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mysql: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mysql: %w", err)
	}
	b := &MysqlBackend{db: db}
	if err := b.createTables(); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

// ensureDSNParam 确保 DSN 里带某个参数（用户没显式设才补；已有则尊重用户）。
// 形如 user:pass@tcp(host)/db?k1=v1&k2=v2。
func ensureDSNParam(dsn, key, val string) string {
	// 已有该参数（k= 形式）则不动
	if hasDSNParam(dsn, key) {
		return dsn
	}
	sep := "?"
	if containsStr(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + key + "=" + val
}

// hasDSNParam 判断 DSN 的 query 部分是否已有某个 key（精确匹配 k=v，避免 tls 误匹配 tlsKey 之类）。
func hasDSNParam(dsn, key string) bool {
	q := dsn
	if i := indexOfStr(dsn, "?"); i >= 0 {
		q = dsn[i+1:]
	}
	for _, pair := range splitStr(q, "&") {
		k := pair
		if i := indexOfStr(pair, "="); i >= 0 {
			k = pair[:i]
		}
		if k == key {
			return true
		}
	}
	return false
}

func (m *MysqlBackend) createTables() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS zai_tokens (
  token TEXT NOT NULL,
  email VARCHAR(255) DEFAULT '',
  user_id VARCHAR(64) DEFAULT '',
  valid TINYINT(1) DEFAULT 1,
  use_count BIGINT DEFAULT 0,
  last_checked DATETIME NULL,
  created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (token(255))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS api_keys (
  ` + "`key`" + ` VARCHAR(64) NOT NULL,
  name VARCHAR(255) DEFAULT '',
  created_at BIGINT DEFAULT 0,
  last_used BIGINT DEFAULT 0,
  use_count BIGINT DEFAULT 0,
  enabled TINYINT(1) DEFAULT 1,
  PRIMARY KEY (` + "`key`" + `)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  ts DATETIME DEFAULT CURRENT_TIMESTAMP,
  api_key VARCHAR(64) DEFAULT '',
  model VARCHAR(64) DEFAULT '',
  input_tokens BIGINT DEFAULT 0,
  output_tokens BIGINT DEFAULT 0,
  success TINYINT(1) DEFAULT 0,
  is_multimodal TINYINT(1) DEFAULT 0,
  latency_ms INT DEFAULT 0,
  INDEX idx_ts (ts),
  INDEX idx_api_key (api_key),
  INDEX idx_model (model)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`,
	}
	for _, s := range stmts {
		if _, err := m.db.Exec(s); err != nil {
			return fmt.Errorf("create table: %w\nSQL: %s", err, s)
		}
	}
	return nil
}

// ---- Z.AI token ----

func (m *MysqlBackend) ListTokens() ([]TokenRecord, error) {
	rows, err := m.db.Query(`SELECT token, email, user_id, valid, use_count, last_checked, created_at FROM zai_tokens`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenRecord
	for rows.Next() {
		var r TokenRecord
		var lastChecked sql.NullTime
		if err := rows.Scan(&r.Token, &r.Email, &r.UserID, &r.Valid, &r.UseCount, &lastChecked, &r.CreatedAt); err != nil {
			return nil, err
		}
		if lastChecked.Valid {
			r.LastChecked = lastChecked.Time
		}
		// DB 里 email/user_id 可能为空（旧数据），从 JWT 补
		if r.Email == "" || r.UserID == "" {
			if em, uid, ok := decodeJWTIdentity(r.Token); ok {
				r.Email, r.UserID = em, uid
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *MysqlBackend) UpsertToken(t TokenRecord) error {
	if t.Email == "" || t.UserID == "" {
		if em, uid, ok := decodeJWTIdentity(t.Token); ok {
			t.Email, t.UserID = em, uid
		}
	}
	_, err := m.db.Exec(`INSERT INTO zai_tokens (token, email, user_id, valid, use_count, last_checked, created_at)
VALUES (?, ?, ?, ?, ?, ?, IFNULL(?, CURRENT_TIMESTAMP))
ON DUPLICATE KEY UPDATE email=VALUES(email), user_id=VALUES(user_id), valid=VALUES(valid), use_count=VALUES(use_count), last_checked=VALUES(last_checked)`,
		t.Token, t.Email, t.UserID, t.Valid, t.UseCount, nullTime(t.LastChecked), nullTime(t.CreatedAt))
	return err
}

func (m *MysqlBackend) DeleteToken(token string) error {
	_, err := m.db.Exec(`DELETE FROM zai_tokens WHERE token = ?`, token)
	return err
}

// ---- API Key ----

func (m *MysqlBackend) ListApiKeys() ([]ApiKeyRecord, error) {
	rows, err := m.db.Query(`SELECT ` + "`key`" + `, name, created_at, last_used, use_count, enabled FROM api_keys ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ApiKeyRecord
	for rows.Next() {
		var r ApiKeyRecord
		if err := rows.Scan(&r.Key, &r.Name, &r.CreatedAt, &r.LastUsed, &r.UseCount, &r.Enabled); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *MysqlBackend) UpsertApiKey(k ApiKeyRecord) error {
	_, err := m.db.Exec(`INSERT INTO api_keys (`+"`key`"+`, name, created_at, last_used, use_count, enabled)
VALUES (?, ?, ?, ?, ?, ?)
ON DUPLICATE KEY UPDATE name=VALUES(name), last_used=VALUES(last_used), use_count=VALUES(use_count), enabled=VALUES(enabled)`,
		k.Key, k.Name, k.CreatedAt, k.LastUsed, k.UseCount, k.Enabled)
	return err
}

func (m *MysqlBackend) DeleteApiKey(key string) error {
	_, err := m.db.Exec(`DELETE FROM api_keys WHERE `+"`key`"+` = ?`, key)
	return err
}

// ---- 用量 ----

func (m *MysqlBackend) RecordUsage(u UsageRecord) error {
	if u.TS.IsZero() {
		u.TS = time.Now()
	}
	_, err := m.db.Exec(`INSERT INTO usage_logs (ts, api_key, model, input_tokens, output_tokens, success, is_multimodal, latency_ms)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.TS, u.ApiKey, u.Model, u.InputTok, u.OutputTok, u.Success, u.IsMultimodal, u.LatencyMs)
	return err
}

func (m *MysqlBackend) QueryUsageSummary(from, to time.Time) (UsageSummary, error) {
	s := UsageSummary{From: from, To: to, ByModel: map[string]int64{}, ByApiKey: map[string]int64{}}
	// 总量
	var succ int64
	row := m.db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(success),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
FROM usage_logs WHERE ts BETWEEN ? AND ?`, from, to)
	if err := row.Scan(&s.TotalRequests, &succ, &s.InputTok, &s.OutputTok); err != nil {
		return s, err
	}
	s.SuccessCount = succ
	// 按 model
	rows, err := m.db.Query(`SELECT model, COUNT(*) FROM usage_logs WHERE ts BETWEEN ? AND ? GROUP BY model`, from, to)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var model string
		var cnt int64
		if rows.Scan(&model, &cnt) == nil {
			s.ByModel[model] = cnt
		}
	}
	rows.Close()
	// 按 api_key
	rows, err = m.db.Query(`SELECT api_key, COUNT(*) FROM usage_logs WHERE ts BETWEEN ? AND ? AND api_key != '' GROUP BY api_key`, from, to)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var key string
		var cnt int64
		if rows.Scan(&key, &cnt) == nil {
			s.ByApiKey[key] = cnt
		}
	}
	rows.Close()
	return s, nil
}

func (m *MysqlBackend) Close() error { return m.db.Close() }

// ---- 工具 ----

func nullTime(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t
}

func containsStr(s, sub string) bool {
	return indexOfStr(s, sub) >= 0
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func splitStr(s, sep string) []string {
	var out []string
	for {
		i := indexOfStr(s, sep)
		if i < 0 {
			if s != "" {
				out = append(out, s)
			}
			return out
		}
		out = append(out, s[:i])
		s = s[i+len(sep):]
	}
}
