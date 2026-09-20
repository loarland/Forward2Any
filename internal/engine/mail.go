package engine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/wneessen/go-mail"

	"github.com/loarland/Webhook2Any/internal/store"
)

// sendMail 通过 SMTP 发出一次投递。
func (e *Engine) sendMail(d *store.Delivery, out *store.Source) error {
	if strings.TrimSpace(out.SMTPHost) == "" {
		return errors.New("未配置 SMTP 服务器")
	}
	if strings.TrimSpace(out.MailFrom) == "" {
		return errors.New("未配置发件人")
	}
	to := SplitList(out.MailTo)
	if len(to) == 0 {
		return errors.New("未配置收件人")
	}

	msg := mail.NewMsg()
	if err := msg.From(out.MailFrom); err != nil {
		return fmt.Errorf("发件人 %q 无效: %w", out.MailFrom, err)
	}
	if err := msg.To(to...); err != nil {
		return fmt.Errorf("收件人无效: %w", err)
	}
	subject := d.Subject
	if subject == "" {
		subject = "[Webhook2Any] 转发消息"
	}
	msg.Subject(subject)
	msg.SetBodyString(mail.TypeTextPlain, d.Rendered)

	// 把跳链写进邮件头，收件方若也是 Webhook2Any（或本实例自己在轮询）就能断环。
	if d.HopChain != "" {
		msg.SetHeader("X-W2A-Hops", d.HopChain)
	}
	if d.TraceID != "" {
		msg.SetHeader("X-W2A-Trace", d.TraceID)
	}

	port := out.SMTPPort
	if port <= 0 {
		port = 587
	}
	opts := []mail.Option{mail.WithPort(port)}

	switch out.SMTPTLS {
	case "tls":
		// 465：连上就是 TLS
		opts = append(opts, mail.WithSSL())
	case "none":
		opts = append(opts, mail.WithTLSPolicy(mail.NoTLS))
	default:
		// 587：STARTTLS
		opts = append(opts, mail.WithTLSPolicy(mail.TLSMandatory))
	}

	if out.SMTPUser != "" {
		opts = append(opts,
			mail.WithSMTPAuth(mail.SMTPAuthPlain),
			mail.WithUsername(out.SMTPUser),
			mail.WithPassword(out.SMTPPass),
		)
	}

	c, err := mail.NewClient(out.SMTPHost, opts...)
	if err != nil {
		return fmt.Errorf("构造 SMTP 客户端失败: %w", err)
	}
	if err := c.DialAndSend(msg); err != nil {
		return fmt.Errorf("发送邮件失败: %w", err)
	}
	return nil
}

// SplitList 解析逗号分隔的地址列表。
func SplitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
