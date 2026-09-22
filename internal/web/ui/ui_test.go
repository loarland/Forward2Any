package ui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

// 页面时间要按 ui.SetLocation 设的时区显示（设置页保存时由 server 同步过来）。
func TestDisplayTimeFollowsLocation(t *testing.T) {
	SetLocation(time.UTC)
	defer SetLocation(nil)

	ts := funcs["ts"].(func(int64) string)
	const stamp = int64(1789980000) // 2026-09-21 的一个固定时刻

	utc := ts(stamp)
	SetLocation(time.FixedZone("CST", 8*3600))
	cst := ts(stamp)

	if utc == cst {
		t.Fatalf("换时区后显示的时间应当不同，实际都是 %q", utc)
	}
	u, err := time.Parse("2006-01-02 15:04:05", utc)
	if err != nil {
		t.Fatalf("UTC 渲染结果格式不对：%q", utc)
	}
	c, err := time.Parse("2006-01-02 15:04:05", cst)
	if err != nil {
		t.Fatalf("CST 渲染结果格式不对：%q", cst)
	}
	if diff := c.Sub(u); diff != 8*time.Hour {
		t.Errorf("两个时区应当差 8 小时，实际 %v（UTC=%s CST=%s）", diff, utc, cst)
	}

	// 0 值仍然显示成破折号
	if got := ts(0); got != "—" {
		t.Errorf("0 应当显示成 —，实际 %q", got)
	}
}

// 每个密码框后面都得跟一个「显示 / 隐藏」按钮（pw-toggle），而且要和输入框一起
// 包在 .pw-group 里 —— 少了这个壳，按钮就没法跟输入框贴在一起、拉成一样高。
// 漏一个的表现是那一格和别处长得不一样，静态扫一遍比逐个页面渲染更省事。
func TestPasswordInputsHaveToggle(t *testing.T) {
	files, err := templatesFS.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	// <input ... type="password" ...> 之后紧跟 {{template "pw-toggle"}}
	pair := regexp.MustCompile(`(?s)<input[^>]*type="password"[^>]*>\s*\{\{template "pw-toggle"\}\}`)
	inputs := regexp.MustCompile(`type="password"`)
	toggles := regexp.MustCompile(`\{\{template "pw-toggle"\}\}`)

	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".html") || f.Name() == "layout.html" {
			continue // layout.html 里是那个按钮的定义本身
		}
		b, err := templatesFS.ReadFile("templates/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		nIn, nPair, nTog := len(inputs.FindAllString(s, -1)), len(pair.FindAllString(s, -1)), len(toggles.FindAllString(s, -1))
		if nIn != nPair {
			t.Errorf("%s：有 %d 个密码框，其中 %d 个后面跟了 pw-toggle 按钮", f.Name(), nIn, nPair)
		}
		if nIn != nTog {
			t.Errorf("%s：密码框 %d 个，pw-toggle 按钮 %d 个", f.Name(), nIn, nTog)
		}
		if n := strings.Count(s, `class="pw-group"`); n != nIn {
			t.Errorf("%s：密码框 %d 个，但 .pw-group 只有 %d 个（输入框和按钮要在同一个 flex 容器里）", f.Name(), nIn, n)
		}
	}
}

// 三个列表页都要能渲染出翻页条。模板写坏（字段名拼错、{{end}} 不配对）时
// 只有真渲染一次才看得出来 —— 服务端那两处是运行时才报「模板渲染失败」。
func TestListPagesRenderPager(t *testing.T) {
	sizes := []int{10, 20, 50, 100, 200}
	src := &store.Source{ID: 1, Name: "入口", Kind: "webhook", Usage: "in", Enabled: true, Slug: "in1"}
	rule := &store.Rule{ID: 1, Name: "转发", Enabled: true, FromSourceIDs: []int64{1}, ToSourceIDs: []int64{2}}

	cases := []struct {
		page string
		data map[string]any
		want []string
	}{
		{
			page: "sources",
			data: map[string]any{
				"Title": "源", "Nav": "sources", "PageSize": 20, "Sizes": sizes,
				"Rows": []map[string]any{{"Src": src, "HookURL": "http://x/hook/in1", "Curl": "curl x"}},
			},
			// 前端翻页：控件齐了，页码由 app.js 填
			want: []string{`data-pager-client data-size="20"`, "data-pg-size", "data-pg-first",
				"data-pg-prev", "data-pg-cur", "data-pg-next", "data-pg-last", "data-pg-go", "每页"},
		},
		{
			page: "rules",
			data: map[string]any{
				"Title": "规则", "Nav": "rules", "PageSize": 50, "Sizes": sizes,
				"Rows": []map[string]any{{"Rule": rule, "From": []string{"入口"}, "To": []string{"目标"}}},
			},
			want: []string{`data-pager-client data-size="50"`, "data-pg-cur"},
		},
		{
			page: "deliveries",
			data: map[string]any{
				"Title": "投递日志", "Nav": "deliveries", "Total": 120, "HasFilter": false,
				"Items":   []*store.Delivery{{ID: 1, Status: "success", CreatedAt: 1789980000}},
				"Sources": []*store.Source{src}, "Rules": []*store.Rule{rule},
				"FStatus": "", "FSource": int64(0), "FRule": int64(0), "FKeyword": "",
				"Pager": map[string]any{
					"Page": 2, "PerPage": 20, "Total": 120, "Pages": 6, "Sizes": sizes,
					"HasPrev": true, "HasNext": true,
					"FirstURL": "/deliveries", "PrevURL": "/deliveries",
					"NextURL": "/deliveries?page=3&per_page=20", "LastURL": "/deliveries?page=6&per_page=20",
				},
			},
			// 服务端翻页：真链接 + 表单（换每页条数要提交）
			want: []string{`data-pg-cur>2<`, `data-pg-pages>6<`, `name="per_page"`,
				`href="/deliveries?page=3&amp;per_page=20"`, `name="page" value="2"`,
				`data-pg-submit`, "跳转"},
		},
	}

	for _, c := range cases {
		var buf bytes.Buffer
		if err := Render(&buf, c.page, c.data); err != nil {
			t.Errorf("%s: 渲染失败: %v", c.page, err)
			continue
		}
		html := buf.String()
		for _, w := range c.want {
			if !strings.Contains(html, w) {
				t.Errorf("%s: 渲染结果里没有 %q", c.page, w)
			}
		}
	}
}
