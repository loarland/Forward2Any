package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/loarland/Forward2Any/internal/store"
)

func (s *Server) registerDeliveries(mux *http.ServeMux) {
	mux.HandleFunc("GET /deliveries", s.handleDeliveryList)
	mux.HandleFunc("GET /deliveries/{id}", s.handleDeliveryDetail)
	mux.HandleFunc("POST /deliveries/{id}/replay", s.handleDeliveryReplay)
	mux.HandleFunc("POST /deliveries/{id}/delete", s.handleDeliveryDelete)
}

func (s *Server) handleDeliveryList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	keyword := strings.TrimSpace(q.Get("q"))
	f := store.DeliveryFilter{
		Status:  q.Get("status"),
		Keyword: keyword,
	}
	if v, err := strconv.ParseInt(q.Get("source"), 10, 64); err == nil {
		f.InSourceID = v
	}
	if v, err := strconv.ParseInt(q.Get("rule"), 10, 64); err == nil {
		f.RuleID = v
	}

	// 先数总数再取这一页：每页条数由 URL 决定，页码要按总数夹一次，
	// 否则手工改成 page=999 会翻到一页空白。
	total, err := s.store.CountDeliveries(f)
	if err != nil {
		s.fail(w, "统计投递记录失败", err)
		return
	}
	p := newPager(q, deliveriesPageSize, total)
	f.Limit, f.Offset = p.PerPage, p.Offset()

	items, err := s.store.ListDeliveries(f)
	if err != nil {
		s.fail(w, "读取投递记录失败", err)
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

	p.fillURLs(func(page int) string { return deliveryPageURL(f, keyword, page, p.PerPage) })

	s.render(w, r, "deliveries", map[string]any{
		"Title":     "投递日志",
		"Nav":       "deliveries",
		"Items":     items,
		"Total":     total,
		"Pager":     p,
		"Sources":   sources,
		"Rules":     rules,
		"FStatus":   f.Status,
		"FSource":   f.InSourceID,
		"FRule":     f.RuleID,
		"FKeyword":  keyword,
		"HasFilter": f.Status != "" || f.InSourceID != 0 || f.RuleID != 0 || keyword != "",
	})
}

// deliveryPageURL 构造投递日志的翻页链接。只带上真正生效的筛选条件，
// 不然 URL 里会挂一串 status=&source=0&rule=0；关键字也必须带过去，
// 否则翻到第二页筛选就悄悄丢了。默认值（第一页、默认每页条数）不写进 URL。
func deliveryPageURL(f store.DeliveryFilter, keyword string, page, perPage int) string {
	v := url.Values{}
	if f.Status != "" {
		v.Set("status", f.Status)
	}
	if f.InSourceID != 0 {
		v.Set("source", strconv.FormatInt(f.InSourceID, 10))
	}
	if f.RuleID != 0 {
		v.Set("rule", strconv.FormatInt(f.RuleID, 10))
	}
	if keyword != "" {
		v.Set("q", keyword)
	}
	if page > 1 {
		v.Set("page", strconv.Itoa(page))
	}
	if perPage != deliveriesPageSize {
		v.Set("per_page", strconv.Itoa(perPage))
	}
	if len(v) == 0 {
		return "/deliveries"
	}
	return "/deliveries?" + v.Encode()
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
