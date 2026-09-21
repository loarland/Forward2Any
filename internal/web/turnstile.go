package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

// Cloudflare Turnstile 的登录人机校验。
//
// 默认关闭：设置里没开启时，登录页连脚本都不加载（不引入任何外部请求）。
// 开启后登录必须先拿到一个 token，服务端再拿它去 siteverify 验一次 —— 只看
// 前端有没有控件是不行的，脚本可以被绕过，必须服务端校验。
const (
	// turnstileScriptURL 是前端控件脚本。浏览器要能直接访问它。
	turnstileScriptURL = "https://challenges.cloudflare.com/turnstile/v0/api.js"
	// turnstileDefaultEndpoint 是服务端校验接口，只在进程启动时读一次环境变量，
	// 好让自建中转（或端到端测试）把它指到别处。
	turnstileDefaultEndpoint = "https://challenges.cloudflare.com/turnstile/v0/siteverify"
)

// turnstileClient 单独给校验用：这个请求不能拖住登录。
var turnstileClient = &http.Client{Timeout: 8 * time.Second}

// turnstileForcedOff 读 F2A_TURNSTILE=off。
//
// 这是「被盾挡在门外」时的救命开关：密钥配错、或者服务器/浏览器访问不到
// challenges.cloudflare.com 时，登录页会一直过不去，而后台是进不去的 ——
// 所以留一个不用进后台就能关掉它的办法。
func turnstileForcedOff() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("F2A_TURNSTILE"))) {
	case "off", "0", "false", "no", "disable", "disabled":
		return true
	}
	return false
}

// turnstileEndpoint 读 F2A_TURNSTILE_ENDPOINT，留空用官方地址。
func turnstileEndpoint() string {
	if v := strings.TrimSpace(os.Getenv("F2A_TURNSTILE_ENDPOINT")); v != "" {
		return v
	}
	return turnstileDefaultEndpoint
}

// turnstileActive 报告这次登录要不要过盾。
//
// 开关打开、两个密钥都在、且没被环境变量强制关掉，才算真的启用 ——
// 只勾了开关没填密钥时当作没开，免得把自己锁在门外。
func (s *Server) turnstileActive(settings *store.Settings) bool {
	if s.turnstileOff {
		return false
	}
	return settings.TurnstileEnabled &&
		settings.TurnstileSiteKey != "" && settings.TurnstileSecret != ""
}

// turnstileResponse 是 siteverify 的返回信封。
type turnstileResponse struct {
	Success    bool     `json:"success"`
	ErrorCodes []string `json:"error-codes"`
	Hostname   string   `json:"hostname"`
}

// verifyTurnstile 拿浏览器给的 token 去校验一次。
// token 是一次性的，重复用会失败，所以只在登录提交时验。
func (s *Server) verifyTurnstile(ctx context.Context, secret, token, remoteIP string) error {
	form := url.Values{"secret": {secret}, "response": {token}}
	if remoteIP != "" {
		form.Set("remoteip", remoteIP)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.turnstileURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("构造校验请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := turnstileClient.Do(req)
	if err != nil {
		return fmt.Errorf("连接校验服务失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("校验服务返回 %s", resp.Status)
	}
	var out turnstileResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return errors.New("校验服务的响应看不懂")
	}
	if !out.Success {
		if len(out.ErrorCodes) > 0 {
			return fmt.Errorf("校验不通过（%s）", strings.Join(out.ErrorCodes, ", "))
		}
		return errors.New("校验不通过")
	}
	return nil
}
