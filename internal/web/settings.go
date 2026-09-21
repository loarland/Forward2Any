package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/loarland/Forward2Any/internal/store"
)

func (s *Server) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings", s.handleSettingsSave)
	mux.HandleFunc("GET /settings/export", s.handleExport)
	mux.HandleFunc("POST /settings/import", s.handleImport)
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, "", "")
}

func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, errMsg, notice string) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, "读取设置失败", err)
		return
	}
	loc := settings.Location()
	zoneLabel := strings.TrimSpace(settings.Timezone)
	if zoneLabel == "" {
		zoneLabel = "跟随系统（" + time.Now().In(loc).Format("MST") + "）"
	}
	data := map[string]any{
		"Title":       "设置",
		"Nav":         "settings",
		"S":           settings,
		"ZoneLabel":   zoneLabel,
		"NowText":     time.Now().In(loc).Format("2006-01-02 15:04:05 MST"),
		"RunningPort": s.Port(),
		"DBPath":      s.store.Path,
		"Error":       errMsg,
		"Palettes":    palettes,
		"ThemeModes":  themeModes,
	}
	// 换端口那一次没法重定向（旧端口马上要关掉），只能就地渲染一条提示。
	// 塞进 Flash 是为了让布局把它渲染成同一个浮层 —— 页面级的反馈全站只有这一种长相。
	// 注意别写成 "Flash": ""：那样 render 会认为调用方已经给过了，
	// ?ok= 那套固定文案（saved / imported / password_changed）就再也生效不了。
	if notice != "" {
		data["Flash"] = notice
	}
	s.render(w, r, "settings", data)
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}

	current, err := s.store.Settings()
	if err != nil {
		s.fail(w, "读取设置失败", err)
		return
	}

	next := *current
	next.WebPort = formInt(r, "web_port", current.WebPort)
	next.AdminUser = formValue(r, "admin_user")
	next.BaseURL = strings.TrimRight(formValue(r, "base_url"), "/")
	// 信任来源和代理一样，只在表单确实提交了它时才动：只提交部分字段的请求
	// 不该把已有的白名单抹掉。存的是规整后的主机名，看不懂的行直接挡回去，不静默丢掉。
	if _, ok := r.PostForm["trusted_origins"]; ok {
		trusted, badTrusted := parseAllowedOrigins(formValue(r, "trusted_origins"))
		if len(badTrusted) > 0 {
			s.renderSettings(w, r, "允许列表里这几条看不懂："+strings.Join(badTrusted, "、")+
				"。写主机名或完整地址都行，例如 https://hooks.example.com", "")
			return
		}
		next.TrustedOrigins = formatAllowedOrigins(trusted)
	}
	// 开关：模板里复选框后面跟了个 hidden 的 "0"，所以未勾选时也能读到明确的值。
	if _, ok := r.PostForm["origin_check"]; ok {
		next.OriginCheck = r.PostFormValue("origin_check") == "1"
	}
	// 时区和信任来源一样，只在表单确实提交了它时才动：
	// 别的只提交部分字段的请求不该把它清成「跟随系统」。
	if _, ok := r.PostForm["timezone"]; ok {
		next.Timezone = strings.TrimSpace(formValue(r, "timezone"))
	}
	next.RetryMax = formInt(r, "retry_max", current.RetryMax)
	next.RetryBackoffSeconds = formInt(r, "retry_backoff_seconds", current.RetryBackoffSeconds)
	next.PayloadMaxBytes = formInt(r, "payload_max_bytes", current.PayloadMaxBytes)
	next.LogRetentionDays = formInt(r, "log_retention_days", current.LogRetentionDays)
	// 外观的两个值都按白名单校验，不合法就保持原样 —— 它们会进 <link href>。
	if v := formValue(r, "theme_color"); validThemeColor(v) {
		next.ThemeColor = v
	}
	if v := formValue(r, "theme_mode"); validThemeMode(v) {
		next.ThemeMode = v
	}
	// 代理这一组只在表单确实提交了它们时才动：别的只提交部分字段的请求
	// （比如只改配色）不该把代理配置抹掉。
	if _, ok := r.PostForm["proxy_type"]; ok {
		next.ProxyType = formValue(r, "proxy_type")
		if next.ProxyType == "" {
			next.ProxyType = "none"
		}
		next.ProxyAddr = formValue(r, "proxy_addr")
	}

	if err := validateSettings(&next); err != nil {
		s.renderSettings(w, r, err.Error(), "")
		return
	}

	passwordChanged := false
	if newPass := r.PostFormValue("new_password"); newPass != "" {
		if newPass != r.PostFormValue("confirm_password") {
			s.renderSettings(w, r, "两次输入的密码不一致", "")
			return
		}
		// 默认密码的检查要排在长度检查前面：默认密码本身很短，
		// 反过来的话这条分支永远不会被命中，用户只会看到「至少 8 位」。
		if newPass == store.DefaultAdminPassword {
			s.renderSettings(w, r, "新密码不能和默认密码相同", "")
			return
		}
		if len([]rune(newPass)) < 8 {
			s.renderSettings(w, r, "新密码至少 8 位", "")
			return
		}
		hash, err := store.HashPassword(newPass)
		if err != nil {
			s.fail(w, "哈希密码失败", err)
			return
		}
		next.AdminPassHash = hash
		next.AdminPassDefault = false
		passwordChanged = true
		s.log.Info("管理员密码已更新", "用户", next.AdminUser)
	}

	oldPort := s.Port()
	if err := s.store.SaveSettings(&next); err != nil {
		s.fail(w, "保存设置失败", err)
		return
	}
	if passwordChanged {
		// 改完密码后台就该解锁，不必等重启。
		s.refreshPassFlag()
	}
	// 换配色/亮暗同样立刻生效，不用重启进程。
	s.refreshTheme()
	// 回调基址、开关或允许列表可能改了，跨站校验策略跟着更新。
	s.refreshOriginPolicy()
	// 时区可能改了：页面时间和日志时间都跟着换。
	s.refreshTimezone()
	s.log.Info("更新设置", "端口", next.WebPort, "管理员", next.AdminUser)

	if next.WebPort != oldPort {
		// 先把这次响应写回浏览器，再去切监听端口。
		// 在处理器里直接 Shutdown 会等待当前请求结束 —— 那就是自己等自己。
		go func() {
			time.Sleep(500 * time.Millisecond)
			if err := s.Rebind(next.WebPort); err != nil {
				s.log.Error("切换监听端口失败，已在原端口继续运行", "err", err)
			}
		}()
		s.renderSettings(w, r, "", fmt.Sprintf(
			"设置已保存。监听端口正在从 %d 切换到 %d，请稍后用新端口访问。"+
				"若在 Docker 中运行，还需要同步修改 docker-compose.yml 的端口映射。", oldPort, next.WebPort))
		return
	}
	if passwordChanged {
		http.Redirect(w, r, "/settings?ok=password_changed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings?ok=saved", http.StatusSeeOther)
}

