package ui

import (
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
