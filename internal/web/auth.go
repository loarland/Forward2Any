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

// destroyOthers 删掉除 keep 之外的所有会话。
// 改密码之后别的浏览器/设备上挂着的会话不该继续有效，当前这一台留着，
// 免得管理员刚改完密码就被自己踢回登录页。
func (s *sessionStore) destroyOthers(keep string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if k != keep {
			delete(s.m, k)
		}
	}
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
// 设置页的开关关掉之后这里直接放行（只剩 cookie 的 SameSite 兜着）。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.originAllowed(r) {
			next.ServeHTTP(w, r)
			return
		}
		origin := requestOrigin(r)
		s.log.Warn("拒绝跨站写请求", "路径", r.URL.Path, "来源", origin, "Host", r.Host,
			"提示", "把访问用的地址加到「设置 → 跨站请求校验 → 允许的 Origin / Referer」，或让反向代理转发原始 Host")
		http.Error(w, fmt.Sprintf(
			"跨站请求被拒绝：请求来自 %s，本服务看到的地址是 %s。\n"+
				"用 IP:端口 直接访问后台时两边天然一致，不用配置；套了反向代理（代理会改写 Host）就到"+
				"「设置 → 跨站请求校验」的允许列表里加上 %s，或者让代理转发原始 Host"+
				"（nginx：proxy_set_header Host $http_host;）。",
			clipHeader(origin), clipHeader(r.Host), clipHeader(origin)), http.StatusForbidden)
	})
}

// originAllowed 判断写请求的 Origin（没有就看 Referer）是否被允许。
// 开关关掉之后一律放行。
func (s *Server) originAllowed(r *http.Request) bool {
	if !s.originCheckOn() {
		return true
	}
	return originMatches(r, s.allowedOriginList())
}

// originMatches 是校验的纯函数部分，方便单测。放行这几种：
//
//  1. 非写请求、以及两个头都不带的请求（curl、脚本）；
//  2. 同源 —— 按 IP:端口 直接访问后台就落在这里，默认可用，也是「先能进后台」的保证；
//  3. 允许列表里的条目 —— 反向代理改写 Host 时靠它放行，其中「回调基址」的主机名
//     在进缓存时按不限协议处理。
func originMatches(r *http.Request, allow []allowedOrigin) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	raw := requestOrigin(r)
	if raw == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := normalizeHost(u.Host)
	if host == "" {
		return false
	}
	if host == normalizeHost(r.Host) {
		return true
	}
	scheme := strings.ToLower(u.Scheme)
	for _, a := range allow {
		if a.matches(scheme, host) {
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

// allowedOrigin 是允许列表里的一条。scheme 为空表示不限协议，只比主机名。
type allowedOrigin struct {
	scheme string
	host   string
}

// matches 判断请求的 scheme / 主机名是否落在这一条上。
func (a allowedOrigin) matches(scheme, host string) bool {
	return a.host == host && (a.scheme == "" || a.scheme == scheme)
}

// parseAllowedOrigins 解析设置里那一栏：每行或用逗号分隔一条，空项忽略。
// 认得 https://hooks.example.com、hooks.example.com、hooks.example.com:8443，
// 带路径和查询串也无所谓，只取 scheme 与主机名。返回看不懂的条目用于保存时报错。
func parseAllowedOrigins(text string) (list []allowedOrigin, bad []string) {
	seen := map[string]bool{}
	for _, item := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ';'
	}) {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		entry := allowedOrigin{}
		rest := item
		if i := strings.Index(rest, "://"); i >= 0 {
			scheme := strings.ToLower(rest[:i])
			if scheme != "http" && scheme != "https" {
				bad = append(bad, item)
				continue
			}
			entry.scheme = scheme
			rest = rest[i+3:]
		}
		// 去掉路径 / 查询串 / userinfo。
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			rest = rest[:i]
		}
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		host := normalizeHost(rest)
		if host == "" || strings.HasPrefix(host, ":") || !validHostChars(host) {
			bad = append(bad, item)
			continue
		}
		entry.host = host
		key := entry.scheme + "|" + entry.host
		if !seen[key] {
			seen[key] = true
			list = append(list, entry)
		}
	}
	return list, bad
}

// formatAllowedOrigins 把允许列表写回设置页那一栏：一行一条，带协议的原样带上。
func formatAllowedOrigins(list []allowedOrigin) string {
	lines := make([]string, 0, len(list))
	for _, a := range list {
		if a.scheme == "" {
			lines = append(lines, a.host)
			continue
		}
		lines = append(lines, a.scheme+"://"+a.host)
	}
	return strings.Join(lines, "\n")
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
	s.renderLogin(w, r, "")
}

