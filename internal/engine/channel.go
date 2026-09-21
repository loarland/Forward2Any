package engine

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

// 内置渠道的报文格式。
//
// 这些渠道都叫「webhook」，但没有两个的报文是一样的：
//
//	钉钉      {"msgtype":"markdown","markdown":{"title":..,"text":..}}
//	企业微信  {"msgtype":"markdown","markdown":{"content":..}}
//	飞书      {"msg_type":"interactive","card":{...}}
//	Bark      {"title":..,"markdown":..}
//	Server酱   {"title":..,"desp":..}
//	WxPusher  {"content":..,"summary":..,"contentType":3,"spt":"SPT_.."}（打到 simple-push）
//	Gotify    {"title":..,"message":..,"priority":5}
//	OneBot    {"user_id|group_id":..,"message":[{"type":"text","data":{"text":..}}]}
//
// 出错方式也不一样：钉钉 / 企业微信 / 飞书 / Server酱 / WxPusher / Bark / OneBot
// 都是 HTTP 200 + 报文体里的业务错误码。只看状态码会把「被拒绝」记成「投递成功」，
// 所以每个渠道都要解析响应体（见 channelVerdict）。
//
// 报文形状参照 huilang-me/CF-Server-Monitor 的 src/services/notification.js，
// 加签算法按各家官方文档实现。

// sendChannel 按渠道格式把这条投递发出去。
func (e *Engine) sendChannel(d *store.Delivery, out *store.Source, client *http.Client) (int, string, error) {
	text := strings.TrimSpace(d.Rendered)
	if text == "" {
		text = strings.TrimSpace(d.Payload)
	}
	if text == "" {
		return 0, "", errors.New("消息内容为空")
	}
	title := strings.TrimSpace(d.Subject)
	if title == "" {
		title = "Forward2Any"
	}

	endpoint, payload, err := buildChannelRequest(out, title, text, time.Now())
	if err != nil {
		return 0, "", redactChannelSecrets(err, out)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", fmt.Errorf("构造请求体失败: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, "", redactChannelSecrets(fmt.Errorf("构造请求失败: %w", err), out)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", redactChannelSecrets(err, out)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if verr := channelVerdict(out.Kind, resp.StatusCode, respBody); verr != nil {
		return resp.StatusCode, string(respBody), verr
	}
	return resp.StatusCode, string(respBody), nil
}

// buildChannelRequest 拼出某个渠道要 POST 的地址与请求体。
func buildChannelRequest(out *store.Source, title, text string, now time.Time) (string, map[string]any, error) {
	endpoint := strings.TrimSpace(out.URL)
	if endpoint == "" {
		return "", nil, errors.New("未配置渠道地址")
	}
	secret := strings.TrimSpace(out.ChannelSecret)
	target := strings.TrimSpace(out.ChannelTarget)

	switch out.Kind {
	case "dingtalk":
		if secret != "" {
			ts := strconv.FormatInt(now.UnixMilli(), 10)
			endpoint = appendQuery(endpoint, "timestamp", ts, "sign", dingtalkSign(ts, secret))
		}
		return endpoint, map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"title": title, "text": text},
		}, nil

	case "wecom":
		return endpoint, map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"content": text},
		}, nil

	case "feishu":
		payload := map[string]any{
			"msg_type": "interactive",
			"card": map[string]any{
				"schema": "2.0",
				"header": map[string]any{
					"template": "blue",
					"title":    map[string]any{"content": title, "tag": "plain_text"},
				},
				"body": map[string]any{
					"elements": []any{map[string]any{"tag": "markdown", "content": text}},
				},
			},
		}
		if secret != "" {
			ts := strconv.FormatInt(now.Unix(), 10)
			payload["timestamp"] = ts
			payload["sign"] = feishuSign(ts, secret)
		}
		return endpoint, payload, nil

	case "bark":
		payload := map[string]any{"title": title, "markdown": text}
		// 填了就当作 Bark 的分组名，用来在 App 里分开归档。
		if target != "" {
			payload["group"] = target
		}
		return endpoint, payload, nil

	case "serverchan":
		return endpoint, map[string]any{"title": title, "desp": text}, nil

	case "wxpusher":
		spt := wxPusherSPT(endpoint)
		if spt == "" {
			return "", nil, errors.New("WxPusher 地址里找不到 SPT，应形如 " +
				"https://wxpusher.zjiecode.com/api/send/message/SPT_xxxxxx")
		}
		return wxPusherEndpoint, map[string]any{
			"content":     text,
			"summary":     title,
			"contentType": 3,
			"spt":         spt,
		}, nil

	case "gotify":
		return endpoint, map[string]any{
			"title":    title,
			"message":  text,
			"priority": 5,
			"extras": map[string]any{
				"client::display": map[string]any{"contentType": "text/markdown"},
			},
		}, nil

	case "onebot":
		field, id, err := oneBotTarget(endpoint, target)
		if err != nil {
			return "", nil, err
		}
		return endpoint, map[string]any{
			field: id,
			"message": []any{
				map[string]any{
					"type": "text",
					"data": map[string]any{"text": title + "\n" + text + "\n"},
				},
			},
		}, nil
	}
	return "", nil, fmt.Errorf("未知的渠道类型 %q", out.Kind)
}

