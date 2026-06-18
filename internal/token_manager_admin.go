package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AddToken 添加一个 token 到 TokenManager 并写入 data/tokens.txt
// 写入文件后 fsnotify 会触发 reload，但我们这里同步更新内存以避免短暂窗口
// extractTokenFromInput 从用户输入中提取 JWT token，兼容多种格式：
//   - 裸 JWT：eyJhbGci...
//   - token=eyJ... 前缀
//   - email----password----token（---- 分隔，token 在最后）
//   - 任意以 ---- 分隔的多段，取最后一段（只要它是合法 JWT）
func extractTokenFromInput(input string) string {
	input = strings.TrimSpace(input)
	if input == "" {
		return ""
	}
	// token= 前缀
	if strings.HasPrefix(input, "token=") {
		input = strings.TrimPrefix(input, "token=")
	}
	// ---- 分隔的多段（如 email----password----token），取最后一段
	if strings.Contains(input, "----") {
		parts := strings.Split(input, "----")
		last := strings.TrimSpace(parts[len(parts)-1])
		// 校验最后一段是 JWT（以 eyJ 开头）
		if strings.HasPrefix(last, "eyJ") {
			return last
		}
	}
	// 裸 JWT
	return strings.TrimSpace(input)
}

func (tm *TokenManager) AddToken(tokenInput string) (*TokenInfo, error) {
	token := extractTokenFromInput(tokenInput)
	if token == "" {
		return nil, fmt.Errorf("token 为空")
	}

	// JWT 校验
	payload, err := DecodeJWTPayload(token)
	if err != nil || payload == nil {
		return nil, fmt.Errorf("无效的 JWT token，请检查格式（应以 eyJ 开头）")
	}

	tm.mu.Lock()
	if _, exists := tm.tokens[token]; exists {
		tm.mu.Unlock()
		return nil, fmt.Errorf("token 已存在")
	}

	info := &TokenInfo{
		Token:  token,
		Email:  payload.Email,
		UserID: payload.ID,
		Valid:  true,
	}
	tm.tokens[token] = info
	tm.validTokens = append(tm.validTokens, token)
	tm.mu.Unlock()

	// 异步验证有效性（后台）
	go func() {
		valid := tm.validateToken(token)
		tm.mu.Lock()
		if t, ok := tm.tokens[token]; ok {
			t.Valid = valid
		}
		if !valid {
			tm.rebuildValidTokensLocked()
		}
		tm.mu.Unlock()
	}()

	// 写入文件（不持锁，因为 writeTokensToFile 自己会读快照）
	if err := tm.writeTokensToFile(); err != nil {
		LogWarn("写入 tokens.txt 失败: %v", err)
	}

	invalidateAnonymousPoolSlots()
	return info, nil
}

// AddTokens 批量添加 token（一次内存操作 + 一次写文件，比逐个 AddToken 快得多）。
// 返回 {added, skipped(重复), failed(格式错误)}。
func (tm *TokenManager) AddTokens(inputs []string) (added, skipped, failed int, addedInfos []*TokenInfo) {
	// 先解析全部 + 去重
	type parsed struct{ token string; payload *JWTPayload }
	var valid []parsed
	seen := make(map[string]bool)
	for _, input := range inputs {
		token := extractTokenFromInput(input)
		if token == "" {
			failed++
			continue
		}
		if seen[token] {
			skipped++
			continue
		}
		payload, err := DecodeJWTPayload(token)
		if err != nil || payload == nil {
			failed++
			continue
		}
		seen[token] = true
		valid = append(valid, parsed{token, payload})
	}

	// 一次加锁，批量入内存
	tm.mu.Lock()
	for _, p := range valid {
		if _, exists := tm.tokens[p.token]; exists {
			skipped++
			continue
		}
		info := &TokenInfo{Token: p.token, Email: p.payload.Email, UserID: p.payload.ID, Valid: true}
		tm.tokens[p.token] = info
		tm.validTokens = append(tm.validTokens, p.token)
		addedInfos = append(addedInfos, info)
		added++
	}
	tm.mu.Unlock()

	// 一次写文件（不是每个 token 写一次）
	if added > 0 {
		if err := tm.writeTokensToFile(); err != nil {
			LogWarn("批量写入 tokens.txt 失败: %v", err)
		}
		// 异步验证全部新 token（后台，不阻塞返回）
		for _, info := range addedInfos {
			go func(t string) {
				valid := tm.validateToken(t)
				tm.mu.Lock()
				if ti, ok := tm.tokens[t]; ok {
					ti.Valid = valid
				}
				if !valid {
					tm.rebuildValidTokensLocked()
				}
				tm.mu.Unlock()
			}(info.Token)
		}
		invalidateAnonymousPoolSlots()
	}
	LogInfo("批量添加 token: +%d（重复 %d，失败 %d）", added, skipped, failed)
	return
}

