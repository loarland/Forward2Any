package web

import (
	"bytes"
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
	"github.com/loarland/Forward2Any/internal/web/ui"
)

func TestIPAllowed(t *testing.T) {
	cases := []struct {
		allow, ip string
		want      bool
	}{
		{"", "1.2.3.4", true},
		{"1.2.3.4", "1.2.3.4", true},
		{"1.2.3.4", "1.2.3.5", false},
		{"10.0.0.0/8", "10.1.2.3", true},
		{"10.0.0.0/8", "11.1.2.3", false},
		{"1.2.3.4, 10.0.0.0/8", "10.9.9.9", true},
		{" 1.2.3.4 ,, ", "1.2.3.4", true},
		{"::1", "::1", true},
		{"10.0.0.0/8", "not-an-ip", false},
		{"10.0.0.0/8", "10.1.2.3.4", false},
	}
	for _, c := range cases {
		if got := ipAllowed(c.allow, c.ip); got != c.want {
			t.Errorf("ipAllowed(%q, %q) = %v, want %v", c.allow, c.ip, got, c.want)
		}
	}
}

func TestValidateIPAllow(t *testing.T) {
	for _, ok := range []string{"", "1.2.3.4", "10.0.0.0/8", "1.2.3.4, ::1"} {
		if err := validateIPAllow(ok); err != nil {
			t.Errorf("%q 应当合法，得到 %v", ok, err)
		}
	}
	for _, bad := range []string{"1.2.3", "10.0.0.0/33", "abc"} {
		if err := validateIPAllow(bad); err == nil {
			t.Errorf("%q 应当被判为非法", bad)
		}
	}
}

func TestVerifyInbound(t *testing.T) {
	body := []byte(`{"hello":"world"}`)

	t.Run("不校验", func(t *testing.T) {
		src := &store.Source{AuthMode: "none"}
		if !verifyInbound(src, httptest.NewRequest("POST", "/hook/x", nil), body) {
			t.Error("none 模式应当放行")
		}
	})

	t.Run("固定密钥", func(t *testing.T) {
		src := &store.Source{AuthMode: "token", AuthSecret: "s3cret"}

		ok := httptest.NewRequest("POST", "/hook/x", nil)
		ok.Header.Set("X-F2A-Token", "s3cret")
		if !verifyInbound(src, ok, body) {
			t.Error("正确密钥应当通过")
		}

		bad := httptest.NewRequest("POST", "/hook/x", nil)
		bad.Header.Set("X-F2A-Token", "wrong")
		if verifyInbound(src, bad, body) {
			t.Error("错误密钥不应通过")
		}

		missing := httptest.NewRequest("POST", "/hook/x", nil)
		if verifyInbound(src, missing, body) {
			t.Error("没带密钥不应通过")
		}

		// 没配密钥时不能放行，否则等于把端点敞开
		empty := &store.Source{AuthMode: "token"}
		if verifyInbound(empty, ok, body) {
			t.Error("未配置密钥时不应通过")
		}
	})

	t.Run("HMAC 签名", func(t *testing.T) {
		secret := "topsecret"
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		sig := hex.EncodeToString(mac.Sum(nil))
		src := &store.Source{AuthMode: "hmac_sha256", AuthSecret: secret}

		ok := httptest.NewRequest("POST", "/hook/x", nil)
		ok.Header.Set("X-Hub-Signature-256", "sha256="+sig)
		if !verifyInbound(src, ok, body) {
			t.Error("正确签名应当通过")
		}

		tampered := httptest.NewRequest("POST", "/hook/x", nil)
		tampered.Header.Set("X-Hub-Signature-256", "sha256="+sig)
		if verifyInbound(src, tampered, []byte(`{"hello":"tampered"}`)) {
			t.Error("报文被改后签名不应通过")
		}

		// GitHub 有时用大写十六进制
		upper := httptest.NewRequest("POST", "/hook/x", nil)
		upper.Header.Set("X-Hub-Signature-256", "sha256="+string([]byte(sig)))
		if !verifyInbound(src, upper, body) {
			t.Error("签名校验不应受大小写影响")
		}
	})

	t.Run("HTTP Basic", func(t *testing.T) {
		src := &store.Source{AuthMode: "basic", AuthHeader: "user", AuthSecret: "pass"}

		ok := httptest.NewRequest("POST", "/hook/x", nil)
		ok.SetBasicAuth("user", "pass")
		if !verifyInbound(src, ok, body) {
			t.Error("正确账号应当通过")
		}

		bad := httptest.NewRequest("POST", "/hook/x", nil)
		bad.SetBasicAuth("user", "bad")
		if verifyInbound(src, bad, body) {
			t.Error("错误密码不应通过")
		}
	})

	t.Run("未知鉴权方式一律拒绝", func(t *testing.T) {
		src := &store.Source{AuthMode: "banana"}
		if verifyInbound(src, httptest.NewRequest("POST", "/hook/x", nil), body) {
			t.Error("不认识的鉴权方式应当拒绝而不是放行")
		}
	})
}

