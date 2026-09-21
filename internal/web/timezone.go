package web

import (
	"time"

	"github.com/loarland/Forward2Any/internal/logging"
	"github.com/loarland/Forward2Any/internal/web/ui"
)

// refreshTimezone 把设置里的时区同步给页面时间展示与日志时间。
//
// 库里存的都是 Unix 秒，跟时区无关；只有渲染成字符串时才需要它。
// 启动时和保存设置之后各调一次。
func (s *Server) refreshTimezone() {
	loc := time.Local
	settings, err := s.store.Settings()
	if err != nil {
		s.log.Error("读取设置失败，时间先按系统时区显示", "err", err)
	} else {
		loc = settings.Location()
	}
	ui.SetLocation(loc)
	logging.SetLocation(loc)
}
