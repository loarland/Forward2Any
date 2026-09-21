package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/loarland/Forward2Any/internal/store"
)

func (s *Server) registerSources(mux *http.ServeMux) {
	mux.HandleFunc("GET /sources", s.handleSourceList)
	mux.HandleFunc("GET /sources/new", s.handleSourceForm)
	mux.HandleFunc("POST /sources", s.handleSourceCreate)
	mux.HandleFunc("GET /sources/{id}/edit", s.handleSourceForm)
	mux.HandleFunc("POST /sources/{id}", s.handleSourceUpdate)
	mux.HandleFunc("POST /sources/{id}/delete", s.handleSourceDelete)
	mux.HandleFunc("POST /sources/{id}/test", s.handleSourceTest)
}

// sourceRow 是列表页一行：源本身 + 自动生成的回调地址与调用示例。
type sourceRow struct {
	Src     *store.Source
	HookURL string
	Curl    string
}

func (s *Server) handleSourceList(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSources()
	if err != nil {
		s.fail(w, "读取源列表失败", err)
		return
	}
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, "读取设置失败", err)
		return
	}

	rows := make([]sourceRow, 0, len(list))
	for _, src := range list {
		rows = append(rows, sourceRow{
			Src:     src,
			HookURL: hookURL(settings.BaseURL, src),
			Curl:    curlExample(settings.BaseURL, src),
		})
	}
	s.render(w, r, "sources", map[string]any{
		"Title": "源",
		"Nav":   "sources",
		"Rows":  rows,
		"Base":  settings.BaseURL,
	})
}

func (s *Server) handleSourceForm(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == "" {
		s.renderSourceForm(w, r, &store.Source{
			Kind: "webhook", Usage: "in", Enabled: true,
			HTTPMethod: "POST", Headers: "{}", AuthMode: "none",
			TgEndpoint: store.DefaultTgEndpoint,
			SMTPPort:   587, SMTPTLS: "starttls",
			IMAPPort: 993, IMAPTLS: true, IMAPFolder: "INBOX", IMAPInterval: 60,
		}, "")
		return
	}

	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	src, err := s.store.GetSource(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "读取源失败", err)
		return
	}
	s.renderSourceForm(w, r, src, "")
}

func (s *Server) renderSourceForm(w http.ResponseWriter, r *http.Request, src *store.Source, errMsg string) {
	settings, err := s.store.Settings()
	if err != nil {
		s.fail(w, "读取设置失败", err)
		return
	}
	s.render(w, r, "source_form", map[string]any{
		"Title": "源",
		"Nav":   "sources",
		"Src":   src,
		"IsNew": src.ID == 0,
		"Error": errMsg,
		// 空串表示设置里还没配代理，前端据此把勾选框禁用掉。
		"Proxy": settings.ProxyURL(),
	})
}

func (s *Server) handleSourceCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	src, err := s.sourceFromForm(r, &store.Source{})
	if err == nil {
		err = s.store.SaveSource(src)
	}
	if err != nil {
		s.renderSourceForm(w, r, src, err.Error())
		return
	}
	s.reloadMail()
	http.Redirect(w, r, "/sources?ok=saved", http.StatusSeeOther)
}

func (s *Server) handleSourceUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	existing, err := s.store.GetSource(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "读取源失败", err)
		return
	}

	src, err := s.sourceFromForm(r, existing)
	src.ID = id
	if err == nil {
		err = s.store.SaveSource(src)
	}
	if err != nil {
		s.renderSourceForm(w, r, src, err.Error())
		return
	}
	s.log.Info("更新源", "源", src.Name, "id", src.ID)
	s.reloadMail()
	http.Redirect(w, r, "/sources?ok=saved", http.StatusSeeOther)
}

func (s *Server) handleSourceDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteSource(id); err != nil {
		s.fail(w, "删除源失败", err)
		return
	}
	s.log.Info("删除源", "id", id)
	s.reloadMail()
	http.Redirect(w, r, "/sources?ok=deleted", http.StatusSeeOther)
}