func TestParseFiltersRoundTrip(t *testing.T) {
	text := `# 只看 push
action eq push
repository.private eq false
commits.0.message contains [skip]
ref exists
`
	filters, err := ParseFilters(text)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(filters) != 4 {
		t.Fatalf("注释与空行不该计入，期望 4 条，得到 %d", len(filters))
	}
	if filters[1].Value != false {
		t.Errorf("false 应按布尔解析，得到 %#v", filters[1].Value)
	}
	if filters[2].Value != "[skip]" {
		t.Errorf("裸词应按字符串处理，得到 %#v", filters[2].Value)
	}
	if filters[3].Op != "exists" || filters[3].Value != nil {
		t.Errorf("exists 不应带值: %#v", filters[3])
	}

	again, err := ParseFilters(FiltersToText(filters))
	if err != nil {
		t.Fatalf("回填文本再次解析失败: %v", err)
	}
	if len(again) != len(filters) {
		t.Fatalf("往返后条数不一致: %d != %d", len(again), len(filters))
	}
	for i := range filters {
		if again[i].Path != filters[i].Path || again[i].Op != filters[i].Op {
			t.Errorf("第 %d 条往返不一致: %#v -> %#v", i, filters[i], again[i])
		}
	}
}

func TestParseFiltersErrors(t *testing.T) {
	bad := []string{
		"action",             // 缺操作符
		"action nope 1",      // 不认识的操作符
		"action eq",          // eq 需要值
		"repository.private", // 只有一个词
	}
	for _, s := range bad {
		if _, err := ParseFilters(s); err == nil {
			t.Errorf("%q 应当报错", s)
		}
	}

	good := []string{"ref exists", "action not_exists", "count gt 3"}
	for _, s := range good {
		if _, err := ParseFilters(s); err != nil {
			t.Errorf("%q 应当合法，得到 %v", s, err)
		}
	}
}

// 过滤条件写错时，表单里其他字段不能跟着被清空 —— 以前 ruleFromForm 每步失败都返回空 Rule，
// 用户改一个错别字就得把名称、源选择、模板全部重填。
func TestRuleFromFormKeepsInputOnError(t *testing.T) {
	form := url.Values{
		"name":             {"我的规则"},
		"enabled":          {"1"},
		"from_source_ids":  {"1", "2"},
		"to_source_ids":    {"3"},
		"filters":          {"action eq push\n第三行乱写\n"},
		"body_template":    {"{{.Payload.action}}"},
		"subject_template": {"主题"},
		"headers_template": {`{"X-A":"1"}`},
	}
	r := httptest.NewRequest("POST", "/rules", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}

	rule, err := (&Server{}).ruleFromForm(r)
	if err == nil {
		t.Fatal("过滤条件有语法错误，应当报错")
	}
	if !strings.Contains(err.Error(), "第 2 行") {
		t.Errorf("错误信息应当指出出问题的行号，得到 %q", err.Error())
	}
	if rule.Name != "我的规则" {
		t.Errorf("名称应当回填，得到 %q", rule.Name)
	}
	if len(rule.FromSourceIDs) != 2 || len(rule.ToSourceIDs) != 1 {
		t.Errorf("源选择应当回填，得到 from=%v to=%v", rule.FromSourceIDs, rule.ToSourceIDs)
	}
	if rule.BodyTemplate != "{{.Payload.action}}" || rule.SubjectTemplate != "主题" {
		t.Errorf("模板应当回填，得到 %q / %q", rule.BodyTemplate, rule.SubjectTemplate)
	}
	if rule.HeadersTemplate != `{"X-A":"1"}` {
		t.Errorf("请求头模板应当回填，得到 %q", rule.HeadersTemplate)
	}
	if !rule.Enabled {
		t.Error("启用状态应当回填")
	}

	// 名称缺失这类错误同样要能回填过滤条件和源选择。
	form.Set("name", "")
	form.Set("filters", "action eq push")
	r2 := httptest.NewRequest("POST", "/rules", strings.NewReader(form.Encode()))
	r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r2.ParseForm()
	rule2, err := (&Server{}).ruleFromForm(r2)
	if err == nil {
		t.Fatal("名称为空应当报错")
	}
	if len(rule2.FromSourceIDs) != 2 {
		t.Errorf("名称为空时源选择也应回填，得到 %v", rule2.FromSourceIDs)
	}
	if len(rule2.Filters) != 1 || rule2.Filters[0].Path != "action" {
		t.Errorf("名称为空时过滤条件也应回填，得到 %#v", rule2.Filters)
	}
}

