package internal

import (
	"sync"
	"time"

	"zai-proxy/internal/storage"
)

// usageLogger 异步批量把 UsageRecord 写入存储后端。
// 避免每次请求一次 DB 写：先入 buffered channel，后台 goroutine 批量 flush。
type usageLogger struct {
	ch     chan storage.UsageRecord
	wg     sync.WaitGroup
	stopCh chan struct{}
}

const (
	usageChanSize    = 4096
	usageFlushEvery  = 30 * time.Second // 定时 flush
	usageBatchMax    = 100              // 攒满就 flush
)

var globalUsage = &usageLogger{
	ch:     make(chan storage.UsageRecord, usageChanSize),
	stopCh: make(chan struct{}),
}

// StartUsageLogger 启动后台 flush goroutine（幂等，多次调用安全）。
var usageStarted sync.Once

func StartUsageLogger() {
	usageStarted.Do(func() {
		globalUsage.wg.Add(1)
		go globalUsage.loop()
	})
}

// RecordUsageFull 记录一次完整的请求用量（异步，不阻塞主流程）。
// 在 chat.go 的成功和失败路径都调用，补齐失败请求的用量（之前缺失）。
func RecordUsageFull(rec storage.UsageRecord) {
	if rec.TS.IsZero() {
		rec.TS = time.Now()
	}
	select {
	case globalUsage.ch <- rec:
	default:
		// channel 满：丢弃避免阻塞（usage 是尽力而为，不能影响请求）
		LogDebug("[usage] channel 满，丢弃一条用量记录")
	}
}

func (u *usageLogger) loop() {
	defer u.wg.Done()
	batch := make([]storage.UsageRecord, 0, usageBatchMax)
	ticker := time.NewTicker(usageFlushEvery)
	defer ticker.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		b := storageBackend()
		if b == nil {
			batch = batch[:0]
			return
		}
		for _, rec := range batch {
			// FileBackend/MySQL 的 RecordUsage 都是单条；这里逐条写（后端内部已优化）。
			if err := b.RecordUsage(rec); err != nil {
				LogDebug("[usage] 写入失败: %v", err)
				break // 后端出错就放弃本批，避免刷屏
			}
		}
		batch = batch[:0]
	}
	for {
		select {
		case rec := <-u.ch:
			batch = append(batch, rec)
			if len(batch) >= usageBatchMax {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-u.stopCh:
			// 关闭：drain 剩余
			for {
				select {
				case rec := <-u.ch:
					batch = append(batch, rec)
				default:
					flush()
					return
				}
			}
		}
	}
}

// StopUsageLogger 优雅关闭（drain 剩余记录）。供测试/关闭用。
func StopUsageLogger() {
	close(globalUsage.stopCh)
	globalUsage.wg.Wait()
}