// handleSourceTest 给单个目标源投一条测试消息，走的是和真实转发完全一样的链路。
func (s *Server) handleSourceTest(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	src, err := s.store.GetSource(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "读取源失败", err)
		return
	}
	if !src.CanSend() {
		http.Error(w, "该源的用途不包含「发送」，无法测试", http.StatusBadRequest)
		return
	}

	// 测试消息也分两种：自定义 Webhook 发的是那串 JSON，内置渠道发的是给人看的一句话
	// （渠道会把正文包进自己的信封里，塞一坨 JSON 进去只会收到一条看不懂的通知）。
	body := `{"test":true,"from":"Forward2Any","message":"这是一条测试消息"}`
	if store.IsChannelKind(src.Kind) {
		body = "这是一条来自 Forward2Any 的测试消息。\n\n如果你在群里看到了它，说明这个渠道配置是对的。"
	}
	if err := s.engine.PostTest(src, body); err != nil {
		s.fail(w, "入队测试消息失败", err)
		return
	}
	http.Redirect(w, r, "/sources?ok=tested", http.StatusSeeOther)
}

// ---------- 表单 -> 模型 ----------

func (s *Server) sourceFromForm(r *http.Request, base *store.Source) (*store.Source, error) {
	v := *base
	v.Name = formValue(r, "name")
	v.Kind = formValue(r, "kind")
	v.Usage = formValue(r, "usage")
	v.Enabled = formBool(r, "enabled")
	v.Slug = formValue(r, "slug")
	v.URL = formValue(r, "url")
	v.HTTPMethod = strings.ToUpper(formValue(r, "http_method"))
	v.Headers = formValue(r, "headers")
	v.AuthMode = formValue(r, "auth_mode")
	v.AuthHeader = formValue(r, "auth_header")
	v.AuthSecret = formValue(r, "auth_secret")
	v.IPAllow = formValue(r, "ip_allow")

	// 「走代理发送」只在能发送的非邮件类型上认（表单上也只有那时才显示）：
	// Webhook、Telegram 和内置渠道都走 HTTP，邮件走 SMTP，代理对它没意义。
	// 其它情况一律沿用原值：用途改成纯接收不该把这个勾悄悄清掉，
	// 改回发送时它还在这儿。
	if v.Kind != "email" && v.CanSend() {
		v.UseProxy = formBool(r, "use_proxy")
	} else {
		v.UseProxy = base.UseProxy
	}

	v.TgToken = formValue(r, "tg_token")
	v.TgChatID = formValue(r, "tg_chat_id")
	v.TgThreadID = formValue(r, "tg_thread_id")
	v.TgEndpoint = formValue(r, "tg_endpoint")

	v.ChannelSecret = formValue(r, "channel_secret")
	v.ChannelTarget = formValue(r, "channel_target")

	v.SMTPHost = formValue(r, "smtp_host")
	v.SMTPPort = formInt(r, "smtp_port", 587)
	v.SMTPUser = formValue(r, "smtp_user")
	v.SMTPPass = formValue(r, "smtp_pass")
	v.SMTPTLS = formValue(r, "smtp_tls")
	v.MailFrom = formValue(r, "mail_from")
	v.MailTo = formValue(r, "mail_to")

	v.IMAPHost = formValue(r, "imap_host")
	v.IMAPPort = formInt(r, "imap_port", 993)
	v.IMAPUser = formValue(r, "imap_user")
	v.IMAPPass = formValue(r, "imap_pass")
	v.IMAPTLS = formBool(r, "imap_tls")
	v.IMAPFolder = formValue(r, "imap_folder")
	v.IMAPInterval = formInt(r, "imap_interval", 60)

	if err := s.validateSource(&v); err != nil {
		return &v, err
	}
	return &v, nil
}

