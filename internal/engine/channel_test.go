package engine

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

// 固定时间戳，加签的期望值另用 python 的 hmac 算了一遍对答案：
//
//	secret = SECtest123, timestamp = 1758441600000(ms) / 1758441600(s)
const (
	signTestSecret = "SECtest123"
	signTestMs     = "1758441600000"
	signTestSec    = "1758441600"
	// python: quote(base64(hmac_sha256(b"SECtest123", b"1758441600000\nSECtest123")))
	wantDingtalkSign = "CYcks696Eq6VagJ67On9CyD8ErvqpqHNuYpojMyq%2BMc%3D"
	// python: base64(hmac_sha256(b"1758441600\nSECtest123", b""))
	wantFeishuSign = "4olLhfOpxa99VBCDUSjeT2Od9dfGe5QvnVAxiIN8s9o="
)

func signTestNow() time.Time { return time.Unix(1758441600, 0) }

// channelSink 记下渠道投递真正发出来的东西。
type channelSink struct {
	path string
	ct   string
	body map[string]any
	raw  string
}

func (s *channelSink) server(reply string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.path = r.URL.RequestURI()
		s.ct = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		s.raw = string(raw)
		s.body = map[string]any{}
		_ = json.Unmarshal(raw, &s.body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(reply))
	}))
}

