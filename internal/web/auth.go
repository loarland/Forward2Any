package web

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

const (
	sessionCookie = "f2a_session"
	sessionTTL    = 7 * 24 * time.Hour
)

// sessionStore 是进程内会话表。
//
// ponytail: 单管理员场景下重启后重新登录可以接受，不值得为它建一张表。
type sessionStore struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessionStore() *sessionStore { return &sessionStore{m: make(map[string]time.Time)} }

func (s *sessionStore) create() (string, error) {
	tok, err := store.RandomHex(32)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, exp := range s.m {
		if now.After(exp) {
			delete(s.m, k)
		}
	}
	s.m[tok] = now.Add(sessionTTL)
	return tok, nil
}

func (s *sessionStore) valid(tok string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.m, tok)
		return false
	}
	return true
}

func (s *sessionStore) destroy(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, tok)
}

// ---------- 登录失败限流 ----------

// ponytail: 内存计数，重启即清空。bcrypt 本身已有几十毫秒延迟，
// 加上这个足够挡住在线暴力破解；要更严格再换持久化计数。
const (
	loginMaxFails = 5
	loginWindow   = 5 * time.Minute
)

type loginLimiter struct {
	mu sync.Mutex
	m  map[string]*loginAttempt
}

type loginAttempt struct {
	fails int
	until time.Time
}

func newLoginLimiter() *loginLimiter { return &loginLimiter{m: make(map[string]*loginAttempt)} }

func (l *loginLimiter) blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.m[key]
	if !ok || time.Now().After(a.until) {
		delete(l.m, key)
		return false
	}
	return a.fails >= loginMaxFails
}

func (l *loginLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.m[key]
	if !ok || time.Now().After(a.until) {
		a = &loginAttempt{}
		l.m[key] = a
	}
	a.fails++
	a.until = time.Now().Add(loginWindow)
}

func (l *loginLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, key)
}

// ---------- 中间件 ----------

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.sessions.valid(sessionToken(r)) {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard 挡跨站写请求。SameSite=Lax 是主要防线，这里是双保险。
// 只作用于后台路由：/hook/ 要接受来自任意站点的服务端请求，不在此列。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.sameOrigin(r) {
			next.ServeHTTP(w, r)
			return
		}
		origin := requestOrigin(r)
		s.log.Warn("拒绝跨站写请求", "路径", r.URL.Path, "来源", origin, "Host", r.Host,
			"提示", "把访问用的地址加到「设置 → 信任的 Origin / Referer」，或让反向代理转发原始 Host")
		http.Error(w, fmt.Sprintf(
			"跨站请求被拒绝：请求来自 %s，本服务看到的地址是 %s。\n"+
				"用 IP:端口 直接访问后台时两边天然一致，不用配置；\n"+
				"套了反向代理（代理会改写 Host）就到「设置 → 信任的 Origin / Referer」里加上 %s，"+
				"或者让代理转发原始 Host（nginx：proxy_set_header Host $http_host;）。",
			clipHeader(origin), clipHeader(r.Host), clipHeader(origin)), http.StatusForbidden)
	})
}

// sameOrigin 判断写请求的 Origin（没有就看 Referer）是否指向本服务。
// 两边都没有的（curl、脚本）放行。
//
// 认这三种地址：
//  1. 请求上的 Host —— 直接按 IP:端口 访问后台就落在这里，默认可用；
//  2. 「回调基址」里的主机名 —— 管理员声明的公开地址，反代转发 Host 时也落在 1；
//  3. 「信任的 Origin / Referer」里列出的主机名 —— 代理改写 Host 时用它放行。
func (s *Server) sameOrigin(r *http.Request) bool {
	return sameOrigin(r, s.trustedHostList()...)
}

