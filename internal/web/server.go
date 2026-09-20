// Package web 是 HTTP 层：后台界面、登录、webhook 接收端点，以及监听端口的生命周期。
package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/loarland/Forward2Any/internal/engine"
	"github.com/loarland/Forward2Any/internal/mailin"
	"github.com/loarland/Forward2Any/internal/store"
	"github.com/loarland/Forward2Any/internal/web/ui"
)

type Server struct {
	store    *store.Store
	log      *slog.Logger
	engine   *engine.Engine
	mailin   *mailin.Poller
	sessions *sessionStore
	limiter  *loginLimiter

	// passIsDefault 为真时后台只开放设置页，逼迫用户先改掉默认密码。
	// 用原子变量缓存，免得每个请求都去查一次设置。
	passIsDefault atomic.Bool

	mu   sync.Mutex
	srv  *http.Server
	ln   net.Listener
	port int
}

func New(st *store.Store, log *slog.Logger, eng *engine.Engine, poller *mailin.Poller) *Server {
	s := &Server{
		store:    st,
		log:      log,
		engine:   eng,
		mailin:   poller,
		sessions: newSessionStore(),
		limiter:  newLoginLimiter(),
	}
	s.refreshPassFlag()
	return s
}

// refreshPassFlag 同步「是否仍在使用默认密码」的缓存。
// 启动时和改过密码之后各调一次。
func (s *Server) refreshPassFlag() {
	settings, err := s.store.Settings()
	if err != nil {
		s.log.Error("读取设置失败，无法判断是否为默认密码", "err", err)
		return
	}
	s.passIsDefault.Store(settings.AdminPassDefault)
}

// reloadMail 让邮件轮询器重新比对一遍数据库里的邮件接收源。
func (s *Server) reloadMail() {
	if s.mailin != nil {
		s.mailin.Reload()
	}
}

func (s *Server) Handler() http.Handler {
	root := http.NewServeMux()

	root.HandleFunc("GET /healthz", s.handleHealth)
	root.Handle("GET /static/", http.FileServerFS(ui.StaticFS))
	root.HandleFunc("GET /login", s.handleLoginForm)
	root.Handle("POST /login", s.guard(http.HandlerFunc(s.handleLoginPost)))
	root.Handle("POST /logout", s.guard(http.HandlerFunc(s.handleLogout)))

	// 注意：/hook/ 注册在 adminMux 之外，见 hook.go。
	s.registerHook(root)

	// 兜底：其余路径都进后台，未登录会被重定向到登录页。
	root.Handle("/", s.adminMux())

	return s.logRequests(root)
}

func (s *Server) adminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleDashboard)

	s.registerSources(mux)
	s.registerRules(mux)
	s.registerDeliveries(mux)
	s.registerSettings(mux)

	// 先认证，再挡跨站写请求，最后检查默认密码。
	return s.requireAuth(s.guard(s.requirePasswordChanged(mux)))
}

// ---------- 监听端口生命周期 ----------

func (s *Server) Start(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listenLocked(port)
}

// listenLocked 要求调用方已持有 s.mu。
func (s *Server) listenLocked(port int) error {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("监听端口 %d 失败: %w", port, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	s.ln, s.srv, s.port = ln, srv, port
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("HTTP 服务异常退出", "err", err)
		}
	}()
	s.log.Info("HTTP 服务已启动", "addr", ln.Addr().String())
	return nil
}

// Rebind 在运行中切换监听端口；新端口绑不上就回滚到原端口。
func (s *Server) Rebind(port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if port == s.port || s.srv == nil {
		return nil
	}
	oldPort := s.port

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	_ = s.ln.Close()
	s.srv, s.ln, s.port = nil, nil, 0

	if err := s.listenLocked(port); err != nil {
		if rbErr := s.listenLocked(oldPort); rbErr != nil {
			return fmt.Errorf("%w；回滚到端口 %d 也失败: %v", err, oldPort, rbErr)
		}
		return err
	}
	s.log.Info("监听端口已切换", "from", oldPort, "to", port)
	return nil
}

func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// ---------- 处理器 ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(); err != nil {
		http.Error(w, "数据库不可用", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok")
}

// ---------- 渲染与错误 ----------

// flashMessages 把 query 里的短码映射成固定文案。
// 直接回显 query 内容会把任意文本反射进页面，不值当。
var flashMessages = map[string]string{
	"saved":                "已保存",
	"deleted":              "已删除",
	"replayed":             "已重新入队，稍后可在日志里看到结果",
	"imported":             "导入完成",
	"tested":               "测试已发送，请在投递日志里看结果",
	"password_changed":     "密码已修改，后台已全部解锁",
	"must_change_password": "你仍在使用默认密码，请先修改后再使用其它功能",
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	if _, ok := data["Title"]; !ok {
		data["Title"] = "Forward2Any"
	}
	if _, ok := data["Flash"]; !ok {
		if msg, ok := flashMessages[r.URL.Query().Get("ok")]; ok {
			data["Flash"] = msg
		}
	}
	// 布局里据此显示「仍在使用默认密码」的告警条。
	if _, ok := data["DefaultPassword"]; !ok {
		data["DefaultPassword"] = s.passIsDefault.Load()
	}
	if _, ok := data["DefaultPasswordValue"]; !ok {
		data["DefaultPasswordValue"] = store.DefaultAdminPassword
	}

	// 先渲染到内存：模板出错时还能干净地返回 500，而不是半截 HTML。
	var buf bytes.Buffer
	if err := ui.Render(&buf, page, data); err != nil {
		s.log.Error("渲染模板失败", "page", page, "err", err)
		http.Error(w, "模板渲染失败", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(buf.Bytes())
}

func (s *Server) fail(w http.ResponseWriter, msg string, err error) {
	s.log.Error(msg, "err", err)
	http.Error(w, msg, http.StatusInternalServerError)
}

// ---------- 请求日志 ----------

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)

		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return
		}
		s.log.Debug("请求", "method", r.Method, "path", r.URL.Path,
			"status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
