package engine

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

func samplePayload() map[string]any {
	return map[string]any{
		"action": "push",
		"repository": map[string]any{
			"private":   false,
			"full_name": "a/b",
		},
		"commits": []any{
			map[string]any{"message": "fix: [skip] ci", "id": "abc"},
			map[string]any{"message": "feat: add", "id": "def"},
		},
		"count": float64(3),
	}
}

func TestMatch(t *testing.T) {
	data := samplePayload()

	cases := []struct {
		name    string
		filters []store.Filter
		want    bool
	}{
		{"无过滤条件放行", nil, true},
		{"等值命中", []store.Filter{{Path: "action", Op: "eq", Value: "push"}}, true},
		{"等值未命中", []store.Filter{{Path: "action", Op: "eq", Value: "pull"}}, false},
		{"布尔按 JSON 值比", []store.Filter{{Path: "repository.private", Op: "eq", Value: false}}, true},
		{"布尔不等于字符串", []store.Filter{{Path: "repository.private", Op: "eq", Value: "true"}}, false},
		{"数字大于", []store.Filter{{Path: "count", Op: "gt", Value: float64(2)}}, true},
		{"数字与字符串比", []store.Filter{{Path: "count", Op: "lt", Value: "10"}}, true},
		{"数组下标取值", []store.Filter{{Path: "commits.1.id", Op: "eq", Value: "def"}}, true},
		{"数组越界视为不匹配", []store.Filter{{Path: "commits.9.id", Op: "eq", Value: "x"}}, false},
		{"字符串包含", []store.Filter{{Path: "commits.0.message", Op: "contains", Value: "[skip]"}}, true},
		{"字段缺少时 ne 为假", []store.Filter{{Path: "nope", Op: "ne", Value: "x"}}, false},
		{"存在", []store.Filter{{Path: "repository.full_name", Op: "exists"}}, true},
		{"不存在", []store.Filter{{Path: "repository.nope", Op: "not_exists"}}, true},
		{"正则", []store.Filter{{Path: "repository.full_name", Op: "regex", Value: `^a/`}}, true},
		{"in 命中", []store.Filter{{Path: "action", Op: "in", Value: []any{"push", "tag"}}}, true},
		{"in 未命中", []store.Filter{{Path: "action", Op: "in", Value: []any{"tag"}}}, false},
		{"多条件全满足", []store.Filter{
			{Path: "action", Op: "eq", Value: "push"},
			{Path: "repository.private", Op: "eq", Value: false},
		}, true},
		{"多条件有一条不满足", []store.Filter{
			{Path: "action", Op: "eq", Value: "push"},
			{Path: "repository.private", Op: "eq", Value: true},
		}, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Match(c.filters, data); got != c.want {
				t.Errorf("Match() = %v, want %v", got, c.want)
			}
		})
	}
}

// 报文不是 JSON 时过滤条件无从匹配，应当一律不命中（而不是误转发）。
func TestMatchOnNonJSONPayload(t *testing.T) {
	f := []store.Filter{{Path: "action", Op: "eq", Value: "push"}}
	if Match(f, nil) {
		t.Error("非 JSON 报文不应命中带路径的过滤条件")
	}
	if !Match(nil, nil) {
		t.Error("没有过滤条件时应当放行")
	}
}

func TestRenderBody(t *testing.T) {
	data := &TemplateData{
		Payload: map[string]any{
			"repository": map[string]any{"full_name": "a/b"},
			"commits":    []any{1, 2},
		},
		Raw:     `{"raw":true}`,
		Source:  map[string]any{"name": "GitHub", "slug": "gh"},
		Headers: map[string]string{"X-Test": "1"},
		TraceID: "t1",
	}

	got, err := RenderBody("", data)
	if err != nil {
		t.Fatal(err)
	}
	if got != data.Raw {
		t.Errorf("空模板应原样透传，得到 %q", got)
	}

	got, err = RenderBody(`{{.Payload.repository.full_name}} 有 {{len .Payload.commits}} 个提交`, data)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a/b 有 2 个提交" {
		t.Errorf("得到 %q", got)
	}

	if _, err := RenderBody("{{.Payload.", data); err == nil {
		t.Error("模板语法错误应当报错")
	}
}

