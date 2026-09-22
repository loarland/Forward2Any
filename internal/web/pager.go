package web

import (
	"net/url"
	"strconv"
)

// perPageOptions 是列表页「每页 N 条」允许的档位。
// URL 是用户能随手改的，不在这几个值里就按默认值处理，不为此报错。
var perPageOptions = []int{10, 20, 50, 100, 200}

// 各列表默认每页多少条：源是卡片（一屏放不下几张），规则和投递日志是表格。
const (
	sourcesPageSize    = 20
	rulesPageSize      = 50
	deliveriesPageSize = 50
)

// pager 是列表页共用的翻页状态。
// Total 是当前筛选条件下的总条数，Pages 按每页条数算出来（至少 1 页）。
// Page 已经夹在 [1, Pages] 里，页面直接拿去算 Offset 就不会翻到空页。
type pager struct {
	Page    int
	PerPage int
	Total   int
	Pages   int

	// Sizes 是「每页 N 条」下拉框的档位。
	Sizes []int

	// 四个方向的链接，只有服务端翻页的列表（投递日志）会填。
	// URL 里带着当前的全部筛选条件与每页条数，点进去筛选不会丢。
	FirstURL string
	PrevURL  string
	NextURL  string
	LastURL  string
}

// newPager 解析 URL 里的 page / per_page 并归一化。
// total 是当前筛选条件下的总条数。
func newPager(q url.Values, defaultPerPage, total int) *pager {
	perPage := defaultPerPage
	if v, err := strconv.Atoi(q.Get("per_page")); err == nil {
		for _, ok := range perPageOptions {
			if v == ok {
				perPage = v
				break
			}
		}
	}

	pages := (total + perPage - 1) / perPage
	if pages < 1 {
		pages = 1
	}
	page := 1
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 1 {
		page = v
	}
	if page > pages {
		page = pages
	}

	return &pager{
		Page:    page,
		PerPage: perPage,
		Total:   total,
		Pages:   pages,
		Sizes:   perPageOptions,
	}
}

// Offset 是本页第一条记录在结果集里的下标。
func (p *pager) Offset() int { return (p.Page - 1) * p.PerPage }

func (p *pager) HasPrev() bool { return p.Page > 1 }
func (p *pager) HasNext() bool { return p.Page < p.Pages }

// fillURLs 用「给一个页码、返回该页地址」的函数补齐四个方向的链接。
// 页码超出范围时也照样填（首页永远是 1、末页永远是 Pages），
// 模板按 HasPrev / HasNext 把到头的方向置灰。
func (p *pager) fillURLs(pageURL func(page int) string) {
	p.FirstURL = pageURL(1)
	p.PrevURL = pageURL(p.Page - 1)
	p.NextURL = pageURL(p.Page + 1)
	p.LastURL = pageURL(p.Pages)
}
