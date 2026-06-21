package internal

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	fhttp "github.com/bogdanfinn/fhttp"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// TokenInfo 存储单个 token 的信息
type TokenInfo struct {
	Token       string    `json:"token"`
	Email       string    `json:"email"`
	UserID      string    `json:"user_id"`
	Valid       bool      `json:"valid"`
	LastChecked time.Time `json:"last_checked"`
	UseCount    int64     `json:"use_count"`

	// ── 熔断标记（405/403 等上游 CDN 拦截）──
	// 阿里云 ESA CDN 会针对某些 token（URL query 里的 JWT）在边缘直接 405。
	// 被标记的 token 暂时跳过（GetToken 不返回），TTL 到期自动解封重试。
	Blocked     bool      `json:"blocked"`
	BlockReason string    `json:"block_reason,omitempty"`
	BlockedAt   time.Time `json:"blocked_at,omitempty"`
	BlockCount  int       `json:"block_count"` // 累计被标记次数（衡量 token 质量）
}

// tokenBlock 记录单个 token 的熔断状态（内存版，不依赖 Redis）。
type tokenBlock struct {
	reason  string
	at      time.Time
	expires time.Time // TTL 到期自动解封
}

// TokenManager 管理所有用户 token
type TokenManager struct {
	mu              sync.RWMutex
	tokens          map[string]*TokenInfo // token -> TokenInfo
	validTokens     []string              // 有效 token 列表
	currentIndex    int                   // 轮询索引
	dataDir         string
	watcher         *fsnotify.Watcher
	checkInterval   time.Duration
	stopChan        chan struct{}
	multimodalCount int64 // 多模态请求计数
	totalCalls      int64 // 累计调用次数
	successCalls    int64 // 成功调用次数

	// 内存熔断表：token -> tokenBlock。HF 等无 Redis 环境用这个。
	// 有 Redis 时也写一份（双写），保证非 Redis 部署也能用。
	blocks    map[string]*tokenBlock
	blockTTL  time.Duration // 熔断时长（默认 10 分钟）
}

var (
	tokenManager *TokenManager
	tokenOnce    sync.Once
)

// GetTokenManager 获取单例 TokenManager
func GetTokenManager() *TokenManager {
	tokenOnce.Do(func() {
		tokenManager = &TokenManager{
			tokens:      make(map[string]*TokenInfo),
			validTokens: make([]string, 0),
			dataDir:     "data",
			checkInterval: 5 * time.Minute, // 每5分钟检查一次
			stopChan:    make(chan struct{}),
			blocks:      make(map[string]*tokenBlock),
			blockTTL:    10 * time.Minute, // 默认熔断 10 分钟，到期自动解封重试
		}
	})
	return tokenManager
}

// Start 启动 token 管理器
func (tm *TokenManager) Start() error {
	// 确保 data 目录存在
	if err := os.MkdirAll(tm.dataDir, 0755); err != nil {
		return fmt.Errorf("创建 data 目录失败: %v", err)
	}

	// 初始加载 token
	if err := tm.loadTokens(); err != nil {
		LogWarn("初始加载 token 失败: %v", err)
	}

	// 启动文件监听
	if err := tm.startWatcher(); err != nil {
		LogWarn("启动文件监听失败: %v", err)
	}

	// 启动定期验证
	go tm.startValidator()

	LogInfo("TokenManager 已启动，当前有效 token 数: %d", len(tm.validTokens))
	return nil
}

// Stop 停止 token 管理器
func (tm *TokenManager) Stop() {
	close(tm.stopChan)
	if tm.watcher != nil {
		tm.watcher.Close()
	}
}

