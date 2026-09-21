package engine

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/loarland/Forward2Any/internal/store"
)

// tgSink 假装自己是 Bot API：记下收到的路径、Content-Type 和 body，再按需要回一个信封。
type tgSink struct {
	path string
	ct   string
	body map[string]any
	raw  string
	hits atomic.Int32
}

func (s *tgSink) server(reply string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		s.path = r.URL.Path
		s.ct = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		s.raw = string(raw)
		s.body = map[string]any{}
		_ = json.Unmarshal(raw, &s.body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
}

func tgDelivery(text string) *store.Delivery {
	return &store.Delivery{Status: store.StatusPending, Rendered: text}
}

func TestSendTelegramRequestShape(t *testing.T) {
	sink := &tgSink{}
	srv := sink.server(`{"ok":true,"result":{"message_id":7}}`)
	defer srv.Close()

	eng := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	out := &store.Source{
		Name: "tg", Kind: "telegram", Usage: "out", Enabled: true,
		TgToken: "123456:ABC-DEF", TgChatID: "-1001234567890", TgThreadID: "42",
		TgEndpoint: srv.URL + "/bot/", // 结尾多一个斜杠也不该拼出双斜杠
	}

	code, body, err := eng.sendTelegram(tgDelivery(`{"action":"push"}`), out, eng.client)
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	if code != http.StatusOK {
		t.Errorf("应当记录响应码 200，实际 %d", code)
	}
	if !strings.Contains(body, `"message_id":7`) {
		t.Errorf("应当把响应体记下来，实际 %q", body)
	}

	// token 拼在路径里，端点结尾的斜杠要去掉
	if want := "/bot123456:ABC-DEF/sendMessage"; sink.path != want {
		t.Errorf("请求路径应为 %q，实际 %q", want, sink.path)
	}
	if sink.ct != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", sink.ct)
	}
	if got, ok := sink.body["chat_id"].(float64); !ok || got != -1001234567890 {
		t.Errorf("chat_id 应当以 JSON 数字发出，实际 %#v", sink.body["chat_id"])
	}
	if got, ok := sink.body["message_thread_id"].(float64); !ok || got != 42 {
		t.Errorf("message_thread_id 应当以 JSON 数字发出，实际 %#v", sink.body["message_thread_id"])
	}
	if got, _ := sink.body["text"].(string); got != `{"action":"push"}` {
		t.Errorf("正文应当原样发出去，实际 %#v", sink.body["text"])
	}
}

func TestSendTelegramChatIDAndThreadOptional(t *testing.T) {
	sink := &tgSink{}
	srv := sink.server(`{"ok":true,"result":{}}`)
	defer srv.Close()

	eng := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// @channelusername 不是数字，得原样当字符串发；话题 ID 没填就不该出现这个字段
	out := &store.Source{Kind: "telegram", Usage: "out", Enabled: true,
		TgToken: "1:x", TgChatID: "@my_channel", TgEndpoint: srv.URL + "/bot"}

	if _, _, err := eng.sendTelegram(tgDelivery("hi"), out, eng.client); err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	if got, ok := sink.body["chat_id"].(string); !ok || got != "@my_channel" {
		t.Errorf("chat_id 应当原样作为字符串发出，实际 %#v", sink.body["chat_id"])
	}
	if _, exists := sink.body["message_thread_id"]; exists {
		t.Error("没填话题 ID 时不该带上 message_thread_id 字段")
	}
	if sink.path != "/bot1:x/sendMessage" {
		t.Errorf("请求路径不对：%q", sink.path)
	}
}