func testEngine() *Engine {
	return New(nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func channelDelivery(text string) *store.Delivery {
	return &store.Delivery{Status: store.StatusPending, Rendered: text, Subject: "标题"}
}

// 每个渠道的请求体形状。这些就是各家文档要求的样子，抄错一个字段对方就会拒。
func TestBuildChannelRequestShapes(t *testing.T) {
	cases := []struct {
		name   string
		out    *store.Source
		checks map[string]string // 取点路径 -> 期望值
	}{
		{
			name: "钉钉",
			out:  &store.Source{Kind: "dingtalk", URL: "https://oapi.dingtalk.com/robot/send?access_token=t"},
			checks: map[string]string{
				"msgtype":        "markdown",
				"markdown.title": "标题",
				"markdown.text":  "正文",
			},
		},
		{
			name: "企业微信",
			out:  &store.Source{Kind: "wecom", URL: "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=k"},
			checks: map[string]string{
				"msgtype":          "markdown",
				"markdown.content": "正文",
			},
		},
		{
			name: "飞书",
			out:  &store.Source{Kind: "feishu", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/h"},
			checks: map[string]string{
				"msg_type":                     "interactive",
				"card.schema":                  "2.0",
				"card.header.title.content":    "标题",
				"card.header.title.tag":        "plain_text",
				"card.body.elements.0.tag":     "markdown",
				"card.body.elements.0.content": "正文",
			},
		},
		{
			name: "Bark",
			out:  &store.Source{Kind: "bark", URL: "https://api.day.app/key"},
			checks: map[string]string{
				"title":    "标题",
				"markdown": "正文",
			},
		},
		{
			name: "Bark 带分组",
			out:  &store.Source{Kind: "bark", URL: "https://api.day.app/key", ChannelTarget: "监控"},
			checks: map[string]string{
				"group": "监控",
			},
		},
		{
			name: "Server酱",
			out:  &store.Source{Kind: "serverchan", URL: "https://sctapi.ftqq.com/SCTkey.send"},
			checks: map[string]string{
				"title": "标题",
				"desp":  "正文",
			},
		},
		{
			name: "WxPusher",
			out: &store.Source{Kind: "wxpusher",
				URL: "https://wxpusher.zjiecode.com/api/send/message/SPT_abc123"},
			checks: map[string]string{
				"content":     "正文",
				"summary":     "标题",
				"contentType": "3",
				"spt":         "SPT_abc123",
			},
		},
		{
			name: "Gotify",
			out:  &store.Source{Kind: "gotify", URL: "https://gotify.example.com/message?token=t"},
			checks: map[string]string{
				"title":    "标题",
				"message":  "正文",
				"priority": "5",
			},
		},
		{
			name: "OneBot 群聊",
			out: &store.Source{Kind: "onebot",
				URL: "http://127.0.0.1:3000/send_group_msg", ChannelTarget: "123456"},
			checks: map[string]string{
				"group_id":            "123456",
				"message.0.type":      "text",
				"message.0.data.text": "标题\n正文\n",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, payload, err := buildChannelRequest(tc.out, "标题", "正文", signTestNow())
			if err != nil {
				t.Fatalf("构造请求失败：%v", err)
			}
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			for path, want := range tc.checks {
				if v := dig(t, got, path); v != want {
					t.Errorf("%s 应为 %q，实际 %q", path, want, v)
				}
			}
		})
	}
}

// dig 按 a.b.0.c 这样的路径取值，统一转成字符串比较。
func dig(t *testing.T, m map[string]any, path string) string {
	t.Helper()
	var cur any = m
	for _, seg := range strings.Split(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			cur = node[seg]
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil {
				t.Fatalf("路径 %s 里的下标 %q 不是数字", path, seg)
			}
			if idx < 0 || idx >= len(node) {
				t.Fatalf("路径 %s 越界", path)
			}
			cur = node[idx]
		default:
			t.Fatalf("路径 %s 在 %q 处断掉了", path, seg)
		}
	}
	switch v := cur.(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return trimFloat(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		b, _ := json.Marshal(v)
		return string(b)
	}
}

// 加签要和官方算法对得上（期望值是另用 python 的 hmac 算的）。
func TestChannelSignatures(t *testing.T) {
	if got := dingtalkSign(signTestMs, signTestSecret); got != wantDingtalkSign {
		t.Errorf("钉钉加签应为 %q，实际 %q", wantDingtalkSign, got)
	}
	if got := feishuSign(signTestSec, signTestSecret); got != wantFeishuSign {
		t.Errorf("飞书签名应为 %q，实际 %q", wantFeishuSign, got)
	}

	// 加签之后时间戳和签名都要落在地址上，且原来的查询串不能被吃掉。
	out := &store.Source{
		Kind: "dingtalk", URL: "https://oapi.dingtalk.com/robot/send?access_token=tok",
		ChannelSecret: signTestSecret,
	}
	endpoint, _, err := buildChannelRequest(out, "标题", "正文", signTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(endpoint, "access_token=tok&timestamp="+signTestMs+"&sign=") {
		t.Errorf("加签参数没拼对：%s", endpoint)
	}
	if !strings.Contains(endpoint, "sign="+wantDingtalkSign) {
		t.Errorf("签名值没拼对：%s", endpoint)
	}

	// 飞书的签名放在请求体里。
	fs := &store.Source{
		Kind: "feishu", URL: "https://open.feishu.cn/open-apis/bot/v2/hook/h",
		ChannelSecret: signTestSecret,
	}
	_, payload, err := buildChannelRequest(fs, "标题", "正文", signTestNow())
	if err != nil {
		t.Fatal(err)
	}
	if payload["timestamp"] != signTestSec {
		t.Errorf("飞书 timestamp 应是秒级字符串，实际 %#v", payload["timestamp"])
	}
	if payload["sign"] != wantFeishuSign {
		t.Errorf("飞书 sign 应为 %q，实际 %#v", wantFeishuSign, payload["sign"])
	}
}

// 各家的「HTTP 200 + 业务错误码」都要认出来，不然会把失败记成成功。
func TestChannelVerdict(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		code    int
		body    string
		wantErr string // 空表示应当判成功
	}{
		{"钉钉成功", "dingtalk", 200, `{"errcode":0,"errmsg":"ok"}`, ""},
		{"钉钉 msgtype 缺失", "dingtalk", 200, `{"errcode":300001,"errmsg":"msgtype is null"}`, "errcode 300001"},
		{"钉钉关键词不匹配", "dingtalk", 200, `{"errcode":310000,"errmsg":"keywords not in content"}`, "自定义关键词"},
		{"企业微信成功", "wecom", 200, `{"errcode":0,"errmsg":"ok"}`, ""},
		{"企业微信失败", "wecom", 200, `{"errcode":93000,"errmsg":"invalid webhook url"}`, "errcode 93000"},
		{"飞书成功", "feishu", 200, `{"code":0,"msg":"success"}`, ""},
		{"飞书失败", "feishu", 200, `{"code":19021,"msg":"sign match fail"}`, "code 19021"},
		{"Bark 成功", "bark", 200, `{"code":200,"message":"success"}`, ""},
		{"Bark 失败", "bark", 200, `{"code":400,"message":"bad key"}`, "code 400"},
		{"Server酱成功", "serverchan", 200, `{"code":0,"message":""}`, ""},
		{"Server酱失败", "serverchan", 200, `{"code":40001,"message":"bad pushtoken"}`, "code 40001"},
		{"WxPusher 成功", "wxpusher", 200, `{"code":1000,"msg":"处理成功"}`, ""},
		{"WxPusher 失败", "wxpusher", 200, `{"code":1002,"msg":"SPT 不存在"}`, "code 1002"},
		{"Gotify 成功", "gotify", 200, `{"id":1,"appid":1,"message":"正文"}`, ""},
		{"Gotify 失败", "gotify", 401, `{"error":"Unauthorized","errorCode":401}`, "HTTP 401"},
		{"OneBot 成功", "onebot", 200, `{"status":"ok","retcode":0,"data":{"message_id":7}}`, ""},
		{"OneBot 失败", "onebot", 200, `{"status":"failed","retcode":100,"wording":"群不存在"}`, "群不存在"},
		{"OneBot retcode 非 0", "onebot", 200, `{"status":"ok","retcode":100,"wording":"群不存在"}`, "retcode 100"},
		{"非 JSON 响应", "dingtalk", 200, `<html>502</html>`, "不是 JSON"},
		{"响应里没有错误码", "dingtalk", 200, `{"hello":"world"}`, "没有 errcode"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := channelVerdict(tc.kind, tc.code, []byte(tc.body))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应当判成功，实际报错：%v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应当报错（含 %q），实际判成功", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("报错信息应含 %q，实际 %q", tc.wantErr, err.Error())
			}
		})
	}
}

// 私聊还是群聊：目标 ID 写前缀最明确，只写数字时看地址里的接口名，看不出来就不猜。
func TestOneBotTarget(t *testing.T) {
	cases := []struct {
		endpoint string
		target   string
		wantKey  string
		wantVal  any
		wantErr  bool
	}{
		{"http://h/send_group_msg", "123456", "group_id", int64(123456), false},
		{"http://h/send_private_msg", "123456", "user_id", int64(123456), false},
		{"http://h/send_msg", "group:999", "group_id", int64(999), false},
		{"http://h/send_msg", "user:999", "user_id", int64(999), false},
		{"http://h/send_msg", "123456", "", nil, true},
		{"http://h/send_group_msg", "", "", nil, true},
	}
	for _, tc := range cases {
		key, val, err := oneBotTarget(tc.endpoint, tc.target)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s + %q 应当报错", tc.endpoint, tc.target)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s + %q 不该报错：%v", tc.endpoint, tc.target, err)
			continue
		}
		if key != tc.wantKey || val != tc.wantVal {
			t.Errorf("%s + %q 应为 %s=%v，实际 %s=%v", tc.endpoint, tc.target,
				tc.wantKey, tc.wantVal, key, val)
		}
	}
}