func validateSettings(v *store.Settings) error {
	if v.WebPort < 1 || v.WebPort > 65535 {
		return errors.New("监听端口必须在 1–65535 之间")
	}
	if v.AdminUser == "" {
		return errors.New("管理员用户名不能为空")
	}
	if v.BaseURL == "" {
		return errors.New("回调基址不能为空")
	}
	if !strings.HasPrefix(v.BaseURL, "http://") && !strings.HasPrefix(v.BaseURL, "https://") {
		return errors.New("回调基址必须以 http:// 或 https:// 开头")
	}
	if v.RetryMax < 1 || v.RetryMax > 100 {
		return errors.New("最大重试次数需在 1–100 之间")
	}
	if v.RetryBackoffSeconds < 1 {
		return errors.New("重试退避基数至少 1 秒")
	}
	if v.PayloadMaxBytes < 1024 {
		return errors.New("报文保存上限至少 1024 字节")
	}
	if v.LogRetentionDays < 1 {
		return errors.New("日志保留天数至少 1 天")
	}
	// 时区留空表示跟随系统；填了就必须是 Go 认得的 IANA 名称，
	// 否则页面上的时间会静默退回系统时区，用户以为设上了其实没生效。
	if v.Timezone != "" {
		if _, err := time.LoadLocation(v.Timezone); err != nil {
			return fmt.Errorf("时区 %q 不认识。要填 IANA 名称，例如 Asia/Shanghai、Asia/Tokyo、Europe/London", v.Timezone)
		}
	}
	return validateProxy(v.ProxyType, v.ProxyAddr)
}

// validateProxy 校验代理设置。类型和地址要么都空着（不使用代理），要么都对。
func validateProxy(kind, addr string) error {
	switch kind {
	case "", "none":
		return nil
	case "http", "https", "socks5":
	default:
		return errors.New("代理类型只能是 HTTP / HTTPS / SOCKS5")
	}
	if addr == "" {
		return errors.New("选了代理类型就得填代理地址，例如 127.0.0.1:7890")
	}
	if strings.Contains(addr, "://") {
		return errors.New("代理地址不用带 http:// 前缀，类型在上面选，地址只填 主机:端口")
	}
	// 允许把账号密码写在地址里，标准库对三种代理都支持。
	// 先摘掉 userinfo 再拆主机端口，否则 SplitHostPort 会被多出来的冒号绊住。
	hostport := addr
	if i := strings.LastIndex(hostport, "@"); i >= 0 {
		hostport = hostport[i+1:]
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return fmt.Errorf("代理地址应形如 127.0.0.1:7890（%v）", err)
	}
	if strings.TrimSpace(host) == "" {
		return errors.New("代理地址里没有主机名")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("代理端口 %q 不合法", port)
	}
	return nil
}

// ---------- 导出 / 导入 ----------

const configVersion = 1