// 翻页链接要带上生效的筛选条件，同时不把空条件也塞进 URL。
func TestDeliveryPageURL(t *testing.T) {
	cases := []struct {
		name    string
		f       store.DeliveryFilter
		keyword string
		page    int
		want    string
	}{
		{"没有筛选就只剩页码", store.DeliveryFilter{}, "", 3, "/deliveries?page=3"},
		{"带状态", store.DeliveryFilter{Status: "dead"}, "", 2, "/deliveries?page=2&status=dead"},
		{"带源和规则", store.DeliveryFilter{InSourceID: 7, RuleID: 9}, "", 1,
			"/deliveries?page=1&rule=9&source=7"},
		{"关键字要带上", store.DeliveryFilter{}, "飞书", 4, "/deliveries?page=4&q=%E9%A3%9E%E4%B9%A6"},
		{"全都有", store.DeliveryFilter{Status: "failed", InSourceID: 1, RuleID: 2}, "a b", 5,
			"/deliveries?page=5&q=a+b&rule=2&source=1&status=failed"},
	}
	for _, c := range cases {
		if got := deliveryPageURL(c.f, c.keyword, c.page); got != c.want {
			t.Errorf("%s: deliveryPageURL = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidateSourceShape(t *testing.T) {
	base := func() *store.Source {
		return &store.Source{
			Name: "测试", Kind: "webhook", Usage: "in",
			Headers: "{}", HTTPMethod: "POST", Slug: "ok-slug",
		}
	}

	if err := validateSourceShape(base()); err != nil {
		t.Errorf("基本配置应当合法: %v", err)
	}

	noName := base()
	noName.Name = ""
	if err := validateSourceShape(noName); err == nil {
		t.Error("缺名称应当报错")
	}

	badKind := base()
	badKind.Kind = "sms"
	if err := validateSourceShape(badKind); err == nil {
		t.Error("未知类型应当报错")
	}

	badSlug := base()
	badSlug.Slug = "Bad Slug!"
	if err := validateSourceShape(badSlug); err == nil {
		t.Error("非法路径标识应当报错")
	}

	noURL := base()
	noURL.Usage = "out"
	noURL.URL = ""
	if err := validateSourceShape(noURL); err == nil {
		t.Error("发送用途缺目标地址应当报错")
	}

	badHeaders := base()
	badHeaders.Headers = "{not json}"
	if err := validateSourceShape(badHeaders); err == nil {
		t.Error("非法请求头 JSON 应当报错")
	}

	// 接收用途且没写 slug 时应自动分配一个
	auto := base()
	auto.Slug = ""
	if err := validateSourceShape(auto); err != nil {
		t.Fatalf("自动生成 slug 不该报错: %v", err)
	}
	if auto.Slug == "" {
		t.Error("应当自动生成 slug")
	}

	// 轮询间隔过短会被纠正，而不是报错
	short := base()
	short.Kind = "email"
	short.Usage = "in"
	short.IMAPHost = "imap.example.com"
	short.IMAPUser = "u"
	short.IMAPInterval = 1
	if err := validateSourceShape(short); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if short.IMAPInterval < 10 {
		t.Errorf("间隔应被纠正到至少 10 秒，得到 %d", short.IMAPInterval)
	}
}

func TestSameOrigin(t *testing.T) {
	get := httptest.NewRequest("GET", "http://example.com/settings", nil)
	if !sameOrigin(get) {
		t.Error("GET 不做来源校验")
	}

	// 非浏览器客户端不带 Origin/Referer
	script := httptest.NewRequest("POST", "http://example.com/settings", nil)
	if !sameOrigin(script) {
		t.Error("没有 Origin/Referer 的请求应当放行")
	}

	same := httptest.NewRequest("POST", "http://example.com/settings", nil)
	same.Header.Set("Origin", "http://example.com")
	if !sameOrigin(same) {
		t.Error("同源请求应当放行")
	}

	cross := httptest.NewRequest("POST", "http://example.com/settings", nil)
	cross.Header.Set("Origin", "http://evil.example")
	if sameOrigin(cross) {
		t.Error("跨站请求应当被拒绝")
	}

	referer := httptest.NewRequest("POST", "http://example.com/settings", nil)
	referer.Header.Set("Referer", "http://evil.example/x")
	if sameOrigin(referer) {
		t.Error("跨站 Referer 应当被拒绝")
	}
}

// 后台页面必须禁用缓存。否则浏览器会用启发式缓存/bfcache 端出旧页面：
// 典型症状是「新建规则」页里源的下拉列表是过期的，刷新一下才对。
func TestAdminPagesAreNotCached(t *testing.T) {
	s := &Server{}
	h := s.noStoreHTML(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/", "/sources", "/rules/new", "/deliveries", "/settings", "/login"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-store") {
			t.Errorf("%s 的 Cache-Control 应当含 no-store，实际 %q", path, cc)
		}
	}
}

// 静态资源反过来应该允许缓存，不然每次都要重新下载 htmx。
func TestStaticAssetsRemainCacheable(t *testing.T) {
	s := &Server{}
	h := s.noStoreHTML(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/static/app.css", nil))
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "no-store") {
		t.Errorf("静态资源不该被设成 no-store，实际 %q", cc)
	}
}

// ---------- gzip ----------

// gunzip 解压响应体，顺便验证它确实是个完整的 gzip 流
// （少写 Close 的话这里会报 unexpected EOF）。
func gunzip(t *testing.T, body []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("响应体不是 gzip 流：%v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("读取 gzip 流失败（多半是没 Close）：%v", err)
	}
	return string(out)
}

func gzipServer(t *testing.T, handler http.HandlerFunc, path, accept, rangeHdr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if accept != "" {
		req.Header.Set("Accept-Encoding", accept)
	}
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	rec := httptest.NewRecorder()
	gzipIfAccepted(handler).ServeHTTP(rec, req)
	return rec
}

func TestGzipCompressesLargeBodyAndDropsContentLength(t *testing.T) {
	big := strings.Repeat("换主题只改一个文件。", 200)
	rec := gzipServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(big)))
		w.Write([]byte(big))
	}, "/x", "gzip, deflate", "")

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("应当压缩，实际 Content-Encoding=%q", got)
	}
	// 压缩后长度变了，Content-Length 必须摘掉，否则浏览器会按原长度读并截断。
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Errorf("压缩后不该还留着 Content-Length，实际 %q", got)
	}
	if got := gunzip(t, rec.Body.Bytes()); got != big {
		t.Errorf("解压后内容不一致")
	}
}

