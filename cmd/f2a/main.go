// 命令 f2a：Forward2Any 服务端。
//
//	go run ./cmd/f2a              # 启动服务
//	./f2a version                 # 打印版本（发布包和 install.sh 靠它对账）
//	./f2a healthcheck --url ...   # 容器健康检查（distroless 里没有 curl）
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// 把时区库编进二进制，distroless 镜像里没有 tzdata。
	_ "time/tzdata"

	"github.com/loarland/Forward2Any/internal/config"
	"github.com/loarland/Forward2Any/internal/engine"
	"github.com/loarland/Forward2Any/internal/mailin"
	"github.com/loarland/Forward2Any/internal/store"
	"github.com/loarland/Forward2Any/internal/web"
)

// version 由发布流程在编译时注入：
//
//	go build -ldflags "-X main.version=1.0.0" ./cmd/f2a
//
// 直接 go build / go run 时就显示 dev。
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck(os.Args[2:]))
		case "version", "-version", "--version":
			fmt.Println(version)
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "启动失败:", err)
		os.Exit(1)
	}
}

func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	urlFlag := fs.String("url", "", "健康检查地址；留空则按数据目录里的设置自动推断")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// 端口是可以在后台改的，所以默认按数据库里的实际值来探，
	// 免得用户改了端口之后容器一直显示 unhealthy。
	target := *urlFlag
	if target == "" {
		port := 16000
		if cfg, err := config.FromEnv(); err == nil {
			if st, err := store.Open(cfg.DataDir); err == nil {
				if s, err := st.Settings(); err == nil {
					port = s.WebPort
				}
				st.Close()
			}
		}
		target = fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "健康检查失败:", err)
		return 1
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "健康检查返回", resp.Status)
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	usingDefaultPassword, err := st.Bootstrap(store.BootstrapInput{
		Port:      cfg.Port,
		AdminUser: cfg.AdminUser,
		AdminPass: cfg.AdminPass,
		BaseURL:   cfg.BaseURL,
	})
	if err != nil {
		return err
	}

	settings, err := st.Settings()
	if err != nil {
		return err
	}

	log.Info("Forward2Any 已启动",
		"版本", version, "db", st.Path, "端口", settings.WebPort,
		"管理员", settings.AdminUser, "回调基址", settings.BaseURL)
	if usingDefaultPassword {
		// 只在使用默认密码时出现。改掉之后后台才会全部解锁。
		log.Warn("当前使用默认管理员密码，登录后会强制要求修改；改掉之前后台只开放设置页",
			"用户名", settings.AdminUser, "默认密码", store.DefaultAdminPassword)
	}

	// 转发引擎：负责规则分发与失败重试。
	eng := engine.New(st, log)
	eng.Start()
	defer eng.Stop()

	// 邮件接收：按数据库里的邮件接收源按需起停轮询协程。
	poller := mailin.New(st, eng, log)
	poller.Start()
	defer poller.Stop()

	srv := web.New(st, log, eng, poller)
	if err := srv.Start(settings.WebPort); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Info("收到退出信号，正在关闭…")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