// exportFile 刻意不含管理员账号与监听端口：那是本机部署的事，不该跟着配置走。
type exportFile struct {
	Version    int            `json:"version"`
	ExportedAt int64          `json:"exported_at"`
	Settings   map[string]any `json:"settings"`
	Sources    []exportSource `json:"sources"`
	Rules      []exportRule   `json:"rules"`
}

// exportSource 带一个下标，规则通过下标引用源，于是文件里不含数据库主键。
type exportSource struct {
	Index int `json:"index"`
	*store.Source
}

type exportRule struct {
	Name            string         `json:"name"`
	Enabled         bool           `json:"enabled"`
	From            []int64        `json:"from"`
	To              []int64        `json:"to"`
	Filters         []store.Filter `json:"filters"`
	BodyTemplate    string         `json:"body_template"`
	SubjectTemplate string         `json:"subject_template"`
	HeadersTemplate string         `json:"headers_template"`
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sources, err := s.store.ListSources()
	if err != nil {
		s.fail(w, "读取源列表失败", err)
		return
	}
	rules, err := s.store.ListRules()
	if err != nil {
		s.fail(w, "读取规则列表失败", err)
		return
	}
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, "读取设置失败", err)
		return
	}

	idx := make(map[int64]int, len(sources))
	ef := exportFile{
		Version:    configVersion,
		ExportedAt: time.Now().Unix(),
		Settings: map[string]any{
			"base_url":              settings.BaseURL,
			"retry_max":             settings.RetryMax,
			"retry_backoff_seconds": settings.RetryBackoffSeconds,
			"payload_max_bytes":     settings.PayloadMaxBytes,
			"log_retention_days":    settings.LogRetentionDays,
		},
	}
	for i, src := range sources {
		idx[src.ID] = i
		ef.Sources = append(ef.Sources, exportSource{Index: i, Source: src})
	}
	for _, rule := range rules {
		er := exportRule{
			Name:            rule.Name,
			Enabled:         rule.Enabled,
			Filters:         rule.Filters,
			BodyTemplate:    rule.BodyTemplate,
			SubjectTemplate: rule.SubjectTemplate,
			HeadersTemplate: rule.HeadersTemplate,
		}
		for _, id := range rule.FromSourceIDs {
			if i, ok := idx[id]; ok {
				er.From = append(er.From, int64(i))
			}
		}
		for _, id := range rule.ToSourceIDs {
			if i, ok := idx[id]; ok {
				er.To = append(er.To, int64(i))
			}
		}
		ef.Rules = append(ef.Rules, er)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf(`attachment; filename="forward2any-%s.json"`, time.Now().Format("20060102-150405")))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(ef); err != nil {
		s.log.Error("写出导出文件失败", "err", err)
	}
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		http.Error(w, "读取上传内容失败", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		s.renderSettings(w, r, "没有收到配置文件", "")
		return
	}
	defer file.Close()

	var ef exportFile
	if err := json.NewDecoder(io.LimitReader(file, 8<<20)).Decode(&ef); err != nil {
		s.renderSettings(w, r, "配置文件不是合法 JSON: "+err.Error(), "")
		return
	}
	if ef.Version != configVersion {
		s.renderSettings(w, r, fmt.Sprintf("配置版本 %d 不受支持（当前支持 %d）", ef.Version, configVersion), "")
		return
	}

	sources := make([]*store.Source, 0, len(ef.Sources))
	seenSlug := map[string]bool{}
	for _, es := range ef.Sources {
		if es.Source == nil {
			continue
		}
		src := *es.Source
		src.ID = 0
		// 导入是整体替换，所以只做「形态」校验，不跟库里现有数据比 slug。
		if err := validateSourceShape(&src); err != nil {
			s.renderSettings(w, r, fmt.Sprintf("第 %d 个源不合法: %v", es.Index, err), "")
			return
		}
		if src.Slug != "" {
			if seenSlug[src.Slug] {
				s.renderSettings(w, r, fmt.Sprintf("配置文件里有重复的路径标识 %q", src.Slug), "")
				return
			}
			seenSlug[src.Slug] = true
		}
		sources = append(sources, &src)
	}

	rules := make([]*store.Rule, 0, len(ef.Rules))
	for _, er := range ef.Rules {
		hdrs := strings.TrimSpace(er.HeadersTemplate)
		if hdrs == "" {
			hdrs = "{}"
		}
		rules = append(rules, &store.Rule{
			Name:            er.Name,
			Enabled:         er.Enabled,
			FromSourceIDs:   er.From,
			ToSourceIDs:     er.To,
			Filters:         er.Filters,
			BodyTemplate:    er.BodyTemplate,
			SubjectTemplate: er.SubjectTemplate,
			HeadersTemplate: hdrs,
		})
	}

	if err := s.store.ReplaceConfig(sources, rules); err != nil {
		s.fail(w, "导入失败", err)
		return
	}
	s.reloadMail()
	s.log.Info("导入配置完成", "源", len(sources), "规则", len(rules))
	http.Redirect(w, r, "/settings?ok=imported", http.StatusSeeOther)
}