// Bot API 用 HTTP 200 + {"ok":false} 报错，所以判定成败只能看 ok，不能看状态码。
func TestSendTelegramTreatsOkFalseAsFailure(t *testing.T) {
	sink := &tgSink{}
	srv := sink.server(`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	defer srv.Close()

	eng := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	out := &store.Source{Kind: "telegram", Usage: "out", Enabled: true,
		TgToken: "1:x", TgChatID: "123", TgEndpoint: srv.URL + "/bot"}

	code, body, err := eng.sendTelegram(tgDelivery("hi"), out, eng.client)
	if err == nil {
		t.Fatal("ok=false 必须算失败")
	}
	if !strings.Contains(err.Error(), "chat not found") || !strings.Contains(err.Error(), "400") {
		t.Errorf("错误里应当带上 Telegram 给的原因和 error_code，实际 %q", err)
	}
	if code != http.StatusOK || !strings.Contains(body, "chat not found") {
		t.Errorf("响应码和响应体仍要记下来，实际 %d / %q", code, body)
	}
}

// 不是 JSON（走错端点、被中间设备拦下）时不能只报个「语法错误」，
// 得把拿到的原文带出来，否则没法查。
func TestSendTelegramReportsNonJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html>404 Not Found</html>"))
	}))
	defer srv.Close()

	eng := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	out := &store.Source{Kind: "telegram", Usage: "out", Enabled: true,
		TgToken: "1:x", TgChatID: "123", TgEndpoint: srv.URL + "/bot"}

	_, _, err := eng.sendTelegram(tgDelivery("hi"), out, eng.client)
	if err == nil || !strings.Contains(err.Error(), "404 Not Found") {
		t.Errorf("错误里应当带上响应原文，实际 %v", err)
	}
}

// Bot API 把 token 放在 URL 路径里，而 net/http 的报错会带上完整 URL ——
// 这条错误要落进投递记录并显示在网页上，所以必须先把 token 抹掉。
func TestSendTelegramRedactsTokenFromErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立刻关掉：连接必然失败，报错里带着 URL

	eng := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	out := &store.Source{Kind: "telegram", Usage: "out", Enabled: true,
		TgToken: "123456:SECRET-PART", TgChatID: "1", TgEndpoint: srv.URL + "/bot"}

	_, _, err := eng.sendTelegram(tgDelivery("hi"), out, eng.client)
	if err == nil {
		t.Fatal("连不上时应当报错")
	}
	t.Logf("原始错误：%v", err)
	if strings.Contains(err.Error(), "SECRET-PART") {
		t.Errorf("错误信息里泄露了 Bot Token：%v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("应当把 token 抹成 ***，实际 %v", err)
	}
}

// 超长消息自己先拦下来：不截断（悄悄丢半条更难查），也不白跑一趟网络。
func TestSendTelegramRejectsTooLongText(t *testing.T) {
	sink := &tgSink{}
	srv := sink.server(`{"ok":true,"result":{}}`)
	defer srv.Close()

	eng := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	out := &store.Source{Kind: "telegram", Usage: "out", Enabled: true,
		TgToken: "1:x", TgChatID: "1", TgEndpoint: srv.URL + "/bot"}

	// 4096 个汉字：字节数远超上限，但按 Telegram 的口径刚好压线，应当放行
	ok := strings.Repeat("中", tgTextLimit)
	if _, _, err := eng.sendTelegram(tgDelivery(ok), out, eng.client); err != nil {
		t.Fatalf("%d 个汉字应当放行：%v", tgTextLimit, err)
	}

	tooLong := strings.Repeat("中", tgTextLimit+1)
	_, _, err := eng.sendTelegram(tgDelivery(tooLong), out, eng.client)
	if err == nil || !strings.Contains(err.Error(), "4096") {
		t.Errorf("超长应当报错并说明上限，实际 %v", err)
	}
	if got := sink.hits.Load(); got != 1 {
		t.Errorf("超长时不该发出请求，实际发了 %d 次", got)
	}

	// 带 emoji 时必须按 UTF-16 码元算：一个 emoji 占 2 个码元
	emoji := strings.Repeat("😀", tgTextLimit/2+1)
	if _, _, err := eng.sendTelegram(tgDelivery(emoji), out, eng.client); err == nil {
		t.Error("emoji 按码元算超长，应当报错")
	}
}

func TestSendTelegramMissingConfig(t *testing.T) {
	eng := New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	cases := []struct {
		name string
		src  store.Source
		text string
	}{
		{"没填 token", store.Source{Kind: "telegram", TgChatID: "1"}, "hi"},
		{"没填 chat id", store.Source{Kind: "telegram", TgToken: "1:x"}, "hi"},
		{"正文为空", store.Source{Kind: "telegram", TgToken: "1:x", TgChatID: "1"}, "  "},
		{"话题 ID 不是数字", store.Source{Kind: "telegram", TgToken: "1:x", TgChatID: "1", TgThreadID: "abc"}, "hi"},
	}
	for _, c := range cases {
		src := c.src
		if _, _, err := eng.sendTelegram(tgDelivery(c.text), &src, eng.client); err == nil {
			t.Errorf("%s 时应当报错", c.name)
		}
	}
}

// Telegram 源只有勾了「通过代理发送」才走代理，跟 Webhook 同一套开关。
func TestProxyUsedByTelegramSources(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var sink tgSink
	srv := sink.server(`{"ok":true,"result":{}}`)
	defer srv.Close()

	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host == "" {
			http.Error(w, "不是代理请求", http.StatusBadRequest)
			return
		}
		proxyHits.Add(1)
		r.RequestURI = ""
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer proxy.Close()

	if err := st.SetSettings(map[string]string{
		store.KeyProxyType: "http",
		store.KeyProxyAddr: strings.TrimPrefix(proxy.URL, "http://"),
	}); err != nil {
		t.Fatal(err)
	}

	mk := func(name string, proxy bool) *store.Source {
		src := &store.Source{Name: name, Kind: "telegram", Usage: "out", Enabled: true,
			TgToken: "1:x", TgChatID: "1", TgEndpoint: srv.URL + "/bot", UseProxy: proxy}
		if err := st.SaveSource(src); err != nil {
			t.Fatal(err)
		}
		return src
	}
	eng := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))

	deliver := func(src *store.Source) {
		d := &store.Delivery{OutSourceID: src.ID, Status: store.StatusPending, Rendered: "hi"}
		if err := st.CreateDelivery(d); err != nil {
			t.Fatal(err)
		}
		eng.attempt(d)
		if d.Status != store.StatusSuccess {
			t.Fatalf("源 %q 投递失败：%s", src.Name, d.LastError)
		}
	}

	deliver(mk("勾了代理", true))
	if got := proxyHits.Load(); got != 1 {
		t.Fatalf("勾了代理的 Telegram 源应当走代理，代理收到 %d 次请求", got)
	}
	deliver(mk("没勾代理", false))
	if got := proxyHits.Load(); got != 1 {
		t.Errorf("没勾代理的 Telegram 源不该走代理，代理收到 %d 次请求", got)
	}
}
