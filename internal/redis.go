package internal

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisCache 可选的 Redis 缓存层（captcha token 池共享 + token 轮询指针 + 失败熔断）。
// 所有操作容错：Redis 不可用时静默降级，不影响主流程。
type redisCache struct {
	client *redis.Client
	enabled bool
}

var (
	rcOnce  sync.Once
	rc      *redisCache
)

const (
	rcCaptchaTTL = 200 * time.Second // captcha token 缓存（略小于上游有效期）
	rcFailTTL    = 5 * time.Minute   // token 失败熔断窗口
)

// InitRedis 初始化 Redis 缓存（REDIS_URL 设了才启用）。失败静默降级。
func InitRedis() {
	rcOnce.Do(func() {
		rc = &redisCache{enabled: false}
		if Cfg.RedisURL == "" {
			return
		}
		opts, err := redis.ParseURL(Cfg.RedisURL)
		if err != nil {
			LogError("Redis URL 解析失败，跳过 Redis 缓存: %v", err)
			return
		}
		c := redis.NewClient(opts)
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Ping(ctx).Err(); err != nil {
			LogError("Redis ping 失败，跳过 Redis 缓存: %v", err)
			_ = c.Close()
			return
		}
		rc.client = c
		rc.enabled = true
		LogInfo("Redis 缓存已启用")
	})
}

// rcCtx 带超时的 context（避免 Redis 卡住阻塞请求）。
func rcCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Second)
}

// ---- captcha token 缓存 ----

// RedisGetCaptcha 从 Redis 取 captcha token（命中直接用）。未启用/未命中返回 ""。
func RedisGetCaptcha(scene string) string {
	if rc == nil || !rc.enabled {
		return ""
	}
	ctx, cancel := rcCtx()
	defer cancel()
	v, err := rc.client.Get(ctx, fmt.Sprintf("zai2api:captcha:%s", scene)).Result()
	if err != nil {
		return ""
	}
	return v
}

// RedisSetCaptcha 写入 captcha token 到 Redis（带 TTL）。
func RedisSetCaptcha(scene, token string) {
	if rc == nil || !rc.enabled || token == "" {
		return
	}
	ctx, cancel := rcCtx()
	defer cancel()
	_ = rc.client.Set(ctx, fmt.Sprintf("zai2api:captcha:%s", scene), token, rcCaptchaTTL).Err()
}

// ---- token 轮询指针（多实例原子轮询）----

// RedisNextTokenIndex 原子自增返回轮询索引（mod total）。未启用返回 -1（调用方回退到本地指针）。
func RedisNextTokenIndex(total int) int {
	if rc == nil || !rc.enabled || total <= 0 {
		return -1
	}
	ctx, cancel := rcCtx()
	defer cancel()
	n, err := rc.client.Incr(ctx, "zai2api:token:rr").Result()
	if err != nil {
		return -1
	}
	return int((n - 1) % int64(total))
}

// ---- token 失败熔断 ----

// RedisMarkTokenFail 标记某 token 短期失败（5分钟内跳过）。
func RedisMarkTokenFail(token string) {
	if rc == nil || !rc.enabled || token == "" {
		return
	}
	ctx, cancel := rcCtx()
	defer cancel()
	_ = rc.client.Set(ctx, "zai2api:token:fail:"+token, "1", rcFailTTL).Err()
}

// RedisIsTokenFailed 查询某 token 是否在熔断窗口内。
func RedisIsTokenFailed(token string) bool {
	if rc == nil || !rc.enabled || token == "" {
		return false
	}
	ctx, cancel := rcCtx()
	defer cancel()
	n, err := rc.client.Exists(ctx, "zai2api:token:fail:"+token).Result()
	return err == nil && n > 0
}
