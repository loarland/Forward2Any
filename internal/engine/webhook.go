package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/loarland/Forward2Any/internal/store"
)

// sendWebhook 用给定的客户端发出一次 HTTP 投递。
// client 由调用方决定：直连的那个，还是绑了代理的那个。
func (e *Engine) sendWebhook(d *store.Delivery, out *store.Source, client *http.Client) (int, string, error) {
	if strings.TrimSpace(out.URL) == "" {
		return 0, "", errors.New("目标地址为空")
	}
	method := strings.ToUpper(strings.TrimSpace(out.HTTPMethod))
	if method == "" {
		method = http.MethodPost
	}

	var body io.Reader
	if d.Rendered != "" {
		body = strings.NewReader(d.Rendered)
	}
	req, err := http.NewRequest(method, out.URL, body)
	if err != nil {
		return 0, "", fmt.Errorf("构造请求失败: %w", err)
	}

	if d.ReqHeaders != "" {
		var hdrs map[string]string
		if err := json.Unmarshal([]byte(d.ReqHeaders), &hdrs); err != nil {
			return 0, "", fmt.Errorf("投递记录 %d 的请求头不是合法 JSON: %w", d.ID, err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}

	// 带上跳链，对方若也是 Forward2Any 就能自动断环。
	if d.TraceID != "" {
		req.Header.Set("X-F2A-Trace", d.TraceID)
	}
	if d.HopChain != "" {
		req.Header.Set("X-F2A-Hops", d.HopChain)
	}

	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, string(respBody), fmt.Errorf("目标返回 %s", resp.Status)
	}
	return resp.StatusCode, string(respBody), nil
}