func (s *Server) validateSource(v *store.Source) error {
	if err := validateSourceShape(v); err != nil {
		return err
	}
	if v.Kind == "webhook" && v.Slug != "" {
		other, err := s.store.SourceBySlug(v.Slug)
		if err == nil && other.ID != v.ID {
			return fmt.Errorf("路径标识 %q 已被源「%s」占用", v.Slug, other.Name)
		}
	}
	return nil
}

// kindListText 是错误信息里那个「类型只能是 …」的列表。
func kindListText() string {
	names := make([]string, 0, len(store.AllKinds))
	for _, k := range store.AllKinds {
		names = append(names, store.KindLabel(k))
	}
	return strings.Join(names, "、")
}

// validateSourceShape 只校验内容本身，不查库 —— 导入时库里还是空的，没法比对。
func validateSourceShape(v *store.Source) error {
	if v.Name == "" {
		return errors.New("名称不能为空")
	}
	if v.Kind != "webhook" && v.Kind != "email" && !store.IsSendOnlyKind(v.Kind) {
		return fmt.Errorf("类型只能是 %s", kindListText())
	}
	if v.Usage != "in" && v.Usage != "out" && v.Usage != "both" {
		return errors.New("用途不合法")
	}

	if strings.TrimSpace(v.Headers) == "" {
		v.Headers = "{}"
	}
	var hdrs map[string]string
	if err := json.Unmarshal([]byte(v.Headers), &hdrs); err != nil {
		return errors.New(`附加请求头必须是 JSON 对象，例如 {"X-Token":"abc"}`)
	}
	if err := validateIPAllow(v.IPAllow); err != nil {
		return err
	}

	if v.Kind == "webhook" {
		if v.HTTPMethod == "" {
			v.HTTPMethod = "POST"
		}
		if v.CanReceive() && v.Slug == "" {
			// 自动分配路径标识，省得用户自己想名字。
			v.Slug = store.SuggestSlug(v.Name)
		}
		if v.CanSend() && v.URL == "" {
			return errors.New("作为发送方的 webhook 必须填写目标地址")
		}
		if v.Slug != "" && !isURLSafeSlug(v.Slug) {
			return errors.New("路径标识只能包含小写字母、数字、连字符和下划线")
		}
	}

	if v.Kind == "email" && v.CanSend() {
		if v.SMTPHost == "" {
			return errors.New("作为发送方的邮件源必须填写 SMTP 服务器")
		}
		if v.MailFrom == "" {
			return errors.New("必须填写发件人")
		}
		if v.MailTo == "" {
			return errors.New("必须填写收件人")
		}
		if v.SMTPTLS == "" {
			v.SMTPTLS = "starttls"
		}
	}

	// Telegram 只能往外发，所以用途必须是「发送」。
	// 允许存成接收用途的话，它会出现在规则表单的接收源栏里，但那条路根本走不通。
	if v.Kind == "telegram" {
		if v.Usage != "out" {
			return errors.New("Telegram 类型只能用作发送源，用途请选「发送」")
		}
		if v.TgToken == "" {
			return errors.New("必须填写 Bot Token")
		}
		if v.TgChatID == "" {
			return errors.New("必须填写 Chat ID")
		}
		if v.TgEndpoint == "" {
			v.TgEndpoint = store.DefaultTgEndpoint
		}
		// 端点后面会直接拼上 token（官方就是 /bot<token>/sendMessage），
		// 所以这里必须要求以 /bot 结尾 —— 少了它只会换来一个看不懂的 404。
		if u, err := url.Parse(v.TgEndpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("请求端点 %q 不合法，应形如 %s", v.TgEndpoint, store.DefaultTgEndpoint)
		}
		if !strings.HasSuffix(strings.TrimRight(v.TgEndpoint, "/"), "/bot") {
			return fmt.Errorf("请求端点要以 /bot 结尾（token 会直接拼在它后面），例如 %s", store.DefaultTgEndpoint)
		}
		// 话题 ID 留空表示不指定；填了就必须是数字，别等投递时才发现。
		if v.TgThreadID != "" {
			if _, err := strconv.ParseInt(v.TgThreadID, 10, 64); err != nil {
				return fmt.Errorf("话题 ID %q 不是数字", v.TgThreadID)
			}
		}
	}

	// 内置渠道：地址是唯一的必填项（凭据就藏在里面），个别渠道还要目标 ID。
	// 报文格式由程序按类型拼，用户不用管 —— 规则里的模板这时是「消息正文」。
	if store.IsChannelKind(v.Kind) {
		label := store.KindLabel(v.Kind)
		if v.Usage != "out" {
			return fmt.Errorf("「%s」类型只能用作发送源，用途请选「发送」", label)
		}
		v.URL = strings.TrimSpace(v.URL)
		if v.URL == "" {
			return fmt.Errorf("必须填写「%s」的推送地址", label)
		}
		u, err := url.Parse(v.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("推送地址 %q 不合法，要写成完整的 http(s) 地址", v.URL)
		}
		switch v.Kind {
		case "onebot":
			if strings.TrimSpace(v.ChannelTarget) == "" {
				return errors.New("OneBot 要填目标 ID：私聊写 QQ 号，群聊写群号")
			}
		case "wxpusher":
			// 极简推送的地址里带着 SPT，投递时要把它抽出来重新打到 simple-push 接口上。
			if !strings.Contains(v.URL, "/message/SPT_") {
				return errors.New("WxPusher 要填带 SPT 的推送地址，形如 " +
					"https://wxpusher.zjiecode.com/api/send/message/SPT_xxxxxx")
			}
		}
	}

	if v.Kind == "email" && v.CanReceive() {
		if v.IMAPHost == "" {
			return errors.New("作为接收方的邮件源必须填写 IMAP 服务器")
		}
		if v.IMAPUser == "" {
			return errors.New("必须填写 IMAP 用户名")
		}
		if v.IMAPFolder == "" {
			v.IMAPFolder = "INBOX"
		}
		if v.IMAPInterval < 10 {
			// 轮询太频繁对邮件服务器不礼貌，也容易被封。
			v.IMAPInterval = 60
		}
	}

	return nil
}

