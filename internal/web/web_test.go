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
// limiter 也一样：登录那条路径第一件事就是查它，nil 会空指针。
func testServer() *Server {
	return &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil)), limiter: newLoginLimiter()}
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

// 代理设置：类型和地址要么都对，要么都空着。
func TestValidateProxy(t *testing.T) {
	ok := []struct{ kind, addr string }{
		{"", ""},
		{"none", ""},
		{"none", "127.0.0.1:7890"}, // 选了「不使用」，地址留着当备忘也不影响
		{"http", "127.0.0.1:7890"},
		{"https", "proxy.corp.example:3128"},
		{"socks5", "10.0.0.1:1080"},
		{"socks5", "user:pw@127.0.0.1:1080"},
		{"http", "[::1]:7890"},
	}
	for _, c := range ok {
		if err := validateProxy(c.kind, c.addr); err != nil {
			t.Errorf("%q/%q 应当合法，得到 %v", c.kind, c.addr, err)
		}
	}

	bad := []struct{ kind, addr string }{
		{"gopher", "127.0.0.1:1"},
		{"socks4", "127.0.0.1:1080"},
		{"http", ""},                   // 选了类型却没填地址
		{"http", "127.0.0.1"},          // 缺端口
		{"http", "http://127.0.0.1:1"}, // 类型在上面选，地址不带前缀
		{"http", ":7890"},              // 没有主机名
		{"http", "127.0.0.1:0"},
		{"http", "127.0.0.1:99999"},
		{"http", "127.0.0.1:abc"},
	}
	for _, c := range bad {
		if err := validateProxy(c.kind, c.addr); err == nil {
			t.Errorf("%q/%q 应当被判为非法", c.kind, c.addr)
		}
	}
}

