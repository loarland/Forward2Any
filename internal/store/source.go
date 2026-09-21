package store

import (
	"fmt"
	"regexp"
	"strings"
)

// Source 是一个「源」：webhook 或 email。
// 同一个源既可作接收方也可作发送方，取决于规则里把它放在哪一侧；
// Usage 只是用来决定表单显示哪些字段。
type Source struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Usage      string `json:"usage"`
	Enabled    bool   `json:"enabled"`
	Slug       string `json:"slug"`
	URL        string `json:"url"`
	HTTPMethod string `json:"http_method"`
	Headers    string `json:"headers"`
	AuthMode   string `json:"auth_mode"`
	AuthHeader string `json:"auth_header"`
	AuthSecret string `json:"auth_secret"`
	IPAllow    string `json:"ip_allow"`

	// UseProxy 表示这个源发送时走设置里配的代理。
	// 只有用途包含发送的源才认这个值，其它情况下引擎会忽略它。
	UseProxy bool `json:"use_proxy"`

	// Telegram 发送源。TgEndpoint 留空表示用官方地址，填了就当自建 Bot API 服务器用。
	TgToken    string `json:"tg_token"`
	TgChatID   string `json:"tg_chat_id"`
	TgThreadID string `json:"tg_thread_id"`
	TgEndpoint string `json:"tg_endpoint"`

	// 内置渠道发送源（钉钉 / 企业微信 / 飞书 / Bark / Server酱 / WxPusher / Gotify / OneBot）。
	// 渠道地址放 URL；ChannelSecret 是加签密钥（钉钉、飞书可选），
	// ChannelTarget 是目标 ID（OneBot 的 QQ 号或群号；Bark 的群组名）。
	ChannelSecret string `json:"channel_secret"`
	ChannelTarget string `json:"channel_target"`

	// 接收源上的默认模板：规则里对应模板留空时用这两份。
	DefaultBodyTemplate    string `json:"default_body_template"`
	DefaultSubjectTemplate string `json:"default_subject_template"`

	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	SMTPUser string `json:"smtp_user"`
	SMTPPass string `json:"smtp_pass"`
	SMTPTLS  string `json:"smtp_tls"`
	MailFrom string `json:"mail_from"`
	MailTo   string `json:"mail_to"`

	IMAPHost     string `json:"imap_host"`
	IMAPPort     int    `json:"imap_port"`
	IMAPUser     string `json:"imap_user"`
	IMAPPass     string `json:"imap_pass"`
	IMAPTLS      bool   `json:"imap_tls"`
	IMAPFolder   string `json:"imap_folder"`
	IMAPInterval int    `json:"imap_interval"`

	CreatedAt int64 `json:"created_at"`
	UpdatedAt int64 `json:"updated_at"`
}

// DefaultTgEndpoint 是 Telegram Bot API 的默认前缀，token 会拼在它后面。
// 自建 Bot API 服务器（telegram-bot-api）时可以改成自己的地址，结尾照样带 /bot。
const DefaultTgEndpoint = "https://api.telegram.org/bot"

// ChannelKinds 是内置渠道发送源的类型。
//
// 这些渠道没有「接收」这一侧：它们是各家的群机器人 / 推送服务的发送接口，
// 只能把消息发出去。想要「自定义」就继续用 webhook 类型。
var ChannelKinds = []string{"dingtalk", "wecom", "feishu", "bark", "serverchan", "wxpusher", "gotify", "onebot"}