func validateIPAllow(s string) error {
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			if _, _, err := net.ParseCIDR(part); err != nil {
				return fmt.Errorf("网段 %q 不合法，应形如 10.0.0.0/8", part)
			}
			continue
		}
		if net.ParseIP(part) == nil {
			return fmt.Errorf("IP %q 不合法", part)
		}
	}
	return nil
}

func isURLSafeSlug(s string) bool {
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// ---------- 展示辅助 ----------

func hookURL(base string, src *store.Source) string {
	if src.Kind != "webhook" || src.Slug == "" {
		return ""
	}
	return strings.TrimRight(base, "/") + "/hook/" + src.Slug
}

func curlExample(base string, src *store.Source) string {
	u := hookURL(base, src)
	if u == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "curl -X POST '%s'", u)
	b.WriteString(" \\\n  -H 'Content-Type: application/json'")
	switch src.AuthMode {
	case "token":
		name := src.AuthHeader
		if name == "" {
			name = "X-F2A-Token"
		}
		fmt.Fprintf(&b, " \\\n  -H '%s: %s'", name, src.AuthSecret)
	case "hmac_sha256":
		b.WriteString(" \\\n  -H 'X-Hub-Signature-256: sha256=<正文的 HMAC-SHA256 十六进制>'")
	case "basic":
		fmt.Fprintf(&b, " \\\n  -u '%s:%s'", src.AuthHeader, src.AuthSecret)
	}
	b.WriteString(` \
  -d '{"hello":"world"}'`)
	return b.String()
}

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}