// loadTokens 从 data 目录加载所有 token
func (tm *TokenManager) loadTokens() error {
	// 优先从存储后端加载（MySQL 或 FileBackend）。后端为空/出错时回退到直接读文件。
	if b := storageBackend(); b != nil {
		records, err := b.ListTokens()
		if err == nil && len(records) > 0 {
			tm.loadFromRecords(records)
			return nil
		}
	}
	// 回退：直接读 data/tokens.txt（FileBackend 未命中或后端不可用）
	tokenFile := filepath.Join(tm.dataDir, "tokens.txt")

	file, err := os.Open(tokenFile)
	if err != nil {
		if os.IsNotExist(err) {
			// 创建示例文件
			tm.createExampleTokenFile(tokenFile)
			return nil
		}
		return err
	}
	defer file.Close()

	tm.mu.Lock()
	defer tm.mu.Unlock()

	// 保留旧的统计数据
	oldTokens := tm.tokens
	tm.tokens = make(map[string]*TokenInfo)
	tm.validTokens = make([]string, 0)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		token := line
		// 支持 token=xxx 格式
		if strings.HasPrefix(line, "token=") {
			token = strings.TrimPrefix(line, "token=")
		}

		if token == "" {
			continue
		}

		// 复用旧的 TokenInfo 如果存在
		if oldInfo, exists := oldTokens[token]; exists {
			tm.tokens[token] = oldInfo
			if oldInfo.Valid {
				tm.validTokens = append(tm.validTokens, token)
			}
		} else {
			// 新 token，解析并标记为待验证
			info := &TokenInfo{
				Token: token,
				Valid: true, // 初始假设有效，验证时会更新
			}
			// 尝试解析 JWT 获取信息
			if payload, err := DecodeJWTPayload(token); err == nil && payload != nil {
				info.Email = payload.Email
				info.UserID = payload.ID
			}
			tm.tokens[token] = info
			tm.validTokens = append(tm.validTokens, token)
		}
	}

	validN := len(tm.validTokens)
	LogInfo("已加载 %d 个 token", validN)
	scanErr := scanner.Err()
	if validN > 0 {
		invalidateAnonymousPoolSlots()
	}
	return scanErr
}

// loadFromRecords 从存储后端的 TokenRecord 列表加载到内存（替代直接读文件）。
func (tm *TokenManager) loadFromRecords(records []storageTokenRecord) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	oldTokens := tm.tokens
	tm.tokens = make(map[string]*TokenInfo, len(records))
	tm.validTokens = make([]string, 0, len(records))
	for _, r := range records {
		if oldInfo, exists := oldTokens[r.Token]; exists {
			tm.tokens[r.Token] = oldInfo
		} else {
			info := &TokenInfo{Token: r.Token, Valid: r.Valid, Email: r.Email, UserID: r.UserID}
			if info.Email == "" || info.UserID == "" {
				if payload, err := DecodeJWTPayload(r.Token); err == nil && payload != nil {
					info.Email, info.UserID = payload.Email, payload.ID
				}
			}
			tm.tokens[r.Token] = info
		}
		if tm.tokens[r.Token].Valid {
			tm.validTokens = append(tm.validTokens, r.Token)
		}
	}
	LogInfo("已加载 %d 个 token（来自存储后端）", len(tm.validTokens))
	if len(tm.validTokens) > 0 {
		invalidateAnonymousPoolSlots()
	}
}

// createExampleTokenFile 创建示例 token 文件
func (tm *TokenManager) createExampleTokenFile(path string) {
	content := `# 用户 Token 文件
# 每行一个 token，支持以下格式：
# 1. 直接写 token
# 2. token=xxx 格式
# 以 # 开头的行为注释

# 示例:
# eyJhbGciOiJFUzI1NiIsInR5cCI6IkpXVCJ9.xxxxx
`
	os.WriteFile(path, []byte(content), 0644)
	LogInfo("已创建示例 token 文件: %s", path)
}

// startWatcher 启动文件变化监听
func (tm *TokenManager) startWatcher() error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	tm.watcher = watcher

	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
					if strings.HasSuffix(event.Name, "tokens.txt") {
						LogInfo("检测到 token 文件变化，重新加载...")
						time.Sleep(100 * time.Millisecond) // 等待文件写入完成
						tm.loadTokens()
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				LogError("文件监听错误: %v", err)
			case <-tm.stopChan:
				return
			}
		}
	}()

	return watcher.Add(tm.dataDir)
}

