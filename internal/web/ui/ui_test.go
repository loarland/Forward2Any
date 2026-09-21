package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"
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
