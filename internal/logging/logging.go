// Package logging 组装进程的日志器，并让日志时间跟随「设置 → 时区」。
//
// 容器里的系统时区通常是 UTC，日志和页面都按 UTC 显示就成了「时间不对」。
// 页面那边由 ui.SetLocation 管，这里管 stdout 上的日志时间。
package logging

import (
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// loc 是日志时间用的时区；没设过就是系统时区。
var loc atomic.Pointer[time.Location]

// SetLocation 设置日志时间使用的时区（nil 表示回到系统时区）。
func SetLocation(l *time.Location) {
	if l == nil {
		l = time.Local
	}
	loc.Store(l)
}

// New 造一个写 stdout 的 text 日志器。
//
// 时间戳用 SetLocation 设的时区：slog 默认写 time.Now() 的本地时区（容器里是 UTC），
// 这里在最外层把时间属性换算过去，日志和页面上的时间就能对上。
func New(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// 只管最外层的时间戳；分组里的同名键不动。
			if a.Key == slog.TimeKey && len(groups) == 0 {
				if l := loc.Load(); l != nil {
					a.Value = slog.TimeValue(a.Value.Time().In(l))
				}
			}
			return a
		},
	})
	return slog.New(h)
}
