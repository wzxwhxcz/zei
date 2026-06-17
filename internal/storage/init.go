package storage

import (
	"fmt"
	"sync"
)

// 全局后端实例（在程序启动时由 InitBackend 设置）。
var (
	current Backend = NoopBackend{}
	mu      sync.RWMutex
)

// Get 返回当前后端。未初始化时返回 NoopBackend。
func Get() Backend {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Set 设置当前后端（供测试或显式初始化使用）。
func Set(b Backend) {
	mu.Lock()
	current = b
	mu.Unlock()
}

// InitBackend 按 env 选择后端：DatabaseURL 设了用 MySQL，否则用文件后端。
// 返回的后端已就绪（MySQL 会建表、测试连接）。
func InitBackend(databaseURL string) (Backend, error) {
	if databaseURL == "" {
		b, err := NewFileBackend("data")
		if err != nil {
			return nil, fmt.Errorf("init file backend: %w", err)
		}
		Set(b)
		return b, nil
	}
	b, err := NewMysqlBackend(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("init mysql backend: %w", err)
	}
	Set(b)
	return b, nil
}