// wxPusherEndpoint 是 WxPusher 的极简推送接口：SPT 从用户填的地址里抽出来放到请求体。
const wxPusherEndpoint = "https://wxpusher.zjiecode.com/api/send/message/simple-push"

// wxPusherSPT 从 https://wxpusher.zjiecode.com/api/send/message/SPT_xxx 里取出 SPT_xxx。
func wxPusherSPT(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	seg := strings.Trim(u.Path, "/")
	if i := strings.LastIndex(seg, "/"); i >= 0 {
		seg = seg[i+1:]
	}
	if !strings.HasPrefix(seg, "SPT_") {
		return ""
	}
	return seg
}

// oneBotTarget 决定这次发给谁：私聊还是群。
//
// 目标 ID 可以写成 group:123456 / user:123456 明确指定；只写数字时按地址里的
// send_group_msg / send_private_msg 判断，两个都判断不出来就让人写清楚 ——
// 猜错的后果是消息发到错误的会话里，宁可不发。
func oneBotTarget(endpoint, target string) (string, any, error) {
	if target == "" {
		return "", nil, errors.New("OneBot 要填目标 ID：私聊写 QQ 号，群聊写群号")
	}
	kind := ""
	switch {
	case strings.HasPrefix(target, "group:"):
		kind, target = "group_id", strings.TrimPrefix(target, "group:")
	case strings.HasPrefix(target, "user:"), strings.HasPrefix(target, "private:"):
		kind = "user_id"
		target = strings.TrimPrefix(strings.TrimPrefix(target, "user:"), "private:")
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return "", nil, errors.New("OneBot 的目标 ID 是空的")
	}

	if kind == "" {
		switch {
		case strings.Contains(endpoint, "send_group_msg"):
			kind = "group_id"
		case strings.Contains(endpoint, "send_private_msg"):
			kind = "user_id"
		default:
			return "", nil, errors.New("看不出这条 OneBot 地址是私聊还是群聊：" +
				"地址里用 send_private_msg 或 send_group_msg，或者把目标写成 user:123 / group:123")
		}
	}
	if n, err := strconv.ParseInt(target, 10, 64); err == nil {
		return kind, n, nil
	}
	return kind, target, nil
}

