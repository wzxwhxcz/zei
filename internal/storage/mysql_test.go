package storage

import (
	"os"
	"testing"
	"time"
)

// TestMysqlBackend 跑通 MySQL 后端 CRUD + 用量汇总。
// 仅在 ZAI_TEST_MYSQL_DSN 设了时运行（需要真实 MySQL）。
// 用法：ZAI_TEST_MYSQL_DSN="root:pass@tcp(127.0.0.1:3306)/zai2api_test?parseTime=true" go test ./internal/storage/ -run TestMysqlBackend -v
func TestMysqlBackend(t *testing.T) {
	dsn := os.Getenv("ZAI_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("跳过：未设 ZAI_TEST_MYSQL_DSN")
	}

	b, err := NewMysqlBackend(dsn)
	if err != nil {
		t.Fatalf("NewMysqlBackend: %v", err)
	}
	defer b.Close()
	// 清表（测试隔离）
	defer cleanMysql(t, b)

	// ---- token CRUD ----
	tok := TokenRecord{
		Token: "eyJfake.test.token", Email: "u@example.com", UserID: "uid-1",
		Valid: true, UseCount: 0, CreatedAt: time.Now(),
	}
	if err := b.UpsertToken(tok); err != nil {
		t.Fatalf("UpsertToken: %v", err)
	}
	toks, err := b.ListTokens()
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(toks) != 1 || toks[0].Token != tok.Token {
		t.Fatalf("ListTokens got %+v", toks)
	}
	// update use_count
	tok.UseCount = 5
	tok.Valid = false
	if err := b.UpsertToken(tok); err != nil {
		t.Fatalf("UpsertToken(update): %v", err)
	}
	toks, _ = b.ListTokens()
	if toks[0].UseCount != 5 || toks[0].Valid {
		t.Fatalf("update 未生效: %+v", toks[0])
	}
	if err := b.DeleteToken(tok.Token); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	toks, _ = b.ListTokens()
	if len(toks) != 0 {
		t.Fatalf("删除后应空: %d", len(toks))
	}

	// ---- API key CRUD ----
	k := ApiKeyRecord{Key: "sk-test1", Name: "n", CreatedAt: time.Now().Unix(), Enabled: true}
	if err := b.UpsertApiKey(k); err != nil {
		t.Fatalf("UpsertApiKey: %v", err)
	}
	keys, err := b.ListApiKeys()
	if err != nil || len(keys) != 1 || keys[0].Key != "sk-test1" {
		t.Fatalf("ListApiKeys: %v %+v", err, keys)
	}
	if err := b.DeleteApiKey("sk-test1"); err != nil {
		t.Fatalf("DeleteApiKey: %v", err)
	}

	// ---- 用量 ----
	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := b.RecordUsage(UsageRecord{
			TS: now, ApiKey: "sk-test", Model: "GLM-5.2",
			InputTok: 10, OutputTok: 5, Success: true,
		}); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}
	if err := b.RecordUsage(UsageRecord{TS: now, ApiKey: "sk-test", Model: "GLM-4.7", InputTok: 3, OutputTok: 1, Success: false}); err != nil {
		t.Fatalf("RecordUsage(fail): %v", err)
	}
	summary, err := b.QueryUsageSummary(now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("QueryUsageSummary: %v", err)
	}
	if summary.TotalRequests != 4 {
		t.Fatalf("total=%d want 4", summary.TotalRequests)
	}
	if summary.SuccessCount != 3 {
		t.Fatalf("success=%d want 3", summary.SuccessCount)
	}
	if summary.InputTok != 33 || summary.OutputTok != 16 {
		t.Fatalf("tokens in=%d out=%d want 33/16", summary.InputTok, summary.OutputTok)
	}
	if summary.ByModel["GLM-5.2"] != 3 || summary.ByModel["GLM-4.7"] != 1 {
		t.Fatalf("by_model=%+v", summary.ByModel)
	}
	t.Logf("usage summary OK: %+v", summary)
}

func cleanMysql(t *testing.T, b Backend) {
	m, ok := b.(*MysqlBackend)
	if !ok {
		return
	}
	for _, tbl := range []string{"usage_logs", "api_keys", "zai_tokens"} {
		if _, err := m.db.Exec("DELETE FROM " + tbl); err != nil {
			t.Logf("clean %s: %v", tbl, err)
		}
	}
}