// 长度已知且很短就不压。
//
// 前提是「长度已知」：处理器没设 Content-Length 时（healthz、/hook/ 这类小响应），
// 中间件在响应头定稿那一刻无从知道大小，会照压 —— 压完反而大几十字节，但 HTTP 语义
// 完全正确。要避免就得先把响应体攒起来再决定，为这点收益不值得。
func TestGzipSkipsSmallBodies(t *testing.T) {
	rec := gzipServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "7")
		w.Write([]byte("ok 200\n"))
	}, "/healthz", "gzip", "")

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("小响应不该压，实际 Content-Encoding=%q", got)
	}
	if got := rec.Body.String(); got != "ok 200\n" {
		t.Errorf("小响应应当原样返回，实际 %q", got)
	}
}

func TestGzipSkipsWhenClientDoesNotAcceptIt(t *testing.T) {
	big := strings.Repeat("x", 4096)
	rec := gzipServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(big))
	}, "/x", "", "")

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("客户端没要 gzip 就不该压，实际 %q", got)
	}
	if rec.Body.Len() != len(big) {
		t.Errorf("body 应当原样返回，长度 %d，期望 %d", rec.Body.Len(), len(big))
	}
	// 但 Vary 必须写上，否则中间缓存会把压过的响应发给不要 gzip 的客户端。
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Encoding") {
		t.Errorf("Vary 里应当有 Accept-Encoding，实际 %q", got)
	}
}