// sameOrigin 是纯函数版本，方便单测。extra 是除 r.Host 之外额外认可的主机。
func sameOrigin(r *http.Request, extra ...string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	raw := requestOrigin(r)
	if raw == "" {
		// 非浏览器客户端（curl、脚本）不会带这两个头。
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	want := normalizeHost(u.Host)
	if want == "" {
		return false
	}
	for _, c := range append([]string{r.Host}, extra...) {
		if normalizeHost(c) == want {
			return true
		}
	}
	return false
}

// requestOrigin 取 Origin，没有就退到 Referer（两者的主机部分语义相同）。
func requestOrigin(r *http.Request) string {
	if v := r.Header.Get("Origin"); v != "" {
		return v
	}
	return r.Header.Get("Referer")
}

// parseTrustedOrigins 解析设置里那一栏：每行一个地址，留空行忽略。
// 行首的 http:// / https:// 和路径都无所谓，只取主机名（带端口就带端口）。
// 认得的形式：hooks.example.com、hooks.example.com:8443、https://hooks.example.com/x
// 返回可用的主机名与看不懂的行（后者用于保存时报错）。
func parseTrustedOrigins(text string) (hosts []string, bad []string) {
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host := line
		if strings.Contains(line, "://") {
			u, err := url.Parse(line)
			if err != nil || u.Host == "" {
				bad = append(bad, line)
				continue
			}
			host = u.Host
		}
		// 去掉可能写上的路径，并挡掉明显的垃圾（空格、纯路径）。
		if i := strings.IndexAny(host, "/?#"); i >= 0 {
			host = host[:i]
		}
		h := normalizeHost(host)
		if h == "" || strings.HasPrefix(h, ":") || !validHostChars(h) {
			bad = append(bad, line)
			continue
		}
		if !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	return hosts, bad
}

// validHostChars 只放行主机名/地址里该出现的字符。
// 中文说明、文件名这类误填的整行会被挡在设置页，而不是存进去当个永远匹配不上的规则。
// （normalizeHost 已经把它们转成小写了，这里只看小写字母。）
func validHostChars(h string) bool {
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == ':', r == '[', r == ']':
		default:
			return false
		}
	}
	return true
}

// normalizeHost 归一化后再比：小写、去掉默认端口。
//
// 纯写法差异不该导致拒绝：nginx 的 `proxy_set_header Host $host` 会把端口吃掉，
// 而浏览器的 Origin 在标准端口上不带端口。非默认端口仍然要求一致 ——
// 同一台机器上的另一个端口是另一个源，不能混为一谈。
func normalizeHost(hostport string) string {
	hostport = strings.ToLower(strings.TrimSpace(hostport))
	if hostport == "" {
		return ""
	}
	if host, port, err := net.SplitHostPort(hostport); err == nil {
		switch port {
		case "80", "443", "":
			return host
		default:
			return net.JoinHostPort(host, port)
		}
	}
	return strings.TrimSuffix(hostport, ":")
}

// clipHeader 把请求头里的值压成适合写进错误页/日志的一行：去掉控制字符、截断。
func clipHeader(v string) string {
	v = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, v)
	if v == "" {
		return "（空）"
	}
	if len(v) > 120 {
		v = v[:120] + "…"
	}
	return v
}

// requirePasswordChanged 在管理员仍使用默认密码时，把后台其它页面全部挡回设置页。
//
// 默认密码是为了「起来就能登」的便利，但一个公网可达的后台配默认密码等于敞开大门，
// 所以用一次强制修改把这个便利限制在首次登录。
// 设置页本身（含导出/导入）放行，否则用户没法完成改密。
func (s *Server) requirePasswordChanged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.passIsDefault.Load() || strings.HasPrefix(r.URL.Path, "/settings") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("HX-Request") == "true" {
			w.Header().Set("HX-Redirect", "/settings")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/settings?ok=must_change_password", http.StatusSeeOther)
	})
}

// ---------- 登录 ----------

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.sessions.valid(sessionToken(r)) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, "login", map[string]any{"Title": "登录"})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		s.render(w, r, "login", map[string]any{
			"Title":      "登录",
			"LoginError": "失败次数过多，请 5 分钟后再试",
		})
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}

	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, "读取设置失败", err)
		return
	}

	user := r.PostFormValue("username")
	pass := r.PostFormValue("password")
	if user != settings.AdminUser || !store.CheckPassword(settings.AdminPassHash, pass) {
		s.limiter.fail(ip)
		s.log.Warn("登录失败", "user", user, "ip", ip)
		s.render(w, r, "login", map[string]any{"Title": "登录", "LoginError": "用户名或密码错误"})
		return
	}

	tok, err := s.sessions.create()
	if err != nil {
		s.fail(w, "创建会话失败", err)
		return
	}
	s.limiter.reset(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   int(sessionTTL / time.Second),
	})
	s.log.Info("登录成功", "user", user, "ip", ip)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.destroy(sessionToken(r))
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: isHTTPS(r),
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

// isHTTPS 判断浏览器到本服务的这一段是不是 HTTPS。
// 直接 TLS 或前置反代声明了 X-Forwarded-Proto 都算。
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// clientIP 只信任直连对端地址。X-Forwarded-For 可伪造，
// 用它做限流键等于把限流关掉。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