// startValidator 启动定期验证
func (tm *TokenManager) startValidator() {
	// 首次延迟验证
	time.Sleep(10 * time.Second)
	tm.validateAllTokens()

	ticker := time.NewTicker(tm.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			tm.validateAllTokens()
		case <-tm.stopChan:
			return
		}
	}
}

// validateAllTokens 验证所有 token
func (tm *TokenManager) validateAllTokens() {
	tm.mu.RLock()
	tokens := make([]string, 0, len(tm.tokens))
	for token := range tm.tokens {
		tokens = append(tokens, token)
	}
	tm.mu.RUnlock()

	LogInfo("开始验证 %d 个 token...", len(tokens))
	invalidCount := 0

	for _, token := range tokens {
		valid := tm.validateToken(token)
		tm.mu.Lock()
		if info, exists := tm.tokens[token]; exists {
			info.Valid = valid
			info.LastChecked = time.Now()
			if !valid {
				invalidCount++
			}
		}
		tm.mu.Unlock()
		time.Sleep(500 * time.Millisecond) // 避免请求过快
	}

	// 更新有效 token 列表
	tm.rebuildValidTokens()
	LogInfo("Token 验证完成，失效 %d 个，剩余有效 %d 个", invalidCount, len(tm.validTokens))

	// 自动删除失效 token
	if invalidCount > 0 {
		tm.removeInvalidTokens()
	}
}

// validateToken 验证单个 token
func (tm *TokenManager) validateToken(token string) bool {
	req, err := fhttp.NewRequest("GET", "https://chat.z.ai/api/v1/auths/", nil)
	if err != nil {
		return false
	}

	ApplyBrowserFingerprintHeaders(req.Header)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("DNT", "1")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", "https://chat.z.ai/")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("sec-gpc", "1")
	req.AddCookie(&fhttp.Cookie{Name: "token", Value: token})

	client, err := TLSHTTPClient(10 * time.Second)
	if err != nil {
		LogDebug("Token 验证 tls client: %v", err)
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		LogDebug("Token 验证请求失败: %v", err)
		return false
	}
	defer resp.Body.Close()

	// 读取响应
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		LogDebug("Token 验证失败，状态码: %d", resp.StatusCode)
		return false
	}

	// 尝试解析响应获取新 token
	var authResp struct {
		Token string `json:"token"`
		Email string `json:"email"`
		ID    string `json:"id"`
	}
	if err := json.Unmarshal(body, &authResp); err == nil && authResp.Token != "" {
		// 更新 token 信息
		tm.mu.Lock()
		if info, exists := tm.tokens[token]; exists {
			if authResp.Email != "" {
				info.Email = authResp.Email
			}
			if authResp.ID != "" {
				info.UserID = authResp.ID
			}
		}
		tm.mu.Unlock()
	}

	return true
}

// rebuildValidTokens 重建有效 token 列表
func (tm *TokenManager) rebuildValidTokens() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tm.validTokens = make([]string, 0)
	for token, info := range tm.tokens {
		if info.Valid {
			tm.validTokens = append(tm.validTokens, token)
		}
	}
}

// removeInvalidTokens 从文件中移除失效 token
func (tm *TokenManager) removeInvalidTokens() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	tokenFile := filepath.Join(tm.dataDir, "tokens.txt")
	invalidFile := filepath.Join(tm.dataDir, "tokens_invalid.txt")

	// 收集失效 token
	var invalidTokens []string
	for token, info := range tm.tokens {
		if !info.Valid {
			invalidTokens = append(invalidTokens, token)
			delete(tm.tokens, token)
		}
	}

	if len(invalidTokens) == 0 {
		return
	}

	// 追加到失效文件
	f, err := os.OpenFile(invalidFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		defer f.Close()
		timestamp := time.Now().Format("2006-01-02 15:04:05")
		for _, token := range invalidTokens {
			f.WriteString(fmt.Sprintf("# 失效于 %s\n%s\n", timestamp, token))
		}
	}

	// 重写有效 token 文件
	var validTokenLines []string
	for _, token := range tm.validTokens {
		validTokenLines = append(validTokenLines, token)
	}

	content := "# 用户 Token 文件（自动更新）\n"
	content += fmt.Sprintf("# 更新时间: %s\n", time.Now().Format("2006-01-02 15:04:05"))
	content += "# 失效 token 已移至 tokens_invalid.txt\n\n"
	content += strings.Join(validTokenLines, "\n")
	if len(validTokenLines) > 0 {
		content += "\n"
	}

	os.WriteFile(tokenFile, []byte(content), 0644)
	LogInfo("已移除 %d 个失效 token 到 %s", len(invalidTokens), invalidFile)
}