func TestWxPusherSPT(t *testing.T) {
	cases := map[string]string{
		"https://wxpusher.zjiecode.com/api/send/message/SPT_abc":  "SPT_abc",
		"https://wxpusher.zjiecode.com/api/send/message/SPT_abc/": "SPT_abc",
		"https://wxpusher.zjiecode.com/api/send/message":          "",
		"https://example.com/hook/xyz":                            "",
		"不是地址":                                                    "",
	}
	for raw, want := range cases {
		if got := wxPusherSPT(raw); got != want {
			t.Errorf("%s 应取出 %q，实际 %q", raw, want, got)
		}
	}
}

// 渠道地址里就带着凭据，报错信息里必须洗掉。
func TestRedactChannelSecrets(t *testing.T) {
	out := &store.Source{
		Kind: "dingtalk", URL: "https://oapi.dingtalk.com/robot/send?access_token=SECRET",
		ChannelSecret: signTestSecret,
	}
	err := errors.New("Post \"https://oapi.dingtalk.com/robot/send?access_token=SECRET\": dial tcp: timeout (secret=" + signTestSecret + ")")
	got := redactChannelSecrets(err, out).Error()
	if strings.Contains(got, "SECRET") || strings.Contains(got, signTestSecret) {
		t.Fatalf("错误信息里还留着凭据：%s", got)
	}
	if !strings.Contains(got, "oapi.dingtalk.com") {
		t.Errorf("主机名应当留着，方便排查：%s", got)
	}
}