// renderLogin 渲染登录页；盾开着的时候把控件也带上。
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, errMsg string) {
	data := map[string]any{"Title": "登录", "LoginError": errMsg}
	settings, err := s.store.Settings()
	if err != nil {
		s.log.Error("读取设置失败，登录页按未开启人机校验渲染", "err", err)
	} else if s.turnstileActive(settings) {
		data["Turnstile"] = true
		data["TurnstileSiteKey"] = settings.TurnstileSiteKey
		data["TurnstileScript"] = turnstileScriptURL
	}
	s.render(w, r, "login", data)
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if s.limiter.blocked(ip) {
		s.renderLogin(w, r, "失败次数过多，请 5 分钟后再试")
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

	// 人机校验排在验密码之前：这一步过不去就没必要去碰 bcrypt。
	// 校验失败不计入登录失败次数 —— 控件加载不出来（网络问题）时不该顺带把 IP 锁了。
	if s.turnstileActive(settings) {
		token := r.PostFormValue("cf-turnstile-response")
		if token == "" {
			s.log.Warn("登录缺少人机校验 token", "ip", ip)
			s.renderLogin(w, r, "人机校验没通过，请刷新页面重试")
			return
		}
		if err := s.verifyTurnstile(r.Context(), settings.TurnstileSecret, token, ip); err != nil {
			s.log.Warn("人机校验失败", "ip", ip, "err", err)
			s.renderLogin(w, r, "人机校验没通过，请刷新页面重试")
			return
		}
	}

	user := r.PostFormValue("username")
	pass := r.PostFormValue("password")
	if user != settings.AdminUser || !store.CheckPassword(settings.AdminPassHash, pass) {
		s.limiter.fail(ip)
		s.log.Warn("登录失败", "user", user, "ip", ip)
		s.renderLogin(w, r, "用户名或密码错误")
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

// clientIP 返回请求的真实来源 IP。
//
// 直连对端不在「受信代理」列表里时只认 RemoteAddr —— X-Forwarded-For 谁都能伪造，
// 拿它做限流键等于把限流关掉。对端是受信代理时，从 X-Forwarded-For 最右侧往回找
// 第一个不属于受信代理的地址：那是代理亲手写下的、也是最后一个可信任的跳。
// 列表为空（默认）表示不信任任何代理。
func (s *Server) clientIP(r *http.Request) string {
	direct := remoteIP(r)
	trusted := s.trustedProxyNets()
	if len(trusted) == 0 || !ipInNets(direct, trusted) {
		return direct
	}
	parts := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part == "" {
			continue
		}
		// 认不出来的值不能当客户端地址用（会进日志和限流键），退回直连地址。
		if net.ParseIP(part) == nil {
			return direct
		}
		if !ipInNets(part, trusted) {
			return part
		}
	}
	return direct
}

// trustedProxyNets 取缓存里的受信代理网段。
func (s *Server) trustedProxyNets() []*net.IPNet {
	if v, ok := s.trustedProxies.Load().([]*net.IPNet); ok {
		return v
	}
	return nil
}

// refreshTrustedProxies 重新解析「受信代理」那一栏并缓存。启动时与保存设置后各调一次。
func (s *Server) refreshTrustedProxies() {
	settings, err := s.store.Settings()
	if err != nil {
		s.log.Error("读取设置失败，受信代理按空处理", "err", err)
		s.trustedProxies.Store([]*net.IPNet{})
		return
	}
	s.trustedProxies.Store(parseProxyNets(settings.TrustedProxies))
}

// remoteIP 取直连对端的地址。
func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ipInNets 判断地址是否落在任一网段里（IPv4 映射地址由 net.IPNet.Contains 归一）。
func ipInNets(s string, nets []*net.IPNet) bool {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil {
		return false
	}
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseProxyNets 把「受信代理」那一栏解析成网段：单个 IP 按 /32、/128 处理。
// 看不懂的条目跳过（保存时已经被挡下来了，这里只是防御直接改库留下的值）。
func parseProxyNets(text string) []*net.IPNet {
	out := []*net.IPNet{}
	for _, item := range splitList(text) {
		if strings.Contains(item, "/") {
			if _, n, err := net.ParseCIDR(item); err == nil {
				out = append(out, n)
			}
			continue
		}
		if ip := net.ParseIP(item); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return out
}

// validateProxyNets 校验「受信代理」那一栏：空表示不信任任何代理。
func validateProxyNets(text string) error {
	for _, item := range splitList(text) {
		if strings.Contains(item, "/") {
			if _, _, err := net.ParseCIDR(item); err != nil {
				return fmt.Errorf("网段 %q 不合法，应形如 172.18.0.0/16", item)
			}
			continue
		}
		if net.ParseIP(item) == nil {
			return fmt.Errorf("地址 %q 不合法，要写 IP 或网段（例如 172.18.0.2、10.0.0.0/8）", item)
		}
	}
	return nil
}

// splitList 按逗号、分号或换行拆分一个列表项，空项忽略。
func splitList(text string) []string {
	out := []string{}
	for _, item := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r'
	}) {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