// IsChannelKind 报告 kind 是不是内置渠道。
func IsChannelKind(kind string) bool {
	for _, k := range ChannelKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// IsSendOnlyKind 报告这个类型是不是只能发送（Telegram 和所有内置渠道）。
func IsSendOnlyKind(kind string) bool {
	return kind == "telegram" || IsChannelKind(kind)
}

// KindLabel 是类型在界面上的名字。
func KindLabel(kind string) string {
	switch kind {
	case "webhook":
		return "Webhook"
	case "email":
		return "邮件"
	case "telegram":
		return "Telegram"
	case "dingtalk":
		return "钉钉"
	case "wecom":
		return "企业微信"
	case "feishu":
		return "飞书"
	case "bark":
		return "Bark"
	case "serverchan":
		return "Server酱"
	case "wxpusher":
		return "WxPusher"
	case "gotify":
		return "Gotify"
	case "onebot":
		return "OneBot"
	}
	return kind
}

// AllKinds 是「类型」下拉里的全部类型，顺序也就是界面上的顺序：
// 先是可以收发的通用类型，再是只能发送的渠道。
var AllKinds = append([]string{"webhook", "email", "telegram"}, ChannelKinds...)

// CanReceive 报告这个源能不能接收。
// Telegram 和内置渠道只能往外发，所以永远为假。
func (s *Source) CanReceive() bool {
	if IsSendOnlyKind(s.Kind) {
		return false
	}
	return s.Usage == "in" || s.Usage == "both"
}

func (s *Source) CanSend() bool { return s.Usage == "out" || s.Usage == "both" }

// PollsMail 表示这个源需要 IMAP 轮询线程。
func (s *Source) PollsMail() bool {
	return s.Enabled && s.Kind == "email" && s.CanReceive() && s.IMAPHost != ""
}

const sourceCols = `id, name, kind, usage, enabled, slug, url, http_method, headers,
	auth_mode, auth_header, auth_secret, ip_allow, use_proxy,
	tg_token, tg_chat_id, tg_thread_id, tg_endpoint,
	channel_secret, channel_target,
	default_body_template, default_subject_template,
	smtp_host, smtp_port, smtp_user, smtp_pass, smtp_tls, mail_from, mail_to,
	imap_host, imap_port, imap_user, imap_pass, imap_tls, imap_folder, imap_interval,
	created_at, updated_at`

func scanSource(sc interface{ Scan(...any) error }) (*Source, error) {
	var v Source
	err := sc.Scan(
		&v.ID, &v.Name, &v.Kind, &v.Usage, &v.Enabled, &v.Slug, &v.URL, &v.HTTPMethod, &v.Headers,
		&v.AuthMode, &v.AuthHeader, &v.AuthSecret, &v.IPAllow, &v.UseProxy,
		&v.TgToken, &v.TgChatID, &v.TgThreadID, &v.TgEndpoint,
		&v.ChannelSecret, &v.ChannelTarget,
		&v.DefaultBodyTemplate, &v.DefaultSubjectTemplate,
		&v.SMTPHost, &v.SMTPPort, &v.SMTPUser, &v.SMTPPass, &v.SMTPTLS, &v.MailFrom, &v.MailTo,
		&v.IMAPHost, &v.IMAPPort, &v.IMAPUser, &v.IMAPPass, &v.IMAPTLS, &v.IMAPFolder, &v.IMAPInterval,
		&v.CreatedAt, &v.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) ListSources() ([]*Source, error) {
	rows, err := s.db.Query(`SELECT ` + sourceCols + ` FROM sources ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("列出源: %w", err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		v, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetSource(id int64) (*Source, error) {
	v, err := scanSource(s.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE id = ?`, id))
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取源 %d: %w", id, err)
	}
	return v, nil
}

func (s *Store) SourceBySlug(slug string) (*Source, error) {
	v, err := scanSource(s.db.QueryRow(`SELECT `+sourceCols+` FROM sources WHERE slug = ?`, slug))
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("按 slug 读取源: %w", err)
	}
	return v, nil
}