// RemoveToken 从 TokenManager 删除一个 token，并更新文件
func (tm *TokenManager) RemoveToken(token string) error {
	tm.mu.Lock()
	if _, exists := tm.tokens[token]; !exists {
		tm.mu.Unlock()
		return fmt.Errorf("token 不存在")
	}
	delete(tm.tokens, token)
	tm.rebuildValidTokensLocked()
	tm.mu.Unlock()

	if err := tm.writeTokensToFile(); err != nil {
		return fmt.Errorf("写入文件失败: %v", err)
	}
	return nil
}

// ListTokens 返回当前所有 token 的快照
func (tm *TokenManager) ListTokens() []*TokenInfo {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	out := make([]*TokenInfo, 0, len(tm.tokens))
	for _, info := range tm.tokens {
		// 复制一份避免被外部修改
		copy := *info
		out = append(out, &copy)
	}
	return out
}

// ValidateTokenNow 立即验证某个 token 的有效性，返回最新状态
func (tm *TokenManager) ValidateTokenNow(token string) (bool, error) {
	tm.mu.RLock()
	_, exists := tm.tokens[token]
	tm.mu.RUnlock()
	if !exists {
		return false, fmt.Errorf("token 不存在")
	}

	valid := tm.validateToken(token)

	tm.mu.Lock()
	if t, ok := tm.tokens[token]; ok {
		t.Valid = valid
	}
	tm.rebuildValidTokensLocked()
	tm.mu.Unlock()
	return valid, nil
}

// rebuildValidTokensLocked 重建 validTokens 列表（必须在已持锁状态下调用）
func (tm *TokenManager) rebuildValidTokensLocked() {
	tm.validTokens = tm.validTokens[:0]
	for token, info := range tm.tokens {
		if info.Valid {
			tm.validTokens = append(tm.validTokens, token)
		}
	}
}

// writeTokensToFile 把当前所有 token 持久化。
// 优先走存储后端：FileBackend 全量重写 data/tokens.txt；MySQL 逐条 upsert。
// 后端不可用时回退到直接写文件。
func (tm *TokenManager) writeTokensToFile() error {
	tm.mu.RLock()
	records := make([]storageTokenRecord, 0, len(tm.tokens))
	for tk, info := range tm.tokens {
		records = append(records, storageTokenRecord{
			Token:       tk,
			Email:       info.Email,
			UserID:      info.UserID,
			Valid:       info.Valid,
			UseCount:    info.UseCount,
			LastChecked: info.LastChecked,
		})
	}
	tm.mu.RUnlock()

	b := storageBackend()
	if b != nil {
		// FileBackend 支持 RewriteTokens（全量覆盖，最高效）；
		// 其他后端（MySQL）逐条 upsert。
		if fb, ok := b.(interface{ RewriteTokens([]storageTokenRecord) error }); ok {
			return fb.RewriteTokens(records)
		}
		var firstErr error
		for _, r := range records {
			if err := b.UpsertToken(r); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return firstErr
	}

	// 回退：直接写文件（后端未初始化，如早期调用）
	return tm.writeTokensToFileRaw()
}

// writeTokensToFileRaw 直接写 data/tokens.txt（后端不可用时的兜底）。
func (tm *TokenManager) writeTokensToFileRaw() error {
	tm.mu.RLock()
	tokens := make([]string, 0, len(tm.tokens))
	for tk := range tm.tokens {
		tokens = append(tokens, tk)
	}
	tm.mu.RUnlock()

	tokenFile := filepath.Join(tm.dataDir, "tokens.txt")
	if err := os.MkdirAll(tm.dataDir, 0755); err != nil {
		return err
	}

	var sb strings.Builder
	sb.WriteString("# 用户 Token 文件 (由管理后台维护，可手动编辑)\n")
	sb.WriteString("# 每行一个 JWT token\n\n")
	for _, t := range tokens {
		sb.WriteString(t)
		sb.WriteString("\n")
	}

	tmpFile := tokenFile + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(sb.String()), 0644); err != nil {
		return err
	}
	return os.Rename(tmpFile, tokenFile)
}
