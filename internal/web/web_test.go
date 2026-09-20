package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"testing"

	"github.com/loarland/Webhook2Any/internal/store"
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
		ok.Header.Set("X-W2A-Token", "s3cret")
		if !verifyInbound(src, ok, body) {
			t.Error("正确密钥应当通过")
		}

		bad := httptest.NewRequest("POST", "/hook/x", nil)
		bad.Header.Set("X-W2A-Token", "wrong")
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
