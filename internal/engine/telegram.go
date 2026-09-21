package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/loarland/Forward2Any/internal/store"
)

// tgTextLimit 是 Telegram 单条消息的长度上限（1-4096 characters）。
// 官方按「解析实体之后的字符」算，实际是 UTF-16 码元数 ——
// 所以既不能拿字节数也不能拿 rune 数去比：中文是 1 个码元，emoji 是 2 个。
// https://core.telegram.org/bots/api#sendmessage
const tgTextLimit = 4096

// tgResponse 是 Bot API 的返回信封：成不成看 ok，失败原因看 description 和 error_code。
type tgResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// sendTelegram 通过 Bot API 的 sendMessage 发出一次投递。
// client 由调用方决定：直连的那个，还是绑了代理的那个。
func (e *Engine) sendTelegram(d *store.Delivery, out *store.Source, client *http.Client) (int, string, error) {
	token := strings.TrimSpace(out.TgToken)
	if token == "" {
		return 0, "", errors.New("未配置 Bot Token")
	}
	chatID := strings.TrimSpace(out.TgChatID)
	if chatID == "" {
		return 0, "", errors.New("未配置 Chat ID")
	}
	text := d.Rendered
	if strings.TrimSpace(text) == "" {
		return 0, "", errors.New("消息内容为空")
	}
	// 与其让 Telegram 回一个「message is too long」再重试五次，不如自己先拦下来，
	// 顺便把「太大了」这件事说清楚。不截断：悄悄丢掉半条消息比失败更难查。
	if n := utf16Len(text); n > tgTextLimit {
		return 0, "", fmt.Errorf("消息有 %d 个字符，超过 Telegram 单条 %d 的上限；在规则里精简报文模板，或改用邮件 / Webhook 目标",
			n, tgTextLimit)
	}

	payload := map[string]any{
		"chat_id": tgChatID(chatID),
		"text":    text,
	}
	if thread := strings.TrimSpace(out.TgThreadID); thread != "" {
		n, err := strconv.ParseInt(thread, 10, 64)
		if err != nil {
			return 0, "", fmt.Errorf("话题 ID %q 不是数字", thread)
		}
		payload["message_thread_id"] = n
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", fmt.Errorf("构造请求体失败: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, tgURL(out.TgEndpoint, token), bytes.NewReader(body))
	if err != nil {
		// 这个报错里也带着完整 URL，同样要洗掉 token。
		return 0, "", redactTgToken(fmt.Errorf("构造请求失败: %w", err), token)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", redactTgToken(err, token)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))

	var tr tgResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		// 走错端点、被中间设备拦下时这里拿到的可能是 HTML，把原文带出来最好查。
		return resp.StatusCode, string(respBody),
			fmt.Errorf("返回的不是 Bot API 的 JSON（HTTP %d）：%s", resp.StatusCode, Truncate(string(respBody), 200))
	}
	if !tr.OK {
		desc := tr.Description
		if desc == "" {
			desc = Truncate(strings.TrimSpace(string(respBody)), 200)
		}
		return resp.StatusCode, string(respBody), fmt.Errorf("Telegram 拒绝了这次发送（%d）：%s", tr.ErrorCode, desc)
	}
	return resp.StatusCode, string(respBody), nil
}

// tgURL 拼出 <请求端点><token>/sendMessage。
// 官方就是不带分隔符地拼的（https://api.telegram.org/bot<token>/sendMessage），
// 所以端点约定以 /bot 结尾，这里只负责去掉多余的结尾斜杠；
// 「有没有 /bot」由保存时的校验把关，不在运行时猜用户想说什么。
func tgURL(endpoint, token string) string {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		endpoint = store.DefaultTgEndpoint
	}
	return endpoint + url.PathEscape(token) + "/sendMessage"
}

// tgChatID 让纯数字的 chat_id 以 JSON 数字发出（Bot API 里它是 Integer），
// @channelusername 这类才当字符串。数字要超过 2^53 才会失真，而 Telegram 的 id 最多 52 位。
func tgChatID(s string) any {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	return s
}

func utf16Len(s string) int { return len(utf16.Encode([]rune(s))) }

// redactTgToken 把错误信息里的 token 抹掉。
// Bot API 把 token 放在 URL 路径里，而 http.Client 的报错会带上完整 URL ——
// 这条错误会写进投递记录（last_error）并显示在网页上，所以落库前先洗一遍。
func redactTgToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "***"))
}
