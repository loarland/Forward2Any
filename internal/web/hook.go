package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/loarland/Webhook2Any/internal/engine"
	"github.com/loarland/Webhook2Any/internal/store"
)

// maxInboundBody 是单次入站报文的内存读取上限。
// 落库时还会按设置里的 payload_max_bytes 再截一次。
const maxInboundBody = 10 << 20

// registerHook 挂载接收端点。
//
// 刻意不走 guard：发送方来自任意站点，Origin 校验会误伤；
// 这个端点的鉴权由源自己的 auth_mode 负责。
func (s *Server) registerHook(root *http.ServeMux) {
	root.HandleFunc("/hook/{slug}", s.handleHook)
}

func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		http.Error(w, "该端点只接受 POST/PUT/PATCH", http.StatusMethodNotAllowed)
		return
	}

	slug := r.PathValue("slug")
	src, err := s.store.SourceBySlug(slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.fail(w, "读取源失败", err)
		return
	}
	// 不存在、停用、非接收用途一律当作 404，不对外暴露区别。
	if !src.Enabled || !src.CanReceive() || src.Kind != "webhook" {
		http.NotFound(w, r)
		return
	}

	ip := clientIP(r)
	if !ipAllowed(src.IPAllow, ip) {
		s.log.Warn("来源 IP 不在白名单内", "源", src.Name, "ip", ip)
		http.Error(w, "来源 IP 未授权", http.StatusForbidden)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxInboundBody))
	if err != nil {
		http.Error(w, "读取请求报文失败", http.StatusBadRequest)
		return
	}

	if !verifyInbound(src, r, body) {
		s.log.Warn("webhook 鉴权失败", "源", src.Name, "ip", ip)
		http.Error(w, "鉴权失败", http.StatusUnauthorized)
		return
	}

	traceID := strings.TrimSpace(r.Header.Get("X-W2A-Trace"))
	if traceID == "" {
		traceID, _ = store.RandomHex(8)
	}
	hops := engine.ParseHops(r.Header.Get("X-W2A-Hops"))

	if engine.IsLoop(hops, src.Slug) {
		s.log.Warn("检测到循环转发，已拦截", "源", src.Name, "跳链", strings.Join(hops, ","))
		s.engine.RecordDropped(src, traceID, string(body), "检测到循环转发，已拦截", strings.Join(hops, ","))
		// 返回 200 而不是错误码：否则发送方会不停地重试一个永远不会成功的请求。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "loop dropped")
		return
	}

	// 能解析成 JSON 就解析，过滤器和模板可以直接取字段；否则退化为纯文本。
	var parsed any
	if len(body) > 0 {
		_ = json.Unmarshal(body, &parsed)
	}

	n, err := s.engine.Submit(&engine.Inbound{
		Source:      src,
		Payload:     body,
		Parsed:      parsed,
		Headers:     headerMap(r.Header),
		Hops:        hops,
		TraceID:     traceID,
		ContentType: r.Header.Get("Content-Type"),
	})
	if err != nil {
		s.fail(w, "分发失败", err)
		return
	}

	s.log.Info("接收 webhook", "源", src.Name, "ip", ip, "字节", len(body), "投递", n, "trace", traceID)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprintf(w, "accepted %d", n)
}

// verifyInbound 按源配置的鉴权方式校验入站请求。
func verifyInbound(src *store.Source, r *http.Request, body []byte) bool {
	switch src.AuthMode {
	case "", "none":
		return true

	case "token":
		if src.AuthSecret == "" {
			return false
		}
		name := src.AuthHeader
		if name == "" {
			name = "X-W2A-Token"
		}
		got := r.Header.Get(name)
		if got == "" {
			got = r.URL.Query().Get("token")
		}
		return subtle.ConstantTimeCompare([]byte(got), []byte(src.AuthSecret)) == 1

	case "hmac_sha256":
		if src.AuthSecret == "" {
			return false
		}
		name := src.AuthHeader
		if name == "" {
			name = "X-Hub-Signature-256"
		}
		got := strings.TrimPrefix(strings.TrimSpace(r.Header.Get(name)), "sha256=")
		if got == "" {
			return false
		}
		mac := hmac.New(sha256.New, []byte(src.AuthSecret))
		mac.Write(body)
		want := hex.EncodeToString(mac.Sum(nil))
		return hmac.Equal([]byte(strings.ToLower(got)), []byte(want))

	case "basic":
		user, pass, ok := r.BasicAuth()
		if !ok {
			return false
		}
		// basic 模式下 auth_header 复用为用户名。
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(src.AuthHeader)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(src.AuthSecret)) == 1
		return userOK && passOK
	}
	return false
}

// ipAllowed 校验来源 IP。空配置表示不限制。
func ipAllowed(allow, ipStr string) bool {
	allow = strings.TrimSpace(allow)
	if allow == "" {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	// 统一成 4 字节形式，否则 IPv4 映射地址匹配不上 IPv4 的 CIDR。
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	for _, part := range strings.Split(allow, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			_, cidr, err := net.ParseCIDR(part)
			if err == nil && cidr.Contains(ip) {
				return true
			}
			continue
		}
		if p := net.ParseIP(part); p != nil && p.Equal(ip) {
			return true
		}
	}
	return false
}

// headerMap 把 http.Header 摊平成 map[string]string，供模板使用。
func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}
