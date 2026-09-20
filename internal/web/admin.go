package web

import (
	"net/http"
	"time"

	"github.com/loarland/Webhook2Any/internal/store"
)

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.Stats(startOfToday())
	if err != nil {
		s.fail(w, "统计投递情况失败", err)
		return
	}
	recent, err := s.store.ListDeliveries(store.DeliveryFilter{Limit: 10})
	if err != nil {
		s.fail(w, "读取投递记录失败", err)
		return
	}
	s.render(w, r, "dashboard", map[string]any{
		"Title":  "概览",
		"Nav":    "dashboard",
		"Stats":  stats,
		"Recent": recent,
	})
}

func startOfToday() int64 {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix()
}