// 静态文件走 http.ServeContent，它会设 Content-Length、也会处理 Range。
// 这两条是最容易把响应写坏的地方，所以单独钉住。
func TestGzipKeepsRangeRequestsIntact(t *testing.T) {
	body := []byte(strings.Repeat("abcdefghij", 500))
	rec := gzipServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "app.css", time.Time{}, bytes.NewReader(body))
	}, "/static/app.css", "gzip", "bytes=0-9")

	if got := rec.Header().Get("Content-Encoding"); got != "" {
		t.Errorf("Range 请求不该压，实际 Content-Encoding=%q", got)
	}
	if rec.Code != http.StatusPartialContent {
		t.Errorf("状态码应当是 206，实际 %d", rec.Code)
	}
	if got := rec.Body.String(); got != "abcdefghij" {
		t.Errorf("Range 内容不对：%q", got)
	}
}

func TestGzipSkipsBodylessStatuses(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotModified} {
		rec := gzipServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}, "/x", "gzip", "")
		if got := rec.Header().Get("Content-Encoding"); got != "" {
			t.Errorf("%d 不该有响应体，也就不该有 Content-Encoding，实际 %q", code, got)
		}
	}
}

// 静态资源真的能压下来，而且压完还能解回原样 —— 顺带守住 pico/theme 有没有被打进二进制。
func TestGzipServesEmbeddedAssetsCompressed(t *testing.T) {
	h := gzipIfAccepted(http.FileServerFS(ui.StaticFS))

	for _, name := range []string{"app.css", "theme.css", "pico.min.css"} {
		req := httptest.NewRequest("GET", "/static/"+name, nil)
		req.Header.Set("Accept-Encoding", "gzip")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s 应当能取到，实际 %d", name, rec.Code)
		}
		if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
			t.Fatalf("%s 应当被压缩，实际 Content-Encoding=%q", name, got)
		}
		plain := gunzip(t, rec.Body.Bytes())
		if len(plain) <= rec.Body.Len() {
			t.Errorf("%s 压完没变小：%d -> %d", name, len(plain), rec.Body.Len())
		}
		if !strings.Contains(plain, "--") {
			t.Errorf("%s 的内容看起来不对", name)
		}
	}
}

// theme.css 是唯一一处写颜色的地方，令牌必须齐；app.css 里不该再出现字面量颜色。
func TestThemeTokensCoverAppCSS(t *testing.T) {
	app, err := ui.StaticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	theme, err := ui.StaticFS.ReadFile("static/theme.css")
	if err != nil {
		t.Fatal(err)
	}

	// 令牌可以定义在 theme.css（颜色）或 app.css（--radius 这种非颜色的）里。
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(--[a-z0-9-]+):`).FindAllStringSubmatch(string(theme)+string(app), -1) {
		defined[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`var\((--[a-z0-9-]+)\)`).FindAllStringSubmatch(string(app), -1) {
		if !defined[m[1]] {
			t.Errorf("app.css 用了 theme.css 里没定义的令牌 %s", m[1])
		}
	}

	// app.css 只写结构：除了 var(...) 之外不该有字面量颜色。
	stripped := regexp.MustCompile(`var\(--[a-z0-9-]+\)`).ReplaceAllString(string(app), "")
	if m := regexp.MustCompile(`#[0-9a-fA-F]{3,6}\b`).FindString(stripped); m != "" {
		t.Errorf("app.css 里还有硬编码颜色 %s，应当挪到 theme.css", m)
	}
}

// ---------- 主题 / 配色 ----------

