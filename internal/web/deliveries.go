package web

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/loarland/Forward2Any/internal/store"
)

const deliveriesPerPage = 50

func (s *Server) registerDeliveries(mux *http.ServeMux) {
	mux.HandleFunc("GET /deliveries", s.handleDeliveryList)
	mux.HandleFunc("GET /deliveries/{id}", s.handleDeliveryDetail)
	mux.HandleFunc("POST /deliveries/{id}/replay", s.handleDeliveryReplay)
	mux.HandleFunc("POST /deliveries/{id}/delete", s.handleDeliveryDelete)
}

func (s *Server) handleDeliveryList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	f := store.DeliveryFilter{
		Status: q.Get("status"),
		Limit:  deliveriesPerPage,
		Offset: (page - 1) * deliveriesPerPage,
	}
	if v, err := strconv.ParseInt(q.Get("source"), 10, 64); err == nil {
		f.InSourceID = v
	}
	if v, err := strconv.ParseInt(q.Get("rule"), 10, 64); err == nil {
		f.RuleID = v
	}

	items, err := s.store.ListDeliveries(f)
	if err != nil {
		s.fail(w, "读取投递记录失败", err)
		return
	}
	total, err := s.store.CountDeliveries(f)
	if err != nil {
		s.fail(w, "统计投递记录失败", err)
		return
	}
	sources, err := s.store.ListSources()
	if err != nil {
		s.fail(w, "读取源列表失败", err)
		return
	}
	rules, err := s.store.ListRules()
	if err != nil {
		s.fail(w, "读取规则列表失败", err)
		return
	}

	s.render(w, r, "deliveries", map[string]any{
		"Title":    "投递日志",
		"Nav":      "deliveries",
		"Items":    items,
		"Total":    total,
		"Page":     page,
		"HasPrev":  page > 1,
		"HasNext":  page*deliveriesPerPage < total,
		"PrevPage": page - 1,
		"NextPage": page + 1,
		"Sources":  sources,
		"Rules":    rules,
		"FStatus":  f.Status,
		"FSource":  f.InSourceID,
		"FRule":    f.RuleID,
	})
}

func (s *Server) handleDeliveryDetail(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.store.GetDelivery(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "读取投递记录失败", err)
		return
	}

	// 目标源可能已经被删掉了，这时只展示记录本身。
	var out *store.Source
	if d.OutSourceID != 0 {
		if v, err := s.store.GetSource(d.OutSourceID); err == nil {
			out = v
		}
	}
	s.render(w, r, "delivery_detail", map[string]any{
		"Title":  "投递详情",
		"Nav":    "deliveries",
		"Item":   d,
		"Target": out,
	})
}

func (s *Server) handleDeliveryReplay(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.ReplayDelivery(id); err != nil {
		s.fail(w, "重置投递记录失败", err)
		return
	}
	s.engine.Wake()
	s.log.Info("手动重放投递", "id", id)
	http.Redirect(w, r, fmt.Sprintf("/deliveries/%d?ok=replayed", id), http.StatusSeeOther)
}

func (s *Server) handleDeliveryDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteDelivery(id); err != nil {
		s.fail(w, "删除投递记录失败", err)
		return
	}
	http.Redirect(w, r, "/deliveries?ok=deleted", http.StatusSeeOther)
}