// 非 JSON 报文下 .Payload 退化成 {"raw": ...}，模板仍要能用。
func TestRenderBodyNonJSON(t *testing.T) {
	data := &TemplateData{Payload: nil, Raw: "纯文本正文"}
	got, err := RenderBody(`{{.Payload.raw}}|{{.Raw}}`, data)
	if err != nil {
		t.Fatal(err)
	}
	if got != "纯文本正文|纯文本正文" {
		t.Errorf("得到 %q", got)
	}
}

func TestRenderHeaders(t *testing.T) {
	data := &TemplateData{
		Payload: map[string]any{"k": "v"},
		Source:  map[string]any{"slug": "gh"},
	}

	// 规则没配请求头时退回源上的固定请求头
	got, err := RenderHeaders("", data, map[string]string{"x-lower": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if got["X-Lower"] != "1" {
		t.Errorf("请求头名应被规范化，得到 %#v", got)
	}

	got, err = RenderHeaders(`{"X-Source":"{{.Source.slug}}","X-Static":"a"}`, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got["X-Source"] != "gh" || got["X-Static"] != "a" {
		t.Errorf("渲染结果不对: %#v", got)
	}

	if _, err := RenderHeaders(`["not","an","object"]`, data, nil); err == nil {
		t.Error("请求头模板不是对象时应当报错")
	}
}

func TestRenderSubjectAndDefault(t *testing.T) {
	in := &store.Source{Name: "GitHub"}
	data := &TemplateData{Payload: map[string]any{"subject": "构建成功"}, Raw: `{"subject":"构建成功"}`}

	got, err := RenderSubject("", data)
	if err != nil || got != "" {
		t.Fatalf("空模板应返回空串，得到 %q err=%v", got, err)
	}
	if s := DefaultSubject(in, data); s != "[GitHub] 构建成功" {
		t.Errorf("默认主题应取 payload.subject，得到 %q", s)
	}

	got, err = RenderSubject("{{.Source.name}} 通知", &TemplateData{Source: map[string]any{"name": "GH"}})
	if err != nil || got != "GH 通知" {
		t.Errorf("得到 %q err=%v", got, err)
	}
}

func TestTruncateKeepsUTF8(t *testing.T) {
	s := strings.Repeat("中", 10) // 每个字符 3 字节，共 30 字节
	cut := Truncate(s, 10)
	if !strings.HasPrefix(cut, "中") {
		t.Errorf("截断结果不应以半个字符开头: %q", cut)
	}
	if len(cut) == 0 || cut == s {
		t.Fatalf("应当被截断，得到 %q", cut)
	}
	if Truncate("abc", 10) != "abc" {
		t.Error("没超限时不应改动")
	}
	if Truncate("abc", 0) != "abc" {
		t.Error("上限为 0 表示不限制")
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if got := backoff(10, 1); got != 10 {
		t.Errorf("第 1 次应为基数，得到 %d", got)
	}
	if got := backoff(10, 2); got != 20 {
		t.Errorf("第 2 次应为 20，得到 %d", got)
	}
	if got := backoff(10, 3); got != 40 {
		t.Errorf("第 3 次应为 40，得到 %d", got)
	}
	if got := backoff(10, 100); got != 3600 {
		t.Errorf("应当封顶在 3600，得到 %d", got)
	}
	if got := backoff(0, 1); got != 10 {
		t.Errorf("基数非法时应回落为 10，得到 %d", got)
	}
}

func TestLoopDetection(t *testing.T) {
	hops := ParseHops(" a , b ,, c ")
	if strings.Join(hops, "|") != "a|b|c" {
		t.Errorf("解析跳链出错: %#v", hops)
	}
	if !IsLoop(hops, "b") {
		t.Error("跳链里已有的源应被判为循环")
	}
	if IsLoop(hops, "z") {
		t.Error("跳链里没有的源不应被判为循环")
	}
	if IsLoop(hops, "") {
		t.Error("空 slug 不应判为循环")
	}
	if got := ParseHops(""); len(got) != 0 {
		t.Errorf("空字符串应解析为空切片，得到 %#v", got)
	}
}

func TestParseFilterValue(t *testing.T) {
	cases := []struct {
		in   string
		want any
	}{
		{"push", "push"},
		{"true", true},
		{"false", false},
		{"123", float64(123)},
		{`"quoted"`, "quoted"},
		{"", ""},
		{"[1,2]", nil}, // 只校验它是切片，下面再看
	}
	for _, c := range cases {
		got := ParseFilterValue(c.in)
		if c.want == nil {
			if _, ok := got.([]any); !ok {
				t.Errorf("ParseFilterValue(%q) 应为切片，得到 %#v", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("ParseFilterValue(%q) = %#v, want %#v", c.in, got, c.want)
		}
	}
}

// waitDeliveryStatus 轮询等待最新一条投递进入指定状态。
func waitDeliveryStatus(t *testing.T, st *store.Store, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		list, err := st.ListDeliveries(store.DeliveryFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(list) > 0 && list[0].Status == want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	list, _ := st.ListDeliveries(store.DeliveryFilter{})
	got := "(没有记录)"
	if len(list) > 0 {
		got = list[0].Status
	}
	t.Fatalf("等待状态 %q 超时，当前是 %q", want, got)
}

// 失败一次后必须落到 failed（退避等待中），而不是退回 pending ——
// 否则日志里分不清「还没试过」和「试过在等重试」，概览页的「待重试」也永远是 0。
func TestRetryGoesThroughFailedThenDead(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 重试调快，让这个测试几秒内跑完
	if err := st.SetSettings(map[string]string{
		store.KeyRetryMax:            "2",
		store.KeyRetryBackoffSeconds: "2",
	}); err != nil {
		t.Fatal(err)
	}

	// 一个永远返回 500 的目标
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	out := &store.Source{
		Name: "坏目标", Kind: "webhook", Usage: "out", Enabled: true,
		URL: srv.URL, HTTPMethod: "POST", Headers: "{}",
	}
	in := &store.Source{
		Name: "入口", Kind: "webhook", Usage: "in", Enabled: true,
		Slug: "in1", HTTPMethod: "POST", Headers: "{}",
	}
	for _, s := range []*store.Source{out, in} {
		if err := st.SaveSource(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveRule(&store.Rule{
		Name: "r", Enabled: true,
		FromSourceIDs: []int64{in.ID}, ToSourceIDs: []int64{out.ID},
	}); err != nil {
		t.Fatal(err)
	}

	eng := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.Start()
	defer eng.Stop()

	if _, err := eng.Submit(&Inbound{
		Source: in, Payload: []byte(`{"a":1}`), Parsed: map[string]any{"a": float64(1)},
		Headers: map[string]string{}, TraceID: "t1",
	}); err != nil {
		t.Fatal(err)
	}

	waitDeliveryStatus(t, st, store.StatusFailed, 5*time.Second)

	// 次数用尽后应当放弃，而不是无限重试
	waitDeliveryStatus(t, st, store.StatusDead, 15*time.Second)

	list, err := st.ListDeliveries(store.DeliveryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Attempt != 2 {
		t.Errorf("重试上限是 2，尝试次数应为 2，实际 %d", list[0].Attempt)
	}
	if list[0].ResponseCode != http.StatusInternalServerError {
		t.Errorf("应当记录目标的响应码 500，实际 %d", list[0].ResponseCode)
	}
	if list[0].LastError == "" {
		t.Error("应当记录失败原因")
	}
}

// 成功路径：投递完成后状态为 success，并记录响应码。
func TestDeliverySuccessRecordsResponse(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var gotTrace, gotHops string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTrace = r.Header.Get("X-F2A-Trace")
		gotHops = r.Header.Get("X-F2A-Hops")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("done"))
	}))
	defer srv.Close()

	out := &store.Source{
		Name: "目标", Kind: "webhook", Usage: "out", Enabled: true,
		URL: srv.URL, HTTPMethod: "POST", Headers: `{"X-Fixed":"yes"}`,
	}
	in := &store.Source{
		Name: "入口", Kind: "webhook", Usage: "in", Enabled: true,
		Slug: "in2", HTTPMethod: "POST", Headers: "{}",
	}
	for _, s := range []*store.Source{out, in} {
		if err := st.SaveSource(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveRule(&store.Rule{
		Name: "r", Enabled: true,
		FromSourceIDs: []int64{in.ID}, ToSourceIDs: []int64{out.ID},
	}); err != nil {
		t.Fatal(err)
	}

	eng := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.Start()
	defer eng.Stop()

	if _, err := eng.Submit(&Inbound{
		Source: in, Payload: []byte(`{"hello":"world"}`),
		Parsed:  map[string]any{"hello": "world"},
		Headers: map[string]string{"Content-Type": "application/json"},
		TraceID: "trace-abc",
	}); err != nil {
		t.Fatal(err)
	}

	waitDeliveryStatus(t, st, store.StatusSuccess, 5*time.Second)

	if gotTrace != "trace-abc" {
		t.Errorf("转发应当带上 trace，实际 %q", gotTrace)
	}
	if gotHops != "in2" {
		t.Errorf("转发应当带上跳链 in2，实际 %q", gotHops)
	}

	list, err := st.ListDeliveries(store.DeliveryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if list[0].ResponseCode != http.StatusCreated {
		t.Errorf("应当记录 201，实际 %d", list[0].ResponseCode)
	}
	if list[0].ResponseBody != "done" {
		t.Errorf("应当记录响应体，实际 %q", list[0].ResponseBody)
	}
}

// 过滤器没命中时不应该产生任何投递记录。
func TestSubmitSkipsUnmatchedRules(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	out := &store.Source{Name: "目标", Kind: "webhook", Usage: "out", Enabled: true, URL: "http://127.0.0.1:1/x", HTTPMethod: "POST", Headers: "{}"}
	in := &store.Source{Name: "入口", Kind: "webhook", Usage: "in", Enabled: true, Slug: "in3", HTTPMethod: "POST", Headers: "{}"}
	for _, s := range []*store.Source{out, in} {
		if err := st.SaveSource(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveRule(&store.Rule{
		Name: "只转发 push", Enabled: true,
		FromSourceIDs: []int64{in.ID}, ToSourceIDs: []int64{out.ID},
		Filters: []store.Filter{{Path: "action", Op: "eq", Value: "push"}},
	}); err != nil {
		t.Fatal(err)
	}

	eng := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	n, err := eng.Submit(&Inbound{
		Source: in, Payload: []byte(`{"action":"pull"}`),
		Parsed: map[string]any{"action": "pull"}, TraceID: "t2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("过滤条件未命中不应入队，实际 %d 条", n)
	}

	list, err := st.ListDeliveries(store.DeliveryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("不该有任何投递记录，实际 %d 条", len(list))
	}
}

// 被停用的目标源不该收到消息。
func TestSubmitSkipsDisabledTarget(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	out := &store.Source{Name: "停用目标", Kind: "webhook", Usage: "out", Enabled: false, URL: "http://127.0.0.1:1/x", HTTPMethod: "POST", Headers: "{}"}
	in := &store.Source{Name: "入口", Kind: "webhook", Usage: "in", Enabled: true, Slug: "in4", HTTPMethod: "POST", Headers: "{}"}
	for _, s := range []*store.Source{out, in} {
		if err := st.SaveSource(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveRule(&store.Rule{
		Name: "r", Enabled: true,
		FromSourceIDs: []int64{in.ID}, ToSourceIDs: []int64{out.ID},
	}); err != nil {
		t.Fatal(err)
	}

	eng := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	n, err := eng.Submit(&Inbound{Source: in, Payload: []byte(`{}`), Parsed: map[string]any{}, TraceID: "t3"})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("停用的目标源不该产生投递，实际 %d 条", n)
	}
}