// testServer 给那些只需要一个能渲染/能记日志的 Server 的测试用。
// log 不能留 nil：渲染失败时 render 会去记日志，nil 会把真正的模板错误盖成 panic。
func testServer() *Server {
	return &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// palettes 那张表是配色名的唯一权威，必须和目录里的文件严格一一对应：
// 少登记一个 → 有文件但选不到；多登记一个 → 下拉框里选了会 404。
func TestThemePalettesMatchFiles(t *testing.T) {
	files, err := paletteFiles()
	if err != nil {
		t.Fatal(err)
	}
	inList := map[string]bool{}
	for _, p := range palettes {
		inList[p.Name] = true
		if p.Label == "" {
			t.Errorf("配色 %s 没有中文名", p.Name)
		}
	}
	for _, f := range files {
		if !inList[f] {
			t.Errorf("有配色文件 %s.css 但没登记在 palettes 里，用户选不到", f)
		}
	}
	onDisk := map[string]bool{}
	for _, f := range files {
		onDisk[f] = true
	}
	for _, p := range palettes {
		if !onDisk[p.Name] {
			t.Errorf("palettes 里登记了 %s 但没有 static/palettes/%s.css", p.Name, p.Name)
		}
	}
	if !inList[DefaultThemeColor] {
		t.Errorf("默认配色 %s 不在列表里", DefaultThemeColor)
	}
	if len(files) < 2 {
		t.Errorf("配色文件只有 %d 个，像是没生成", len(files))
	}
}

func TestThemeValueValidation(t *testing.T) {
	if !validThemeColor(DefaultThemeColor) {
		t.Error("默认配色应当合法")
	}
	if !validThemeMode(DefaultThemeMode) {
		t.Error("默认亮暗应当合法")
	}
	// 这两个值会进 <link href> 和 <html data-theme>，必须挡住任意字符串。
	for _, bad := range []string{"", "nope", "../app", "blue.css", "'; alert(1); //"} {
		if validThemeColor(bad) {
			t.Errorf("%q 不该被当成合法配色", bad)
		}
	}
	for _, bad := range []string{"", "nope", "Dark", "auto light"} {
		if validThemeMode(bad) {
			t.Errorf("%q 不该被当成合法亮暗", bad)
		}
	}
}

// 外观必须出现在每个页面的布局里 —— 不然登录页和设置页会跟后台其它页长得不一样。
func TestRenderInjectsThemeEverywhere(t *testing.T) {
	s := testServer()
	// 覆盖三种页面：不带顶栏的登录页、要统计数据的概览页、表单型设置页。
	// 其余页面走的是同一个布局和同一个 render，e2e 里有整轮页面渲染的断言。
	pages := map[string]map[string]any{
		"login":     {"S": &store.Settings{}},
		"dashboard": {"Stats": map[string]int{}},
		"settings":  {"S": &store.Settings{}, "Palettes": palettes, "ThemeModes": themeModes},
	}
	for page, extra := range pages {
		data := map[string]any{}
		for k, v := range extra {
			data[k] = v
		}
		rec := httptest.NewRecorder()
		s.render(rec, httptest.NewRequest("GET", "/", nil), page, data)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s 渲染失败：%d %s", page, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		want := "/static/palettes/" + DefaultThemeColor + ".css"
		if !strings.Contains(body, want) {
			t.Errorf("%s 没带上配色文件 %s", page, want)
		}
		// auto 档不写 data-theme，交给 Pico 跟随系统。
		if strings.Contains(body, "data-theme=") {
			t.Errorf("auto 档不该给 <html> 写 data-theme：%s", page)
		}
	}
}

// 选成 light/dark 时要把 data-theme 写出去，Pico 的 conditional 构建认这个属性。
func TestRenderWritesExplicitThemeMode(t *testing.T) {
	s := testServer()
	for _, mode := range []string{"light", "dark"} {
		s.theme.Store(themeChoice{Color: "jade", Mode: mode})
		rec := httptest.NewRecorder()
		s.render(rec, httptest.NewRequest("GET", "/", nil), "login", map[string]any{"S": &store.Settings{}})
		body := rec.Body.String()
		if !strings.Contains(body, `data-theme="`+mode+`"`) {
			t.Errorf("%s 档应当写出 data-theme=%q", mode, mode)
		}
		if !strings.Contains(body, "/static/palettes/jade.css") {
			t.Errorf("%s 档没带上选中的配色文件", mode)
		}
	}
}

// 库里存了非法值时不能把它拼进 href，回落到默认。
func TestRefreshThemeFallsBackOnGarbage(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSettings(map[string]string{
		store.KeyThemeColor: `"><script>alert(1)</script>`,
		store.KeyThemeMode:  "banana",
	}); err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.store = st
	s.refreshTheme()

	got := s.themeChoice()
	if got.Color != DefaultThemeColor || got.Mode != DefaultThemeMode {
		t.Errorf("非法值应当回落到默认，实际 %q/%q", got.Color, got.Mode)
	}

	rec := httptest.NewRecorder()
	s.render(rec, httptest.NewRequest("GET", "/", nil), "login", map[string]any{"S": &store.Settings{}})
	if strings.Contains(rec.Body.String(), "<script>alert(1)</script>") {
		t.Error("非法配色被拼进了页面")
	}
}
