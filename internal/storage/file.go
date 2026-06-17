package storage

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// FileBackend 把现有 data/ 文件持久化包成 Backend 接口。
// - Z.AI token: data/tokens.txt（纯文本，每行一个 JWT；# 注释）
// - API Key:    data/api_keys.json（JSON 数组）
// - 用量:        data/usage.jsonl（append-only，每行一条；可选，缺失不报错）
//
// 注意：tokens.txt 只存 token 字符串（与现有实现一致），
// Email/UserID 由 JWT 解码补全，UseCount/Valid 等 stat 不落盘（重启丢失，和现状一致）。
type FileBackend struct {
	dataDir string
}

func NewFileBackend(dataDir string) (*FileBackend, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir data dir: %w", err)
	}
	return &FileBackend{dataDir: dataDir}, nil
}

func (f *FileBackend) tokenPath() string  { return filepath.Join(f.dataDir, "tokens.txt") }
func (f *FileBackend) apiKeyPath() string { return filepath.Join(f.dataDir, "api_keys.json") }
func (f *FileBackend) usagePath() string  { return filepath.Join(f.dataDir, "usage.jsonl") }

// ---- Z.AI token ----

func (f *FileBackend) ListTokens() ([]TokenRecord, error) {
	data, err := os.ReadFile(f.tokenPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // 文件不存在=空池，不算错
		}
		return nil, err
	}
	var out []TokenRecord
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "token=")
		rec := TokenRecord{Token: line, Valid: true, CreatedAt: time.Now()}
		if email, uid, ok := decodeJWTIdentity(line); ok {
			rec.Email, rec.UserID = email, uid
		}
		out = append(out, rec)
	}
	return out, nil
}

// UpsertToken 对文件后端=全量重写（按 token 去重保留最新）。
// 调用方（TokenManager）已持有全量内存快照，这里直接覆盖写文件。
func (f *FileBackend) UpsertToken(t TokenRecord) error {
	// 单条 upsert 在文件语义下等价于：读全部 → 合并 → 写全部。
	existing, err := f.ListTokens()
	if err != nil {
		existing = nil
	}
	merged := make([]TokenRecord, 0, len(existing)+1)
	found := false
	for _, e := range existing {
		if e.Token == t.Token {
			merged = append(merged, t)
			found = true
		} else {
			merged = append(merged, e)
		}
	}
	if !found {
		merged = append(merged, t)
	}
	return f.writeTokens(merged)
}

// RewriteTokens 全量替换文件（供 TokenManager 批量保存用）。
func (f *FileBackend) RewriteTokens(tokens []TokenRecord) error {
	return f.writeTokens(tokens)
}

// RewriteApiKeys 全量替换 api_keys.json（供 ApiKeyManager 批量保存用）。
func (f *FileBackend) RewriteApiKeys(keys []ApiKeyRecord) error {
	return f.writeApiKeys(keys)
}

func (f *FileBackend) writeTokens(tokens []TokenRecord) error {
	var sb strings.Builder
	sb.WriteString("# 用户 Token 文件 (由管理后台维护，可手动编辑)\n# 每行一个 JWT token\n\n")
	for _, t := range tokens {
		sb.WriteString(t.Token)
		sb.WriteByte('\n')
	}
	return atomicWrite(f.tokenPath(), sb.String())
}

func (f *FileBackend) DeleteToken(token string) error {
	existing, err := f.ListTokens()
	if err != nil {
		return err
	}
	kept := make([]TokenRecord, 0, len(existing))
	for _, e := range existing {
		if e.Token != token {
			kept = append(kept, e)
		}
	}
	return f.writeTokens(kept)
}

// ---- API Key ----

func (f *FileBackend) ListApiKeys() ([]ApiKeyRecord, error) {
	data, err := os.ReadFile(f.apiKeyPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []ApiKeyRecord
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode api_keys.json: %w", err)
	}
	return out, nil
}

func (f *FileBackend) UpsertApiKey(k ApiKeyRecord) error {
	existing, _ := f.ListApiKeys()
	merged := make([]ApiKeyRecord, 0, len(existing)+1)
	found := false
	for _, e := range existing {
		if e.Key == k.Key {
			merged = append(merged, k)
			found = true
		} else {
			merged = append(merged, e)
		}
	}
	if !found {
		merged = append(merged, k)
	}
	return f.writeApiKeys(merged)
}

func (f *FileBackend) writeApiKeys(keys []ApiKeyRecord) error {
	// 按 CreatedAt 升序，与现有 ApiKeyManager.save 一致
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[i].CreatedAt > keys[j].CreatedAt {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	b, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(f.apiKeyPath(), string(b))
}

func (f *FileBackend) DeleteApiKey(key string) error {
	existing, _ := f.ListApiKeys()
	kept := make([]ApiKeyRecord, 0, len(existing))
	for _, e := range existing {
		if e.Key != key {
			kept = append(kept, e)
		}
	}
	return f.writeApiKeys(kept)
}

// ---- 用量（usage.jsonl，append-only）----

func (f *FileBackend) RecordUsage(u UsageRecord) error {
	if u.TS.IsZero() {
		u.TS = time.Now()
	}
	b, err := json.Marshal(u)
	if err != nil {
		return err
	}
	// O_APPEND 追加，每行一条
	file, err := os.OpenFile(f.usagePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(b, '\n'))
	return err
}

func (f *FileBackend) QueryUsageSummary(from, to time.Time) (UsageSummary, error) {
	summary := UsageSummary{
		From: from, To: to,
		ByModel:  map[string]int64{},
		ByApiKey: map[string]int64{},
	}
	file, err := os.Open(f.usagePath())
	if err != nil {
		if os.IsNotExist(err) {
			return summary, nil
		}
		return summary, err
	}
	defer file.Close()
	sc := bufio.NewScanner(file)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		var u UsageRecord
		if json.Unmarshal([]byte(sc.Text()), &u) != nil {
			continue
		}
		if !u.TS.Before(from) && !u.TS.After(to) {
			summary.TotalRequests++
			if u.Success {
				summary.SuccessCount++
			}
			summary.InputTok += u.InputTok
			summary.OutputTok += u.OutputTok
			summary.ByModel[u.Model]++
			if u.ApiKey != "" {
				summary.ByApiKey[u.ApiKey]++
			}
		}
	}
	return summary, sc.Err()
}

func (f *FileBackend) Close() error { return nil }

// ---- 工具 ----

// atomicWrite 原子写：先写 .tmp 再 rename（与现有实现一致）。
func atomicWrite(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// decodeJWTIdentity 从 JWT payload 解出 email/user_id（与 jwt.go 的 DecodeJWTPayload 等价，但本包独立避免循环依赖）。
func decodeJWTIdentity(token string) (email, userID string, ok bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", "", false
	}
	payload := parts[1]
	decoded, err := base64URLDecode(payload)
	if err != nil {
		return "", "", false
	}
	var ident struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	}
	if json.Unmarshal(decoded, &ident) != nil {
		return "", "", false
	}
	return ident.Email, ident.ID, ident.ID != ""
}

// base64URLDecode 兼容标准/原始 URL-safe base64（自动补 padding）。
func base64URLDecode(s string) ([]byte, error) {
	if pad := 4 - len(s)%4; pad != 4 {
		s += strings.Repeat("=", pad)
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawURLEncoding.DecodeString(s)
}
