package internal

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	logger   *slog.Logger
	logLevel = new(slog.LevelVar)
	logOnce  sync.Once
)

type colorHandler struct {
	writer io.Writer
	level  *slog.LevelVar
}

var levelColorMap = map[slog.Level]string{
	slog.LevelDebug: "\033[36m", // 青色
	slog.LevelInfo:  "\033[32m", // 绿色
	slog.LevelWarn:  "\033[33m", // 黄色
	slog.LevelError: "\033[31m", // 红色
}

var levelNameMap = map[slog.Level]string{
	slog.LevelDebug: "DEBUG",
	slog.LevelInfo:  "INFO",
	slog.LevelWarn:  "WARN",
	slog.LevelError: "ERROR",
}

const resetColor = "\033[0m"

func (h *colorHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *colorHandler) Handle(_ context.Context, r slog.Record) error {
	timestamp := r.Time.Format("2006/01/02 15:04:05")
	caller := "unknown.go"
	if r.PC != 0 {
		frames := runtime.CallersFrames([]uintptr{r.PC})
		f, _ := frames.Next()
		if f.File != "" {
			caller = filepath.Base(f.File)
		}
	}
	color := levelColorMap[r.Level]
	name := levelNameMap[r.Level]
	if name == "" {
		name = r.Level.String()
	}
	fmt.Fprintf(h.writer, "%s [%s] %s[%s]%s %s\n", timestamp, caller, color, name, resetColor, r.Message)
	return nil
}

func (h *colorHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *colorHandler) WithGroup(_ string) slog.Handler      { return h }

func InitLogger() {
	logOnce.Do(func() {
		logLevel.Set(slog.LevelInfo)
	})

	if Cfg != nil && Cfg.DebugLogging {
		logLevel.Set(slog.LevelDebug)
	}

	if Cfg != nil && Cfg.LogLevel != "" {
		switch strings.ToLower(Cfg.LogLevel) {
		case "debug":
			logLevel.Set(slog.LevelDebug)
		case "warn":
			logLevel.Set(slog.LevelWarn)
		case "error":
			logLevel.Set(slog.LevelError)
		default:
			logLevel.Set(slog.LevelInfo)
		}
	}

	logger = slog.New(&colorHandler{writer: os.Stdout, level: logLevel})
	slog.SetDefault(logger)
}

func logMsg(level slog.Level, format string, v ...interface{}) {
	if logger == nil {
		return
	}
	if !logger.Enabled(context.Background(), level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	r := slog.NewRecord(time.Now(), level, fmt.Sprintf(format, v...), pcs[0])
	_ = logger.Handler().Handle(context.Background(), r)
}

func LogDebug(format string, v ...interface{}) {
	logMsg(slog.LevelDebug, format, v...)
}

func LogInfo(format string, v ...interface{}) {
	logMsg(slog.LevelInfo, format, v...)
}

func LogWarn(format string, v ...interface{}) {
	logMsg(slog.LevelWarn, format, v...)
}

func LogError(format string, v ...interface{}) {
	logMsg(slog.LevelError, format, v...)
}