// HasValidUpstreamTokens 是否存在可用的 z.ai 上游 token（TokenManager 轮询用）
func (tm *TokenManager) HasValidUpstreamTokens() bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return len(tm.validTokens) > 0
}

// GetToken 获取一个有效 token（轮询）。
// Redis 启用时用原子轮询指针（多实例不抢），并跳过熔断中的 token。
func (tm *TokenManager) GetToken() string {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if len(tm.validTokens) == 0 {
		return ""
	}

	n := len(tm.validTokens)
	// 轮询起点：Redis 原子指针优先，否则本地指针
	start := tm.currentIndex % n
	if rcIdx := RedisNextTokenIndex(n); rcIdx >= 0 {
		start = rcIdx
	}
	tm.currentIndex++

	// 跳过熔断中的 token（最多试 n 次避免死循环）
	// 同时支持 Redis（多实例）和内存表（无 Redis 部署）。
	for attempt := 0; attempt < n; attempt++ {
		token := tm.validTokens[(start+attempt)%n]
		if tm.isTokenBlockedLocked(token) {
			continue
		}
		if info, exists := tm.tokens[token]; exists {
			info.UseCount++
		}
		return token
	}

	// 全部熔断：降级返回第一个（聊胜于无）
	token := tm.validTokens[start]
	if info, exists := tm.tokens[token]; exists {
		info.UseCount++
	}
	return token
}

// RecordCall 记录调用
func (tm *TokenManager) RecordCall(success bool, isMultimodal bool) {
	atomic.AddInt64(&tm.totalCalls, 1)
	if success {
		atomic.AddInt64(&tm.successCalls, 1)
	}
	if isMultimodal {
		atomic.AddInt64(&tm.multimodalCount, 1)
	}
}

// MarkTokenBlocked 标记某 token 熔断（405/403 等上游 CDN 拦截）。
// 熔断期内 GetToken 跳过该 token，TTL 到期自动解封（给 token 重试机会，
// 因为 CDN 拦截可能是间歇的）。同时写 Redis（若启用）和内存表（双写）。
func (tm *TokenManager) MarkTokenBlocked(token, reason string) {
	if token == "" {
		return
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	now := time.Now()
	// 内存表（非 Redis 部署的主路径）
	tm.blocks[token] = &tokenBlock{
		reason:  reason,
		at:      now,
		expires: now.Add(tm.blockTTL),
	}
	// 同步 Redis（若启用，多实例共享）
	RedisMarkTokenFail(token)
	// 同步 TokenInfo（后台展示用）
	if info, ok := tm.tokens[token]; ok {
		info.Blocked = true
		info.BlockReason = reason
		info.BlockedAt = now
		info.BlockCount++
	}
	LogInfo("[TokenBlock] 标记熔断 token=%s reason=%s ttl=%v (累计%d次)",
		maskToken(token), reason, tm.blockTTL, func() int {
			if info, ok := tm.tokens[token]; ok {
				return info.BlockCount
			}
			return 0
		}())
}

// isTokenBlockedLocked 查询 token 是否在熔断期内（调用方持锁）。
func (tm *TokenManager) isTokenBlockedLocked(token string) bool {
	if token == "" {
		return false
	}
	// Redis 优先（多实例一致）
	if RedisIsTokenFailed(token) {
		return true
	}
	b, ok := tm.blocks[token]
	if !ok {
		return false
	}
	// TTL 过期：删除并解封
	if time.Now().After(b.expires) {
		delete(tm.blocks, token)
		if info, ok2 := tm.tokens[token]; ok2 {
			info.Blocked = false
			info.BlockReason = ""
		}
		return false
	}
	return true
}

// UnblockToken 手动解封某 token（后台用）。
func (tm *TokenManager) UnblockToken(token string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	_, existed := tm.blocks[token]
	delete(tm.blocks, token)
	if info, ok := tm.tokens[token]; ok {
		info.Blocked = false
		info.BlockReason = ""
	}
	return existed
}

// CleanupExpiredBlocks 清理过期的熔断记录（定时调用，避免 map 无限增长）。
func (tm *TokenManager) CleanupExpiredBlocks() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	now := time.Now()
	for tok, b := range tm.blocks {
		if now.After(b.expires) {
			delete(tm.blocks, tok)
			if info, ok := tm.tokens[tok]; ok {
				info.Blocked = false
				info.BlockReason = ""
			}
		}
	}
}

