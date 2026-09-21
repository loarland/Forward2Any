// Package ui 持有内嵌的模板与静态资源。
//
// 全部用 embed 编译进二进制，容器里不需要额外的静态文件目录。
package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var StaticFS embed.FS

// displayLoc 是页面上展示时间用的时区。
//
// 数据库里存的一律是 Unix 秒，跟时区无关；只有渲染成字符串时才需要它。
// 默认跟随系统（容器里通常是 UTC），设置页保存时由 SetLocation 换成用户选的。
var displayLoc atomic.Pointer[time.Location]

// SetLocation 设置页面展示时间用的时区（nil 表示回到系统时区）。
func SetLocation(loc *time.Location) {
	if loc == nil {
		loc = time.Local
	}
	displayLoc.Store(loc)
}

func location() *time.Location {
	if loc := displayLoc.Load(); loc != nil {
		return loc
	}
	return time.Local
}

var funcs = template.FuncMap{
	"ts": func(v int64) string {
		if v == 0 {
			return "—"
		}
		return time.Unix(v, 0).In(location()).Format("2006-01-02 15:04:05")
	},
	"truncate": func(n int, s string) string {
		r := []rune(s)
		if len(r) <= n {
			return s
		}
		return string(r[:n]) + "…"
	},
	// pretty 把 JSON 字符串格式化成缩进样式，失败时原样返回。
	"pretty": func(s string) string {
		if strings.TrimSpace(s) == "" {
			return ""
		}
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			return s
		}
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return s
		}
		return string(b)
	},
	"add":        func(a, b int) int { return a + b },
	"sub":        func(a, b int) int { return a - b },
	"join":       strings.Join,
	"statusText": statusText,
	"kindText":   kindText,
	"usageText":  usageText,
	// 类型下拉、列表筛选都用这一份，省得界面上漏掉新加的类型。
	"kindOptions": func() []string { return store.AllKinds },
	// 内置渠道的 kind 列表（逗号分隔），表单里按它决定显示哪一组字段。
	"channelKinds": func() string { return strings.Join(store.ChannelKinds, ",") },
	// 需要填「推送地址」的类型：自定义 Webhook 与所有内置渠道。
	// Telegram 不在里面 —— 它的地址是拼出来的，表单上只填 token。
	"urlKinds": func() string {
		return strings.Join(append([]string{"webhook"}, store.ChannelKinds...), ",")
	},
	// 「发送代理」那一块是 Webhook / Telegram / 内置渠道共用的：
	// data-when 里要列全，不然新渠道的源表单上不会出现这个勾选框。
	"proxyKinds": func() string {
		kinds := append([]string{"webhook", "telegram"}, store.ChannelKinds...)
		return strings.Join(kinds, ",")
	},
}

func statusText(s string) string {
	switch s {
	case "pending":
		return "待投递"
	case "success":
		return "成功"
	case "failed":
		return "失败"
	case "dead":
		return "已放弃"
	case "dropped":
		return "已拦截"
	}
	return s
}

func kindText(k string) string { return store.KindLabel(k) }

func usageText(u string) string {
	switch u {
	case "in":
		return "接收"
	case "out":
		return "发送"
	case "both":
		return "接收 + 发送"
	}
	return u
}

// Render 用 layout.html 套着指定页面模板渲染。
//
// 每个页面单独组一个 template 集合：页面文件各自定义 "content"，
// 放在同一个集合里会互相覆盖。
func Render(w io.Writer, page string, data any) error {
	t, err := template.New(page).Funcs(funcs).ParseFS(
		templatesFS, "templates/layout.html", "templates/"+page+".html")
	if err != nil {
		return fmt.Errorf("解析模板 %s: %w", page, err)
	}
	return t.ExecuteTemplate(w, "layout", data)
}

// HasPage 报告某个页面模板是否存在，便于测试遗漏的模板。
func HasPage(page string) bool {
	_, err := templatesFS.ReadFile("templates/" + page + ".html")
	return err == nil
}