// 走一遍完整的 sendChannel：报文发出去是对的，业务错误码也要判成失败。
func TestSendChannelEndToEnd(t *testing.T) {
	sink := &channelSink{}
	srv := sink.server(`{"errcode":0,"errmsg":"ok"}`)
	defer srv.Close()

	eng := testEngine()
	out := &store.Source{Name: "钉钉", Kind: "dingtalk", Usage: "out", Enabled: true, URL: srv.URL + "/robot/send?access_token=t"}
	code, body, err := eng.sendChannel(channelDelivery("正文"), out, eng.client)
	if err != nil {
		t.Fatalf("发送失败：%v", err)
	}
	if code != http.StatusOK {
		t.Errorf("响应码应为 200，实际 %d", code)
	}
	if !strings.Contains(body, "errcode") {
		t.Errorf("响应体要记下来，实际 %q", body)
	}
	if sink.ct != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", sink.ct)
	}
	if got, _ := sink.body["msgtype"].(string); got != "markdown" {
		t.Errorf("msgtype 应为 markdown，实际 %#v", sink.body["msgtype"])
	}
	md, _ := sink.body["markdown"].(map[string]any)
	if md == nil || md["text"] != "正文" || md["title"] != "标题" {
		t.Errorf("markdown 内容不对：%#v", sink.body["markdown"])
	}

	// 同一条地址，对方换成业务错误码：这次必须判失败，并把原因带出来。
	srv2 := (&channelSink{}).server(`{"errcode":300001,"errmsg":"msgtype is null"}`)
	defer srv2.Close()
	bad := &store.Source{Kind: "dingtalk", Usage: "out", Enabled: true, URL: srv2.URL}
	_, _, err = eng.sendChannel(channelDelivery("正文"), bad, eng.client)
	if err == nil {
		t.Fatal("业务错误码必须判成失败，不然会显示绿灯然后丢消息")
	}
	if !strings.Contains(err.Error(), "300001") {
		t.Errorf("失败原因要带上对方的错误码，实际 %q", err)
	}
}

// 没配地址 / 内容为空这类情况要在本地就拦下来，别去打扰对方。
func TestSendChannelMissingConfig(t *testing.T) {
	eng := testEngine()
	cases := []struct {
		name string
		out  *store.Source
		d    *store.Delivery
	}{
		{"没地址", &store.Source{Kind: "dingtalk"}, channelDelivery("正文")},
		{"没内容", &store.Source{Kind: "dingtalk", URL: "https://x/y"}, &store.Delivery{}},
		{"WxPusher 地址里没有 SPT", &store.Source{Kind: "wxpusher", URL: "https://wxpusher.zjiecode.com/api/send/message"}, channelDelivery("正文")},
		{"OneBot 没填目标", &store.Source{Kind: "onebot", URL: "https://x/send_group_msg"}, channelDelivery("正文")},
	}
	for _, tc := range cases {
		if _, _, err := eng.sendChannel(tc.d, tc.out, eng.client); err == nil {
			t.Errorf("%s：应当报错", tc.name)
		}
	}
}