// dingtalkSign 是钉钉自定义机器人的加签：
// sign = urlEncode(base64(HMAC-SHA256(key=secret, data=timestamp+"\n"+secret)))，时间戳是毫秒。
func dingtalkSign(timestamp, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + secret))
	return url.QueryEscape(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// feishuSign 是飞书自定义机器人的签名：key 是 timestamp+"\n"+secret，消息体为空。时间戳是秒。
func feishuSign(timestamp, secret string) string {
	stringToSign := timestamp + "\n" + secret
	mac := hmac.New(sha256.New, []byte(stringToSign))
	mac.Write(nil)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// appendQuery 往地址后面追加查询参数（已有 ? 时用 & 接上）。
func appendQuery(raw string, kv ...string) string {
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	var sb strings.Builder
	sb.WriteString(raw)
	for i := 0; i+1 < len(kv); i += 2 {
		sb.WriteString(sep)
		sb.WriteString(url.QueryEscape(kv[i]))
		sb.WriteString("=")
		sb.WriteString(kv[i+1])
		sep = "&"
	}
	return sb.String()
}

// channelVerdict 判断这一次到底成没成。
//
// 这些渠道普遍是「HTTP 200 + 报文体里一个业务错误码」，只看状态码会把拒绝记成成功，
// 白白把消息丢掉还显示绿灯。
func channelVerdict(kind string, code int, respBody []byte) error {
	if code < 200 || code > 299 {
		return fmt.Errorf("目标返回 HTTP %d：%s", code, Truncate(strings.TrimSpace(string(respBody)), 200))
	}

	var m map[string]any
	if err := json.Unmarshal(respBody, &m); err != nil {
		return fmt.Errorf("目标的响应不是 JSON（HTTP %d）：%s", code, Truncate(strings.TrimSpace(string(respBody)), 200))
	}

	codeOf := func(keys ...string) (float64, bool) {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if f, ok := v.(float64); ok {
					return f, true
				}
			}
		}
		return 0, false
	}
	textOf := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k]; ok {
				if s := strings.TrimSpace(fmt.Sprint(v)); s != "" && s != "<nil>" {
					return s
				}
			}
		}
		return ""
	}

	switch kind {
	case "dingtalk", "wecom":
		if n, ok := codeOf("errcode"); ok && n != 0 {
			msg := textOf("errmsg")
			if kind == "dingtalk" && n == 310000 {
				msg += "（钉钉机器人的安全设置要求消息里带自定义关键词，或者加签没对上）"
			}
			return fmt.Errorf("被拒绝（errcode %s）：%s", trimFloat(n), msg)
		}
		if _, ok := m["errcode"]; !ok {
			return errors.New("响应里没有 errcode，无法确认是否成功：" + Truncate(string(respBody), 200))
		}
		return nil

	case "feishu":
		n, ok := codeOf("code", "StatusCode")
		if !ok {
			return errors.New("响应里没有 code，无法确认是否成功：" + Truncate(string(respBody), 200))
		}
		if n != 0 {
			return fmt.Errorf("被拒绝（code %s）：%s", trimFloat(n), textOf("msg", "StatusMessage"))
		}
		return nil

	case "bark":
		n, ok := codeOf("code")
		if !ok {
			return errors.New("响应里没有 code，无法确认是否成功：" + Truncate(string(respBody), 200))
		}
		if n != 200 {
			return fmt.Errorf("被拒绝（code %s）：%s", trimFloat(n), textOf("message"))
		}
		return nil

	case "serverchan":
		n, ok := codeOf("code")
		if !ok {
			return errors.New("响应里没有 code，无法确认是否成功：" + Truncate(string(respBody), 200))
		}
		if n != 0 {
			// Server酱把失败原因放在 message 或 data.error 里。
			msg := textOf("message")
			if msg == "" || msg == "SUCCESS" {
				if data, ok := m["data"].(map[string]any); ok {
					msg = strings.TrimSpace(fmt.Sprint(data["error"]))
				}
			}
			if msg == "<nil>" {
				msg = ""
			}
			return fmt.Errorf("被拒绝（code %s）：%s", trimFloat(n), msg)
		}
		return nil

	case "wxpusher":
		n, ok := codeOf("code")
		if !ok {
			return errors.New("响应里没有 code，无法确认是否成功：" + Truncate(string(respBody), 200))
		}
		// 1000 = 处理成功；0 是少数接口的成功写法，一并认。
		if n != 1000 && n != 0 {
			return fmt.Errorf("被拒绝（code %s）：%s", trimFloat(n), textOf("msg"))
		}
		return nil

	case "gotify":
		if msg := textOf("error"); msg != "" {
			return fmt.Errorf("被拒绝：%s", msg)
		}
		if _, ok := m["id"]; !ok {
			return errors.New("响应里没有 id，无法确认是否成功：" + Truncate(string(respBody), 200))
		}
		return nil

	case "onebot":
		if status, _ := m["status"].(string); status != "" && status != "ok" {
			return fmt.Errorf("被拒绝（status %s）：%s", status, textOf("wording", "msg"))
		}
		n, ok := codeOf("retcode")
		if !ok {
			return errors.New("响应里没有 retcode，无法确认是否成功：" + Truncate(string(respBody), 200))
		}
		if n != 0 {
			return fmt.Errorf("被拒绝（retcode %s）：%s", trimFloat(n), textOf("wording", "msg"))
		}
		return nil
	}
	return nil
}

func trimFloat(f float64) string {
	if f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// redactChannelSecrets 把错误信息里的渠道地址和加签密钥抹掉。
//
// 渠道的凭据就藏在地址里（?access_token=、/hook/xxx、SPT_xxx），而 http.Client
// 的报错会把完整 URL 带上，这条错误会写进投递记录并显示在网页上。
func redactChannelSecrets(err error, out *store.Source) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if raw := strings.TrimSpace(out.URL); raw != "" {
		if masked := maskedURL(raw); masked != "" {
			msg = strings.ReplaceAll(msg, raw, masked)
		}
	}
	if secret := strings.TrimSpace(out.ChannelSecret); secret != "" {
		msg = strings.ReplaceAll(msg, secret, "***")
	}
	return errors.New(msg)
}

// maskedURL 保留主机名，把路径与查询串藏掉。
func maskedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/…"
}