// MailReceivingSources 返回所有需要 IMAP 轮询的源。
func (s *Store) MailReceivingSources() ([]*Source, error) {
	rows, err := s.db.Query(`SELECT ` + sourceCols + ` FROM sources
		WHERE enabled = 1 AND kind = 'email' AND usage IN ('in','both') AND imap_host <> ''
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("列出邮件接收源: %w", err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		v, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) SaveSource(v *Source) error { return saveSource(s.db, v) }

func saveSource(db execer, v *Source) error {
	v.UpdatedAt = nowUnix()
	if v.ID == 0 {
		v.CreatedAt = v.UpdatedAt
		res, err := db.Exec(`INSERT INTO sources (
			name, kind, usage, enabled, slug, url, http_method, headers,
			auth_mode, auth_header, auth_secret, ip_allow, use_proxy,
			tg_token, tg_chat_id, tg_thread_id, tg_endpoint,
			channel_secret, channel_target,
			default_body_template, default_subject_template,
			smtp_host, smtp_port, smtp_user, smtp_pass, smtp_tls, mail_from, mail_to,
			imap_host, imap_port, imap_user, imap_pass, imap_tls, imap_folder, imap_interval,
			created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			v.Name, v.Kind, v.Usage, v.Enabled, v.Slug, v.URL, v.HTTPMethod, v.Headers,
			v.AuthMode, v.AuthHeader, v.AuthSecret, v.IPAllow, v.UseProxy,
			v.TgToken, v.TgChatID, v.TgThreadID, v.TgEndpoint,
			v.ChannelSecret, v.ChannelTarget,
			v.DefaultBodyTemplate, v.DefaultSubjectTemplate,
			v.SMTPHost, v.SMTPPort, v.SMTPUser, v.SMTPPass, v.SMTPTLS, v.MailFrom, v.MailTo,
			v.IMAPHost, v.IMAPPort, v.IMAPUser, v.IMAPPass, v.IMAPTLS, v.IMAPFolder, v.IMAPInterval,
			v.CreatedAt, v.UpdatedAt,
		)
		if err != nil {
			return fmt.Errorf("新建源: %w", err)
		}
		v.ID, err = res.LastInsertId()
		return err
	}

	_, err := db.Exec(`UPDATE sources SET
		name=?, kind=?, usage=?, enabled=?, slug=?, url=?, http_method=?, headers=?,
		auth_mode=?, auth_header=?, auth_secret=?, ip_allow=?, use_proxy=?,
		tg_token=?, tg_chat_id=?, tg_thread_id=?, tg_endpoint=?,
		channel_secret=?, channel_target=?,
		default_body_template=?, default_subject_template=?,
		smtp_host=?, smtp_port=?, smtp_user=?, smtp_pass=?, smtp_tls=?, mail_from=?, mail_to=?,
		imap_host=?, imap_port=?, imap_user=?, imap_pass=?, imap_tls=?, imap_folder=?, imap_interval=?,
		updated_at=?
		WHERE id=?`,
		v.Name, v.Kind, v.Usage, v.Enabled, v.Slug, v.URL, v.HTTPMethod, v.Headers,
		v.AuthMode, v.AuthHeader, v.AuthSecret, v.IPAllow, v.UseProxy,
		v.TgToken, v.TgChatID, v.TgThreadID, v.TgEndpoint,
		v.ChannelSecret, v.ChannelTarget,
		v.DefaultBodyTemplate, v.DefaultSubjectTemplate,
		v.SMTPHost, v.SMTPPort, v.SMTPUser, v.SMTPPass, v.SMTPTLS, v.MailFrom, v.MailTo,
		v.IMAPHost, v.IMAPPort, v.IMAPUser, v.IMAPPass, v.IMAPTLS, v.IMAPFolder, v.IMAPInterval,
		v.UpdatedAt, v.ID,
	)
	if err != nil {
		return fmt.Errorf("更新源 %d: %w", v.ID, err)
	}
	return nil
}

func (s *Store) DeleteSource(id int64) error {
	_, err := s.db.Exec(`DELETE FROM sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("删除源 %d: %w", id, err)
	}
	// 规则里残留的源 id 不会造成转发，引擎查不到源就跳过；这里顺手清掉更干净。
	return s.pruneSourceFromRules(id)
}

var slugStrip = regexp.MustCompile(`[^a-z0-9]+`)

// SuggestSlug 由名称生成 URL 友好的 slug。
// 额外拼 4 个随机十六进制字符：路径本身不是密钥，但不该被轻易猜到。
func SuggestSlug(name string) string {
	base := slugStrip.ReplaceAllString(strings.ToLower(name), "-")
	base = strings.Trim(base, "-")
	if base == "" {
		base = "hook"
	}
	if len(base) > 32 {
		base = strings.Trim(base[:32], "-")
	}
	suffix, err := RandomHex(2)
	if err != nil {
		return base
	}
	return base + "-" + suffix
}
