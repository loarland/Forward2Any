package web

import (
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
		if !sameOrigin(r) {
			http.Error(w, "跨站请求被拒绝", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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

func sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	raw := r.Header.Get("Origin")
	if raw == "" {
		raw = r.Header.Get("Referer")
	}
	if raw == "" {
		// 非浏览器客户端（curl、脚本）不会带这两个头。
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
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
