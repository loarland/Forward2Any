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
	// Turnstile 这一组跟代理一样，只在表单确实提交了开关时才动，
	// 免得别的部分提交把它关掉。
	if _, ok := r.PostForm["turnstile_enabled"]; ok {
		next.TurnstileEnabled = r.PostFormValue("turnstile_enabled") == "1"
		next.TurnstileSiteKey = strings.TrimSpace(formValue(r, "turnstile_site_key"))
		next.TurnstileSecret = strings.TrimSpace(formValue(r, "turnstile_secret"))
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
	// 开了人机校验就必须有密钥：只勾开关不填密钥会把登录挡死（前端控件渲染不出来）。
	if v.TurnstileEnabled {
		if v.TurnstileSiteKey == "" {
			return errors.New("开启 Turnstile 后必须填站点密钥（Site Key）")
		}
		if v.TurnstileSecret == "" {
			return errors.New("开启 Turnstile 后必须填密钥（Secret Key）")
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

// exportFile 刻意不含管理员账号与密码、监听端口、代理、时区、外观和 Turnstile 密钥：
// 那是本机部署的事，不该跟着配置走。设置里只有下面这几项跟着文件走。
type exportFile struct {
	Version    int            `json:"version"`
	ExportedAt int64          `json:"exported_at"`
	Settings   exportSettings `json:"settings"`
	Sources    []exportSource `json:"sources"`
	Rules      []exportRule   `json:"rules"`
}

// exportSettings 是跟着配置文件走的那几项设置。
//
// 用指针是为了区分「文件里没写」和「写了个 0」：导入只覆盖文件里确实带着的项，
// 手工删掉一项或老文件里没有的项，不会被压成零值。
type exportSettings struct {
	BaseURL             *string `json:"base_url,omitempty"`
	RetryMax            *int    `json:"retry_max,omitempty"`
	RetryBackoffSeconds *int    `json:"retry_backoff_seconds,omitempty"`
	PayloadMaxBytes     *int    `json:"payload_max_bytes,omitempty"`
	LogRetentionDays    *int    `json:"log_retention_days,omitempty"`
}

// exportSource 带一个下标，规则通过下标引用源，于是文件里不含数据库主键。
// 源自身的主键和时间戳导出前会清零（见 handleExport），不再出现在文件里。
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
		Settings: exportSettings{
			BaseURL:             &settings.BaseURL,
			RetryMax:            &settings.RetryMax,
			RetryBackoffSeconds: &settings.RetryBackoffSeconds,
			PayloadMaxBytes:     &settings.PayloadMaxBytes,
			LogRetentionDays:    &settings.LogRetentionDays,
		},
	}
	for i, src := range sources {
		idx[src.ID] = i
		// 拷一份再清掉主键与时间戳：文件是给别的机器用的，这几个值带过去没有意义
		//（导入时一律重新生成），留在文件里只会让人以为要手工对齐。
		exported := *src
		exported.ID = 0
		exported.CreatedAt = 0
		exported.UpdatedAt = 0
		ef.Sources = append(ef.Sources, exportSource{Index: i, Source: &exported})
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
	// 文件名里的时间戳按设置里选的时区来，跟页面上看到的时间一致。
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="forward2any-%s.json"`,
		time.Now().In(settings.Location()).Format("20060102-150405")))
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

	// 先把三样东西都验完再落库：任何一处不合法都当作没导入过。
	sources, err := importSources(ef.Sources)
	if err != nil {
		s.renderSettings(w, r, err.Error(), "")
		return
	}
	rules, err := importRules(ef.Rules, len(sources))
	if err != nil {
		s.renderSettings(w, r, err.Error(), "")
		return
	}
	kv, err := s.importSettings(ef.Settings)
	if err != nil {
		s.renderSettings(w, r, err.Error(), "")
		return
	}

	if err := s.store.ReplaceConfig(sources, rules, kv); err != nil {
		s.fail(w, "导入失败", err)
		return
	}
	s.reloadMail()
	// 回调基址可能跟着文件换了，跨站校验的允许列表要重新取一遍。
	if len(kv) > 0 {
		s.refreshOriginPolicy()
	}
	s.log.Info("导入配置完成", "源", len(sources), "规则", len(rules), "设置", len(kv))
	http.Redirect(w, r, "/settings?ok=imported", http.StatusSeeOther)
}

// importSources 把文件里的源按下标摆回原位。
//
// 规则里的 from/to 存的就是这些下标（不是数组顺序），所以必须按 index 就位 ——
// 手工调整过顺序的文件才不会把规则指到别的源上。
// 空位、重复下标、越界下标都不放过：静默跳过会让规则少一个源，比当场拒绝更难查。
// 「正好 0 到 n-1 各一次」是这份文件能自洽的前提：既保证每个下标都指得到源，
// 也保证规则不会指向一个被丢掉的源。
func importSources(list []exportSource) ([]*store.Source, error) {
	out := make([]*store.Source, len(list))
	seenSlug := map[string]bool{}
	for _, es := range list {
		if es.Index < 0 || es.Index >= len(list) {
			return nil, fmt.Errorf("源的下标 %d 越界：文件里一共 %d 个源，下标必须正好是 0 到 %d 各一次",
				es.Index, len(list), len(list)-1)
		}
		if es.Source == nil {
			return nil, fmt.Errorf("下标为 %d 的源没有内容", es.Index)
		}
		if out[es.Index] != nil {
			return nil, fmt.Errorf("源的下标 %d 出现了两次：文件里一共 %d 个源，下标必须正好是 0 到 %d 各一次",
				es.Index, len(list), len(list)-1)
		}
		src := *es.Source
		src.ID = 0
		// 导入是整体替换，所以只做「形态」校验，不跟库里现有数据比 slug。
		if err := validateSourceShape(&src); err != nil {
			return nil, fmt.Errorf("下标为 %d 的源（%s）不合法: %v", es.Index, src.Name, err)
		}
		if src.Slug != "" {
			if seenSlug[src.Slug] {
				return nil, fmt.Errorf("配置文件里有重复的路径标识 %q", src.Slug)
			}
			seenSlug[src.Slug] = true
		}
		out[es.Index] = &src
	}
	return out, nil
}

// importRules 校验文件里的规则，并把 from/to 的下标核一遍。
//
// 校验用的是界面保存时的同一套（validateRule）：手工改过的配置文件不该能塞进
// 一条界面上根本存不下的规则 —— 那种规则要么永远不触发，要么到投递时才炸。
func importRules(list []exportRule, nSources int) ([]*store.Rule, error) {
	out := make([]*store.Rule, 0, len(list))
	for i, er := range list {
		from, err := importRefs(er.From, nSources)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条规则（%s）的接收源 %v", i+1, er.Name, err)
		}
		to, err := importRefs(er.To, nSources)
		if err != nil {
			return nil, fmt.Errorf("第 %d 条规则（%s）的目标源 %v", i+1, er.Name, err)
		}
		rule := &store.Rule{
			Name:            strings.TrimSpace(er.Name),
			Enabled:         er.Enabled,
			FromSourceIDs:   from,
			ToSourceIDs:     to,
			Filters:         er.Filters,
			BodyTemplate:    strings.TrimSpace(er.BodyTemplate),
			SubjectTemplate: strings.TrimSpace(er.SubjectTemplate),
			HeadersTemplate: strings.TrimSpace(er.HeadersTemplate),
		}
		if err := validateRule(rule); err != nil {
			return nil, fmt.Errorf("第 %d 条规则不合法: %v", i+1, err)
		}
		out = append(out, rule)
	}
	return out, nil
}

// importRefs 检查一条规则里引用的源下标，越界就报错（不再像以前那样悄悄丢掉）。
func importRefs(idx []int64, nSources int) ([]int64, error) {
	out := make([]int64, 0, len(idx))
	for _, i := range idx {
		if i < 0 || int(i) >= nSources {
			return nil, fmt.Errorf("引用了不存在的源下标 %d（文件里有 %d 个源）", i, nSources)
		}
		out = append(out, i)
	}
	return out, nil
}

// importSettings 校验文件里的设置项，并返回要落库的键值。
//
// 只认文件里确实带着的项：导入文件允许只带一部分设置（老文件、手工删过的文件），
// 少一项不该把本机的配置清成零值 —— 没带的项就是不覆盖。
//
// 校验拿的是一份「默认值 + 文件里这几项」，而不是本机现有设置：
// 要拦的只是文件带来的值（比如回调基址没写协议、重试次数是 0），
// 本机别的字段哪怕是历史遗留的怪值，也不该连累这次导入。
func (s *Server) importSettings(es exportSettings) (map[string]string, error) {
	probe := store.DefaultSettings()
	kv := map[string]string{}
	if es.BaseURL != nil {
		probe.BaseURL = strings.TrimRight(strings.TrimSpace(*es.BaseURL), "/")
		kv[store.KeyBaseURL] = probe.BaseURL
	}
	if es.RetryMax != nil {
		probe.RetryMax = *es.RetryMax
		kv[store.KeyRetryMax] = strconv.Itoa(probe.RetryMax)
	}
	if es.RetryBackoffSeconds != nil {
		probe.RetryBackoffSeconds = *es.RetryBackoffSeconds
		kv[store.KeyRetryBackoffSeconds] = strconv.Itoa(probe.RetryBackoffSeconds)
	}
	if es.PayloadMaxBytes != nil {
		probe.PayloadMaxBytes = *es.PayloadMaxBytes
		kv[store.KeyPayloadMaxBytes] = strconv.Itoa(probe.PayloadMaxBytes)
	}
	if es.LogRetentionDays != nil {
		probe.LogRetentionDays = *es.LogRetentionDays
		kv[store.KeyLogRetentionDays] = strconv.Itoa(probe.LogRetentionDays)
	}
	if err := validateSettings(probe); err != nil {
		return nil, fmt.Errorf("配置文件里的设置项不合法: %v", err)
	}
	return kv, nil
}