// TokenHealthStats 返回 token 健康度统计（供后台 overview 展示）。
//   - total:       管理的 token 总数
//   - healthy:     有效且未被熔断（可正常用）
//   - blocked:     当前被熔断（405/403 CDN 拦截，TTL 内自动解封）
//   - invalid:     无效（校验失败/过期）
//   - total_block_count: 累计触发熔断次数（衡量 token 池整体质量）
type TokenHealthStats struct {
	Total            int `json:"total"`
	Healthy          int `json:"healthy"`
	Blocked          int `json:"blocked"`
	Invalid          int `json:"invalid"`
	TotalBlockCount  int `json:"total_block_count"`
}

// HealthStats 计算 token 健康度统计。调用时清理一次过期熔断。
func (tm *TokenManager) HealthStats() TokenHealthStats {
	tm.CleanupExpiredBlocks()
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	st := TokenHealthStats{Total: len(tm.tokens)}
	for _, info := range tm.tokens {
		if !info.Valid {
			st.Invalid++
			continue
		}
		if tm.isTokenBlockedRLocked(info.Token) {
			st.Blocked++
		} else {
			st.Healthy++
		}
		st.TotalBlockCount += info.BlockCount
	}
	return st
}

// isTokenBlockedRLocked 持读锁版本（HealthStats 用，避免升级锁）。
func (tm *TokenManager) isTokenBlockedRLocked(token string) bool {
	if token == "" {
		return false
	}
	if RedisIsTokenFailed(token) {
		return true
	}
	b, ok := tm.blocks[token]
	if !ok {
		return false
	}
	return time.Now().Before(b.expires)
}

// GetStats 获取统计数据
func (tm *TokenManager) GetStats() TokenManagerStats {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	total := atomic.LoadInt64(&tm.totalCalls)
	success := atomic.LoadInt64(&tm.successCalls)
	multimodal := atomic.LoadInt64(&tm.multimodalCount)

	var successRate float64
	if total > 0 {
		successRate = float64(success) / float64(total) * 100
	}

	return TokenManagerStats{
		ValidTokenCount: len(tm.validTokens),
		TotalTokenCount: len(tm.tokens),
		MultimodalCount: multimodal,
		TotalCalls:      total,
		SuccessCalls:    success,
		SuccessRate:     successRate,
	}
}

// TokenManagerStats token 管理器统计数据
type TokenManagerStats struct {
	ValidTokenCount int     `json:"valid_token_count"`
	TotalTokenCount int     `json:"total_token_count"`
	MultimodalCount int64   `json:"multimodal_count"`
	TotalCalls      int64   `json:"total_calls"`
	SuccessCalls    int64   `json:"success_calls"`
	SuccessRate     float64 `json:"success_rate"`
}

// GetClientIP 从请求中获取客户端 IP
func GetClientIP(r *http.Request) string {
	// 优先从 X-Forwarded-For 获取
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		// X-Forwarded-For 可能包含多个 IP，取第一个
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			ip := strings.TrimSpace(ips[0])
			if ip != "" {
				return ip
			}
		}
	}

	// 尝试 X-Real-IP
	xri := r.Header.Get("X-Real-IP")
	if xri != "" {
		return xri
	}

	// 最后使用 RemoteAddr
	ip := r.RemoteAddr
	// 去除端口
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	return ip
}