// 设置页可能只提交一部分字段（比如只改配色）。这种情况下代理配置必须原样保留，
// 否则改一次外观就会把代理抹掉。非法值则必须被挡下、原值不变。
func TestSettingsSaveKeepsProxyOnPartialPost(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.store = st
	s.port = 16000

	if err := st.SetSettings(map[string]string{
		store.KeyProxyType: "socks5", store.KeyProxyAddr: "127.0.0.1:1080",
	}); err != nil {
		t.Fatal(err)
	}

	save := func(extra map[string]string) *httptest.ResponseRecorder {
		form := url.Values{
			"web_port": {"16000"}, "base_url": {"http://localhost:16000"},
			"admin_user": {"admin"}, "retry_max": {"5"}, "retry_backoff_seconds": {"10"},
			"payload_max_bytes": {"65536"}, "log_retention_days": {"30"},
		}
		for k, v := range extra {
			form.Set(k, v)
		}
		req := httptest.NewRequest("POST", "/settings", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		s.handleSettingsSave(rec, req)
		return rec
	}

	// 只改外观：代理不动
	save(map[string]string{"theme_color": "jade", "theme_mode": "dark"})
	after, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if after.ProxyType != "socks5" || after.ProxyAddr != "127.0.0.1:1080" {
		t.Errorf("只改外观不该动到代理，实际 %q/%q", after.ProxyType, after.ProxyAddr)
	}

	// 非法地址：报错，且原值不变
	rec := save(map[string]string{"proxy_type": "http", "proxy_addr": "http://127.0.0.1:7890"})
	if !strings.Contains(rec.Body.String(), "代理地址") {
		t.Errorf("非法代理地址应当给出提示，实际：%s", rec.Body.String()[:min(200, rec.Body.Len())])
	}
	after, err = st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if after.ProxyType != "socks5" || after.ProxyAddr != "127.0.0.1:1080" {
		t.Errorf("非法提交不该改掉已存的代理，实际 %q/%q", after.ProxyType, after.ProxyAddr)
	}

	// 合法提交：写进去
	save(map[string]string{"proxy_type": "http", "proxy_addr": "127.0.0.1:7890"})
	after, err = st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if after.ProxyURL() != "http://127.0.0.1:7890" {
		t.Errorf("代理没存住：%q", after.ProxyURL())
	}

	// 切回「不使用代理」：地址可以留空
	save(map[string]string{"proxy_type": "none", "proxy_addr": ""})
	after, err = st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if after.ProxyURL() != "" {
		t.Errorf("选了不使用代理之后不该还拼得出地址：%q", after.ProxyURL())
	}
}

// 「走代理发送」只在 webhook + 用途含发送时才认；其它情况沿用原值。
func TestSourceFormUseProxyRules(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.store = st

	parse := func(base *store.Source, form url.Values) *store.Source {
		req := httptest.NewRequest("POST", "/sources", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		src, err := s.sourceFromForm(req, base)
		if err != nil {
			t.Fatalf("表单解析失败: %v", err)
		}
		return src
	}
	webhookForm := func(usage string, useProxy bool) url.Values {
		v := url.Values{
			"name": {"源"}, "kind": {"webhook"}, "usage": {usage}, "enabled": {"1"},
			"headers": {"{}"}, "http_method": {"POST"}, "url": {"http://example.com/x"},
		}
		if useProxy {
			v.Set("use_proxy", "1")
		}
		return v
	}

	// 发送源勾上 → 存下来
	got := parse(&store.Source{}, webhookForm("out", true))
	if !got.UseProxy {
		t.Error("发送源勾了代理却没存下来")
	}
	// 发送源取消勾选 → 清掉
	got = parse(&store.Source{UseProxy: true}, webhookForm("both", false))
	if got.UseProxy {
		t.Error("取消勾选后应当清掉代理开关")
	}
	// 用途是接收 → 表单里的 use_proxy 被忽略
	got = parse(&store.Source{}, webhookForm("in", true))
	if got.UseProxy {
		t.Error("用途不含发送时不该记下代理开关")
	}
	// 用途从发送改成接收 → 原来的勾要留着（改回发送时还照旧）
	got = parse(&store.Source{UseProxy: true}, webhookForm("in", false))
	if !got.UseProxy {
		t.Error("改成接收用途不该把原来的代理开关抹掉")
	}
	// 邮件源压根没有这个字段（SMTP 不走代理）→ 沿用原值
	mailForm := url.Values{
		"name": {"邮件"}, "kind": {"email"}, "usage": {"out"}, "enabled": {"1"},
		"headers": {"{}"}, "smtp_host": {"smtp.example.com"}, "smtp_port": {"587"},
		"smtp_tls": {"starttls"}, "mail_from": {"a@example.com"}, "mail_to": {"b@example.com"},
		"use_proxy": {"1"},
	}
	if got := parse(&store.Source{}, mailForm); got.UseProxy {
		t.Error("邮件源不该记下代理开关")
	}
}

// 源表单上的代理开关：没配代理时禁用（勾了也不生效），配了就显示当前代理地址。
func TestSourceFormRendersProxyToggle(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.store = st

	tagFor := func(useProxy bool) (string, string) {
		rec := httptest.NewRecorder()
		src := &store.Source{Kind: "webhook", Usage: "out", Headers: "{}", UseProxy: useProxy}
		s.renderSourceForm(rec, httptest.NewRequest("GET", "/sources/new", nil), src, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("源表单渲染失败：%d %s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		tag := regexp.MustCompile(`<input[^>]*name="use_proxy"[^>]*>`).FindString(body)
		if tag == "" {
			t.Fatal("源表单里没有代理开关")
		}
		return tag, body
	}

	// 还没配代理：开关禁用，并说清楚勾了也直连
	tag, body := tagFor(false)
	if !strings.Contains(tag, "disabled") {
		t.Errorf("没配代理时开关应当禁用，实际：%s", tag)
	}
	if !strings.Contains(body, "还没配置代理") {
		t.Error("没配代理时应当给出提示")
	}

	// 配上代理：开关可用，并把地址显示出来
	if err := st.SetSettings(map[string]string{
		store.KeyProxyType: "socks5", store.KeyProxyAddr: "127.0.0.1:1080",
	}); err != nil {
		t.Fatal(err)
	}
	tag, body = tagFor(true)
	if strings.Contains(tag, "disabled") {
		t.Errorf("配了代理之后开关不该再禁用，实际：%s", tag)
	}
	if !strings.Contains(tag, "checked") {
		t.Errorf("勾了代理的源应当回填成选中，实际：%s", tag)
	}
	if !strings.Contains(body, "socks5://127.0.0.1:1080") {
		t.Error("表单里应当显示当前生效的代理地址")
	}
}

// ---------- 规则表单的「选源」两栏 ----------

// ruleFormWithSources 造三个用途不同的源，渲染一次规则表单。
// 返回整页 HTML 和「接收源」「目标源」两栏各自的 HTML；规则由 build 拿到源 id 后自己拼。
func ruleFormWithSources(t *testing.T, build func(ids map[string]int64) *store.Rule) (body, from, to string) {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]int64{}
	for _, v := range []struct{ name, usage string }{
		{"只接收的源", "in"},
		{"只发送的源", "out"},
		{"两用的源", "both"},
	} {
		src := &store.Source{Name: v.name, Kind: "webhook", Usage: v.usage, Enabled: true, Headers: "{}"}
		if err := st.SaveSource(src); err != nil {
			t.Fatal(err)
		}
		ids[v.name] = src.ID
	}

	s := testServer()
	s.store = st
	rec := httptest.NewRecorder()
	s.renderRuleForm(rec, httptest.NewRequest("GET", "/rules/new", nil), build(ids), "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("规则表单渲染失败：%d %s", rec.Code, rec.Body.String())
	}
	body = rec.Body.String()

	// 两栏是相邻的两个兄弟节点，所以「接收源」栏就是从它的标记到「目标源」的标记。
	fromStart := strings.Index(body, `data-picker="from"`)
	toStart := strings.Index(body, `data-picker="to"`)
	if fromStart < 0 || toStart < 0 || toStart < fromStart {
		t.Fatalf("规则表单里没有找到两栏选源列表：from=%d to=%d", fromStart, toStart)
	}
	return body, body[fromStart:toStart], body[toStart:]
}

// rowOf 返回某一行的定位串。带上结尾空格是为了只匹配那一行的 data-search，
// 而不是页面其它地方出现的源名。
func rowOf(name string) string { return `data-search="` + name + ` ` }

// 用途不合适的源默认不出现在那一栏里：放上去只会让人以为配了就能用。
// 判断依据跟服务端执行时一致（接收侧 CanReceive、发送侧 CanSend）。
func TestRuleFormPickerFiltersByUsage(t *testing.T) {
	_, from, to := ruleFormWithSources(t, func(map[string]int64) *store.Rule {
		return &store.Rule{Enabled: true}
	})

	if !strings.Contains(from, rowOf("只接收的源")) || !strings.Contains(from, rowOf("两用的源")) {
		t.Errorf("接收源栏应当列出用途含接收的源，实际：%s", from)
	}
	if strings.Contains(from, rowOf("只发送的源")) {
		t.Error("只发送的源不该出现在接收源栏里")
	}
	if !strings.Contains(to, rowOf("只发送的源")) || !strings.Contains(to, rowOf("两用的源")) {
		t.Errorf("目标源栏应当列出用途含发送的源，实际：%s", to)
	}
	if strings.Contains(to, rowOf("只接收的源")) {
		t.Error("只接收的源不该出现在目标源栏里")
	}

	// 被藏起来的源要说一声，不然用户只会觉得「我的源怎么没了」。
	if !strings.Contains(from, "另有 1 个源用途不含接收") {
		t.Error("接收源栏应当说明有源因为用途不符没列出来")
	}
	if !strings.Contains(to, "另有 1 个源用途不含发送") {
		t.Error("目标源栏应当说明有源因为用途不符没列出来")
	}
}

// 已经选上的源即使用途变了也照样列出来（带警告），
// 否则用户打开表单随手一存，规则里那个 ID 就被悄悄抹掉了。
func TestRuleFormPickerKeepsUnusableSelection(t *testing.T) {
	_, _, to := ruleFormWithSources(t, func(ids map[string]int64) *store.Rule {
		// 目标源指向一个只接收的源：改用途之前建的老规则就长这样。
		return &store.Rule{Enabled: true, ToSourceIDs: []int64{ids["只接收的源"]}}
	})

	id := pickerRowID(t, to, "to_source_ids", "只接收的源")
	if !strings.Contains(to, `name="to_source_ids" value="`+id+`" checked`) {
		t.Errorf("用途不合适的已选目标源也该回填成选中（源 id %s）", id)
	}
	if !strings.Contains(to, "用途不含发送") {
		t.Error("用途不合适的已选目标源应当带上警告")
	}
	if strings.Contains(to, "另有") {
		t.Error("已经列出来的源不该再算进「没列出来」的计数里")
	}
}

// pickerRowID 从一栏的 HTML 里取某个源那一行的勾选框 value（也就是源 id）。
func pickerRowID(t *testing.T, col, input, name string) string {
	t.Helper()
	start := strings.Index(col, rowOf(name))
	if start < 0 {
		t.Fatalf("%s 那一栏里没有 %s 这一行", input, name)
	}
	seg := col[start:]
	if end := strings.Index(seg, "</label>"); end > 0 {
		seg = seg[:end]
	}
	m := regexp.MustCompile(`name="` + input + `" value="(\d+)"`).FindStringSubmatch(seg)
	if m == nil {
		t.Fatalf("%s 那一行里没有 %s 的勾选框", name, input)
	}
	return m[1]
}

// 整栏不能套在一个大 <label> 里：按 HTML 规则，一个 label 只认它里面的第一个表单控件，
// 于是点栏目标题、点列表空白都会去勾第一项（改之前在 Chrome 里实测到过）。
func TestRuleFormHasNoNestedLabels(t *testing.T) {
	body, from, to := ruleFormWithSources(t, func(map[string]int64) *store.Rule {
		return &store.Rule{Enabled: true}
	})

	depth, max := 0, 0
	for _, m := range regexp.MustCompile(`<label\b|</label>`).FindAllString(body, -1) {
		if m == "</label>" {
			depth--
			continue
		}
		depth++
		if depth > max {
			max = depth
		}
	}
	if max > 1 {
		t.Errorf("规则表单里出现了嵌套的 label（最深 %d 层），点空白会误勾第一个控件", max)
	}

	// 每一行自己是个 label，且只包一个勾选框 —— 这样点整行才是「勾这一行」。
	for _, col := range []string{from, to} {
		rows := regexp.MustCompile(`(?s)<label class="pick".*?</label>`).FindAllString(col, -1)
		if len(rows) == 0 {
			t.Fatal("选源列表里没有找到可点的行")
		}
		for _, row := range rows {
			if n := strings.Count(row, "<input"); n != 1 {
				t.Errorf("每一行应当只包一个勾选框，实际 %d 个：%s", n, row)
			}
		}
	}
}

// 源卡片上的「curl 示例」：一键复制的按钮必须指名一个真的存在的元素 ——
// 选择器写错的话点下去是静默无效，谁都发现不了。顺带钉住「按钮在 summary 里」，
// 这样收起来的状态下也能直接复制整段。
func TestSourcesPageCurlCopyTargetsExist(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSettings(map[string]string{store.KeyBaseURL: "http://example.test:16000"}); err != nil {
		t.Fatal(err)
	}
	src := &store.Source{
		Name: "钩子", Kind: "webhook", Usage: "in", Enabled: true, Slug: "gh",
		Headers: "{}", HTTPMethod: "POST", AuthMode: "none",
	}
	if err := st.SaveSource(src); err != nil {
		t.Fatal(err)
	}

	s := testServer()
	s.store = st
	rec := httptest.NewRecorder()
	s.handleSourceList(rec, httptest.NewRequest("GET", "/sources", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("源列表渲染失败：%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	sels := regexp.MustCompile(`data-copy-from="#([^"]+)"`).FindAllStringSubmatch(body, -1)
	if len(sels) == 0 {
		t.Fatal("源卡片上没有 curl 示例的复制按钮")
	}
	for _, m := range sels {
		if !strings.Contains(body, `id="`+m[1]+`"`) {
			t.Errorf("复制按钮指向 #%s，但页面上没有这个 id", m[1])
		}
	}

	// 按钮要在 summary 里（收起来也能点），示例命令要在 pre.code 里。
	sum := regexp.MustCompile(`(?s)<summary>(.*?)</summary>`).FindStringSubmatch(body)
	if sum == nil || !strings.Contains(sum[1], "data-copy-from") {
		t.Error("复制按钮应当放在 summary 里，收起来时也能复制")
	}
	pre := regexp.MustCompile(`(?s)<pre class="code" id="curl-\d+">(.*?)</pre>`).FindStringSubmatch(body)
	if pre == nil {
		t.Fatal("curl 示例没有渲染成 pre.code")
	}
	if !strings.Contains(pre[1], "/hook/gh") || !strings.Contains(pre[1], "curl -X POST") {
		t.Errorf("curl 示例的内容不对：%s", pre[1])
	}

	// 回调地址那个复制按钮用的是 data-copy（老写法），别在改这段时弄丢。
	if !strings.Contains(body, "data-copy=") {
		t.Error("回调地址的复制按钮不见了")
	}
}

// ---------- Telegram 发送源 ----------

func tgBase() *store.Source {
	return &store.Source{
		Name: "TG", Kind: "telegram", Usage: "out", Enabled: true, Headers: "{}",
		TgToken: "123456:ABC", TgChatID: "-1001234", TgEndpoint: store.DefaultTgEndpoint,
	}
}

func TestValidateTelegramSource(t *testing.T) {
	if err := validateSourceShape(tgBase()); err != nil {
		t.Fatalf("基本配置应当合法: %v", err)
	}

	// 留空端点时自动补上官方地址（老配置或手填一半的情况）
	empty := tgBase()
	empty.TgEndpoint = ""
	if err := validateSourceShape(empty); err != nil {
		t.Fatalf("端点留空应当补默认值: %v", err)
	}
	if empty.TgEndpoint != store.DefaultTgEndpoint {
		t.Errorf("端点应补成 %q，实际 %q", store.DefaultTgEndpoint, empty.TgEndpoint)
	}

	// Telegram 只能发，不能收
	recv := tgBase()
	recv.Usage = "in"
	if err := validateSourceShape(recv); err == nil {
		t.Error("Telegram 用作接收源应当报错")
	}
	both := tgBase()
	both.Usage = "both"
	if err := validateSourceShape(both); err == nil {
		t.Error("Telegram 用作「接收 + 发送」应当报错")
	}

	noToken := tgBase()
	noToken.TgToken = ""
	if err := validateSourceShape(noToken); err == nil {
		t.Error("缺 Bot Token 应当报错")
	}
	noChat := tgBase()
	noChat.TgChatID = ""
	if err := validateSourceShape(noChat); err == nil {
		t.Error("缺 Chat ID 应当报错")
	}

	// 端点会直接拼上 token，所以必须以 /bot 结尾，否则只会换来一个看不懂的 404
	for _, bad := range []string{"https://api.telegram.org", "not-a-url", "ftp://x/bot", "https://api.telegram.org/bots"} {
		v := tgBase()
		v.TgEndpoint = bad
		if err := validateSourceShape(v); err == nil {
			t.Errorf("端点 %q 应当报错", bad)
		}
	}
	// 结尾多个斜杠是无害的
	trailing := tgBase()
	trailing.TgEndpoint = "https://api.telegram.org/bot/"
	if err := validateSourceShape(trailing); err != nil {
		t.Errorf("结尾多一个斜杠不该报错: %v", err)
	}

	// 话题 ID 要么不填，要么是数字 —— 别等投递时才发现
	badThread := tgBase()
	badThread.TgThreadID = "第一话题"
	if err := validateSourceShape(badThread); err == nil {
		t.Error("话题 ID 不是数字应当报错")
	}
	okThread := tgBase()
	okThread.TgThreadID = "42"
	if err := validateSourceShape(okThread); err != nil {
		t.Errorf("数字话题 ID 应当合法: %v", err)
	}
}

// Telegram 源不能接收：既不能出现在规则表单的接收源栏里，也不能是 webhook 那样带回调地址。
func TestTelegramSourceCannotReceive(t *testing.T) {
	src := &store.Source{Kind: "telegram", Usage: "both", Enabled: true}
	if src.CanReceive() {
		t.Error("Telegram 源永远不该被当成接收源")
	}
	if !src.CanSend() {
		t.Error("用途含发送时 Telegram 源应当能发送")
	}
	onlyIn := &store.Source{Kind: "telegram", Usage: "in"}
	if onlyIn.CanReceive() || onlyIn.CanSend() {
		t.Error("用途是接收的 Telegram 源两头都不该通")
	}
	// 回调地址只给 webhook 生成
	if got := hookURL("http://x", onlyIn); got != "" {
		t.Errorf("Telegram 源不该有回调地址，得到 %q", got)
	}
}

// 源表单里 Telegram 的参数要能填、能回填；「通过代理发送」只能有一个，
// 两个面板各放一份会导致 name 提交两次、id 重复（点 label 会点到另一个上）。
func TestSourceFormTelegramFieldsAndSingleProxySwitch(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := tgBase()
	src.TgThreadID = "42"
	src.TgEndpoint = "https://api.telegram.org/bot"
	src.UseProxy = true
	if err := st.SaveSource(src); err != nil {
		t.Fatal(err)
	}

	s := testServer()
	s.store = st
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/sources/"+strconv.FormatInt(src.ID, 10)+"/edit", nil)
	req.SetPathValue("id", strconv.FormatInt(src.ID, 10))
	s.handleSourceForm(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("源表单渲染失败：%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		`<option value="telegram" selected>`,
		`name="tg_token"`,
		`name="tg_chat_id"`,
		`name="tg_thread_id"`,
		`name="tg_endpoint"`,
		`data-when="telegram:send"`,
		`data-when="telegram:recv"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("源表单里缺少 %s", want)
		}
	}
	// 值要回填（不然改一次别的字段就把 token 弄丢了）
	for _, want := range []string{`value="123456:ABC"`, `value="-1001234"`, `value="42"`} {
		if !strings.Contains(body, want) {
			t.Errorf("表单没有回填 %s", want)
		}
	}

	if n := strings.Count(body, `name="use_proxy"`); n != 1 {
		t.Errorf("「通过代理发送」应当只有一个开关，实际 %d 个", n)
	}
	if n := strings.Count(body, `id="use_proxy"`); n != 1 {
		t.Errorf("use_proxy 的 id 应当只有一个（重复会让 label 点到另一个上），实际 %d 个", n)
	}
	if !strings.Contains(body, `data-when="webhook,telegram:send"`) {
		t.Error("代理开关所在的块应当对 Webhook 和 Telegram 都显示")
	}
}

// 代理开关对 Telegram 源也要生效（存得上、回填得出来），且用途不含发送时不认这个勾。
func TestTelegramSourceProxySwitchRoundTrip(t *testing.T) {
	s := testServer()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.store = st

	form := url.Values{
		"name": {"TG"}, "kind": {"telegram"}, "usage": {"out"}, "enabled": {"1"},
		"tg_token": {"1:x"}, "tg_chat_id": {"@c"}, "tg_thread_id": {""},
		"tg_endpoint": {"https://api.telegram.org/bot"}, "use_proxy": {"1"},
	}
	r := httptest.NewRequest("POST", "/sources", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	src, err := s.sourceFromForm(r, &store.Source{})
	if err != nil {
		t.Fatalf("Telegram 源表单应当解析成功: %v", err)
	}
	if !src.UseProxy {
		t.Error("勾了「通过代理发送」应当被认下来")
	}

	// 用途不含发送时这个勾不生效（表单上也只有发送时才显示），但库里原来那个值要留着
	stale := &store.Source{Kind: "telegram", Usage: "out", UseProxy: true}
	recv := url.Values{"name": {"TG"}, "kind": {"telegram"}, "usage": {"in"}, "tg_token": {"1:x"}, "tg_chat_id": {"@c"}}
	// 用途是接收时校验会拦下来，这里只关心 UseProxy 有没有被悄悄清掉
	r2 := httptest.NewRequest("POST", "/sources", strings.NewReader(recv.Encode()))
	r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r2.ParseForm(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.sourceFromForm(r2, stale)
	if !got.UseProxy {
		t.Error("用途不含发送时不该把库里留着的那一勾清掉")
	}
}

// Telegram 源不能接收：即使库里的用途被写成 both（只能来自手工改库或很老的数据），
// 也绝不能出现在规则表单的接收源栏里 —— 能选中它就意味着配了一条永远收不到消息的规则。
// 走界面建出来的 Telegram 源用途只能是 out，那时它本来就因为「用途不含接收」被藏掉，
// 所以下面那句说明文字在真实数据上都是对的。
func TestRuleFormHidesTelegramFromReceiveColumn(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hook := &store.Source{Name: "钩子", Kind: "webhook", Usage: "in", Enabled: true, Headers: "{}", Slug: "h1"}
	tg := &store.Source{Name: "TG 目标", Kind: "telegram", Usage: "both", Enabled: true, Headers: "{}",
		TgToken: "1:x", TgChatID: "1"}
	for _, src := range []*store.Source{hook, tg} {
		if err := st.SaveSource(src); err != nil {
			t.Fatal(err)
		}
	}

	s := testServer()
	s.store = st
	rec := httptest.NewRecorder()
	s.renderRuleForm(rec, httptest.NewRequest("GET", "/rules/new", nil), &store.Rule{Enabled: true}, "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("规则表单渲染失败：%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	fromStart := strings.Index(body, `data-picker="from"`)
	toStart := strings.Index(body, `data-picker="to"`)
	if fromStart < 0 || toStart < fromStart {
		t.Fatal("规则表单里没有找到两栏选源列表")
	}
	from, to := body[fromStart:toStart], body[toStart:]

	if strings.Contains(from, rowOf("TG 目标")) {
		t.Error("Telegram 源不该出现在接收源栏里")
	}
	if !strings.Contains(from, rowOf("钩子")) {
		t.Error("接收源栏应当列出 webhook 接收源")
	}
	if !strings.Contains(to, rowOf("TG 目标")) {
		t.Error("目标源栏应当列出 Telegram 源")
	}
	if !strings.Contains(from, "另有 1 个源用途不含接收") {
		t.Error("被藏起来的源要说明一声")
	}
}

// 「已保存 / 测试已发送」这类提示必须是不占位的浮层。
//
// 以前它是一条占页面流的横条：出现的那一刻整页高度会变，跨过「要不要纵向滚动条」
// 那条线时，经典滚动条会占走 15px，居中的内容随之整体横移（无头 Chrome 里实测
// 7.5px，列表页上就是搜索框和按钮「抖一下」）。这条测试钉住「浮层 + 会自动消失 +
// 不拦点击」，顺便钉住 html 的滚动条槽位不跟着内容高度变。
func TestFlashRendersAsNonBlockingToast(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.store = st

	// ok=tested 是「发送测试」之后跳回来的那种 URL
	rec := httptest.NewRecorder()
	s.handleSourceList(rec, httptest.NewRequest("GET", "/sources?ok=tested", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("源列表渲染失败：%d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	if !strings.Contains(body, `class="toast"`) {
		t.Error("提示没有渲染成浮层（.toast）")
	}
	if strings.Contains(body, `class="alert ok"`) {
		t.Error("提示又变回占位横条了：它会顶动页面，滚动条一出现整页就横移")
	}
	if !strings.Contains(body, `role="status"`) {
		t.Error("浮层要用 role=status，读屏才会念一遍（它会自己消失，错过就没了）")
	}
	if !strings.Contains(body, "data-toast-close") {
		t.Error("浮层缺关闭按钮")
	}

	app, err := ui.StaticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(app)
	for _, want := range []struct{ what, re string }{
		{"浮层要固定定位（占位就会顶动页面）", `(?s)\.toast\s*\{[^}]*position:\s*fixed`},
		{"浮层要在视口正中（横竖都居中，两个方向都得靠 transform 找回来）", `(?s)\.toast\s*\{[^}]*top:\s*50%[^}]*left:\s*50%[^}]*transform:\s*translate\(-50%,\s*-50%\)`},
		{"浮层不能吃点击（它可能正盖着页面头的按钮）", `(?s)\.toast\s*\{[^}]*pointer-events:\s*none`},
		{"关闭按钮要单独把点击收回来", `(?s)\.toast-close\s*\{[^}]*pointer-events:\s*auto`},
		{"浮层要自己淡出（没有 JS 时也得消失，不能一直糊在页面上）", `(?s)@keyframes toast-life.*?visibility:\s*hidden`},
		{"无 JS 时关闭按钮点了没反应，要藏掉", `html:not\(\.js\) \.toast-close\s*\{\s*display:\s*none`},
		// scrollbar-gutter: stable 对根元素不预留槽位（量过），只能靠 overflow-y: scroll
		{"滚动条槽位要一直占住，内容宽度才不跟着页面高度变", `(?s)html\s*\{[^}]*overflow-y:\s*scroll`},
		// 出错那版是同一个浮层，只是不淡出：错误得一直看得见，直到用户看完关掉
		{"出错浮层不能自动淡出", `(?s)\.toast\.err\s*\{[^}]*animation:\s*none`},
		{"出错浮层要用错误色，不能跟成功提示一个长相", `(?s)\.toast\.err\s*\{[^}]*background:\s*var\(--err-bg\)`},
		// 提交回来后先藏住、还原完再显示，否则会先画一帧顶部的画面（看着就是「刷新了一下」）
		{"还原滚动位置时要先把页面藏起来", `(?s)\.restoring body\s*\{\s*visibility:\s*hidden`},
	} {
		if !regexp.MustCompile(want.re).MatchString(css) {
			t.Errorf("app.css 里缺少这条规则：%s", want.what)
		}
	}

	// 「提交前记住位置、回到页面时还原」是两段内联脚本 + 一段 CSS，缺一段都不成立：
	// 少了 head 那段就没有标记，CSS 不会藏页面；少了 body 末尾那段页面会一直藏着。
	if n := strings.Count(body, "sessionStorage.getItem('f2a:scroll')"); n != 2 {
		t.Errorf("布局里读滚动位置的脚本应当有 head / body 末尾两处，实际 %d 处", n)
	}
	if !strings.Contains(body, "classList.add('restoring')") {
		t.Error("没有「带着滚动位置回来」的标记，CSS 就不知道该先藏住页面")
	}
	if !strings.Contains(body, "classList.remove('restoring')") {
		t.Error("还原完没有把标记摘掉，页面会一直藏着")
	}
}

// 页面级的反馈只有一种长相：视口正中的浮层（成功版自动淡出，出错版一直挂着）。
// 以前保存设置的提示是页面顶端一条 .alert.ok，加上提交后滚动位置归零，
// 看起来就是「刷新了页面再弹个框」。
func TestPageFeedbackIsAlwaysTheCenteredToast(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := testServer()
	s.store = st

	// 1) 设置页：?ok=saved 走公共 flash，换端口那条就地渲染的提示也得是浮层
	rec := httptest.NewRecorder()
	s.handleSettings(rec, httptest.NewRequest("GET", "/settings?ok=saved", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `class="toast"`) || !strings.Contains(body, "已保存") {
		t.Error("保存设置之后的提示没走浮层")
	}
	if strings.Contains(body, `class="alert`) {
		t.Error("设置页里还有占位的 .alert：它会顶动页面")
	}
	rec = httptest.NewRecorder()
	s.renderSettings(rec, httptest.NewRequest("GET", "/settings", nil), "",
		"设置已保存。监听端口正在从 1 切换到 2。")
	body = rec.Body.String()
	if !strings.Contains(body, `class="toast"`) || !strings.Contains(body, "正在从 1 切换到 2") {
		t.Error("换端口那条就地渲染的提示没走浮层")
	}

	// 注意 render 只在调用方没给 Flash 时才去认 ?ok=：
	// 上面那条分支如果把空字符串也塞进 Flash，保存成功就再也不会有提示了。
	rec = httptest.NewRecorder()
	s.handleSettings(rec, httptest.NewRequest("GET", "/settings?ok=imported", nil))
	if !strings.Contains(rec.Body.String(), "导入完成") {
		t.Error("设置页的 ?ok= 提示被空 Flash 顶掉了")
	}

	// 2) 校验失败：整页重渲染，错误同样浮在正中，而且不自动淡出
	form := url.Values{"name": {"电报"}, "kind": {"telegram"}, "usage": {"in"}, "enabled": {"1"}}
	req := httptest.NewRequest("POST", "/sources", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	s.handleSourceCreate(rec, req)
	body = rec.Body.String()
	if !strings.Contains(body, `class="toast err"`) {
		t.Error("保存失败的提示没有渲染成浮层（.toast err）")
	}
	if !strings.Contains(body, `role="alert"`) {
		t.Error("出错浮层要用 role=alert：它是打断式的信息，读屏得马上念")
	}
	if strings.Contains(body, `class="alert err"`) {
		t.Error("页面级的错误又变回占位横条了")
	}
	if !strings.Contains(body, "只能用作发送源") {
		t.Error("错误内容没带上")
	}

	// 3) 登录框那条报错留在卡片里：它贴着出错的表单，
	//    飘到视口正中反而会盖住用户名和密码。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/login", strings.NewReader("username=admin&password=wrong"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	s.handleLoginPost(rec, req)
	body = rec.Body.String()
	if !strings.Contains(body, `class="alert err"`) || !strings.Contains(body, "用户名或密码错误") {
		t.Error("登录失败应当在卡片里就地报错")
	}
	if strings.Contains(body, `class="toast`) {
		t.Error("登录框的报错不该飘到页面正中（那里正盖着用户名和密码）")
	}
}

// 列表筛选条那一行的几何必须在首帧就定死。真机上量过（无头 Chrome，1466×905）：
//   - 计数原本是空的、等 app.js 补字，补上「共 3 条」的那一刻搜索框从 427px 缩到 407px；
//   - 开始筛选时「清除筛选」长出来，再缩到 349.8px。
//
// 于是每次「发送测试」跳回来，用户都看见这一行横着抖一下。现在三件事各占一个固定位置：
// 计数由服务端渲染、计数盒子留足宽度、按钮用 visibility 藏（位置留着）。
func TestFilterBarGeometryIsStable(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, name := range []string{"一号", "二号", "三号"} {
		src := &store.Source{Name: name, Kind: "webhook", Usage: "out", Enabled: true, Headers: "{}"}
		if err := st.SaveSource(src); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, src.ID)
	}
	if err := st.SaveRule(&store.Rule{
		Name: "全部转发", Enabled: true, FromSourceIDs: []int64{ids[0]}, ToSourceIDs: ids[1:],
	}); err != nil {
		t.Fatal(err)
	}

	s := testServer()
	s.store = st
	for _, page := range []struct {
		name, path string
		handler    func(http.ResponseWriter, *http.Request)
		want       string
	}{
		{"源", "/sources", s.handleSourceList, "共 3 条"},
		{"规则", "/rules", s.handleRuleList, "共 1 条"},
	} {
		rec := httptest.NewRecorder()
		page.handler(rec, httptest.NewRequest("GET", page.path, nil))
		body := rec.Body.String()
		if !strings.Contains(body, `data-lf-count>`+page.want+`<`) {
			t.Errorf("%s列表的计数没有在服务端渲染出来（页面里得有 %q）：JS 事后再补会把搜索框挤窄",
				page.name, page.want)
		}
		if strings.Contains(body, `data-lf-reset hidden`) {
			t.Errorf("%s列表的「清除筛选」又用 hidden 藏了：hidden 是 display:none（Pico 还带 !important），"+
				"它一出现就会挤动搜索框，得用 class 把位置留住", page.name)
		}
		if !strings.Contains(body, `class="btn sm lf-off" data-lf-reset`) {
			t.Errorf("%s列表的「清除筛选」缺少 lf-off 初始状态", page.name)
		}
	}

	app, err := ui.StaticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ what, re string }{
		{"计数要留够宽度，文字在「共 N 条」和「显示 N / M 条」之间换时不重新分配空间",
			`(?s)\.filter-bar \.lf-count\s*\{[^}]*min-width:\s*10em`},
		{"没筛选时「清除筛选」要把位置占住（visibility 而不是 display）",
			`(?s)\.filter-bar \[data-lf-reset\]\.lf-off\s*\{\s*visibility:\s*hidden`},
	} {
		if !regexp.MustCompile(want.re).MatchString(string(app)) {
			t.Errorf("app.css 里缺少这条规则：%s", want.what)
		}
	}

	js, err := ui.StaticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "reset.classList.toggle('lf-off'") {
		t.Error("app.js 又用 hidden 切换「清除筛选」了，位置留不住")
	}
	// 提交前记住滚动位置：只有真要走的那次提交才记（被校验拦下的提交页面根本没动）。
	if !strings.Contains(string(js), "sessionStorage.setItem('f2a:scroll'") {
		t.Error("app.js 没有在提交前记住滚动位置")
	}
	if !strings.Contains(string(js), "if (e.defaultPrevented)") {
		t.Error("没跳过被拦下的提交：那种提交页面还在原地，记下的位置会坑到下一次")
	}
}
