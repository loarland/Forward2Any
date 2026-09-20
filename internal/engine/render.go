package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode/utf8"

	"github.com/loarland/Forward2Any/internal/store"
)

// TemplateData 是规则模板里可用的数据。
type TemplateData struct {
	Payload any               // 解析后的 JSON；非 JSON 报文为 nil
	Raw     string            // 原始报文
	Source  map[string]any    // 接收源
	Headers map[string]string // 入站请求头
	TraceID string
	Now     time.Time
}

// view 组装模板实际看到的数据。
func (d *TemplateData) view() map[string]any {
	payload := d.Payload
	if payload == nil {
		// 不是 JSON 时给个保底结构，模板里 .Payload.raw 仍然可读。
		payload = map[string]any{"raw": d.Raw}
	}
	return map[string]any{
		"Payload": payload,
		"Raw":     d.Raw,
		"Source":  d.Source,
		"Headers": d.Headers,
		"TraceID": d.TraceID,
		"Now":     d.Now,
	}
}

func parse(name, text string) (*template.Template, error) {
	t, err := template.New(name).Option("missingkey=zero").Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s模板语法错误: %w", name, err)
	}
	return t, nil
}

// 过滤器里的路径支持 commits.0.id 这种数字下标（那是我们自己解析的），
// 但 Go 模板不支持，必须写 index。从过滤器那边顺过来的写法很容易踩这个坑。
var numericIndex = regexp.MustCompile(`\.[0-9]+`)

// ValidateTemplate 只检查模板语法，不执行它。
//
// 保存规则时不能拿一份假 payload 去执行模板：任何引用真实字段的写法
// （比如 .Payload.repository.full_name）都会因为假数据里没有这个字段而报
// "nil pointer evaluating interface {}"，用户明明写对了却被拒绝，
// 等于把模板功能废掉。执行期的取值错误只能等真实报文到了才知道，
// 那时由 Submit 记日志并跳过该目标，不影响其它投递。
func ValidateTemplate(name, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if _, err := parse(name, text); err != nil {
		if numericIndex.MatchString(text) {
			return fmt.Errorf("%w（提示：模板里的数组下标要写 {{index .Payload.commits 0}}，"+
				"不能像过滤器那样写 .commits.0）", err)
		}
		return err
	}
	return nil
}

// ValidateHeaderTemplates 校验请求头模板：必须是 JSON 对象，且每个值的模板语法正确。
func ValidateHeaderTemplates(tmplJSON string) error {
	trimmed := strings.TrimSpace(tmplJSON)
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	var raw map[string]string
	if err := json.Unmarshal([]byte(tmplJSON), &raw); err != nil {
		return fmt.Errorf(`请求头模板必须是 JSON 对象，例如 {"X-Token":"{{.Source.slug}}"}`)
	}
	for k, v := range raw {
		if err := ValidateTemplate("请求头 "+k, v); err != nil {
			return err
		}
	}
	return nil
}

// RenderBody 渲染出站报文体。模板为空表示原样透传入站报文。
func RenderBody(tmplText string, data *TemplateData) (string, error) {
	if strings.TrimSpace(tmplText) == "" {
		return data.Raw, nil
	}
	t, err := parse("报文", tmplText)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data.view()); err != nil {
		return "", fmt.Errorf("渲染报文模板失败: %w", err)
	}
	return buf.String(), nil
}

// RenderSubject 渲染邮件主题。模板为空返回空串，由调用方决定默认主题。
func RenderSubject(tmplText string, data *TemplateData) (string, error) {
	if strings.TrimSpace(tmplText) == "" {
		return "", nil
	}
	t, err := parse("主题", tmplText)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data.view()); err != nil {
		return "", fmt.Errorf("渲染主题模板失败: %w", err)
	}
	return strings.TrimSpace(buf.String()), nil
}

// RenderHeaders 渲染出站请求头。
// 规则上没配时退回源自身的固定请求头。
func RenderHeaders(tmplJSON string, data *TemplateData, fallback map[string]string) (map[string]string, error) {
	out := map[string]string{}
	trimmed := strings.TrimSpace(tmplJSON)
	if trimmed == "" || trimmed == "{}" {
		for k, v := range fallback {
			out[http.CanonicalHeaderKey(k)] = v
		}
		return out, nil
	}

	var raw map[string]string
	if err := json.Unmarshal([]byte(tmplJSON), &raw); err != nil {
		return nil, fmt.Errorf("请求头模板必须是 JSON 对象: %w", err)
	}
	for k, v := range raw {
		t, err := parse("请求头 "+k, v)
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, data.view()); err != nil {
			return nil, fmt.Errorf("渲染请求头 %s 失败: %w", k, err)
		}
		out[http.CanonicalHeaderKey(k)] = buf.String()
	}
	return out, nil
}

// DefaultSubject 在规则没配主题模板时给出一个还能看的主题。
func DefaultSubject(in *store.Source, data *TemplateData) string {
	prefix := "[" + in.Name + "]"
	if m, ok := data.Payload.(map[string]any); ok {
		if s, ok := m["subject"]; ok {
			if s := strings.TrimSpace(fmt.Sprint(s)); s != "" {
				return prefix + " " + s
			}
		}
	}
	if raw := strings.TrimSpace(data.Raw); raw != "" {
		line := raw
		if i := strings.IndexAny(line, "\r\n"); i >= 0 {
			line = line[:i]
		}
		return prefix + " " + truncateRunes(line, 80)
	}
	return prefix + " Forward2Any 转发"
}

// Truncate 按字节上限截断，且不切碎多字节字符。
func Truncate(s string, maxBytes int) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "\n…（内容过长，已截断）"
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// SourceView 是模板里 .Source 的内容，只暴露非敏感字段。
func SourceView(s *store.Source) map[string]any {
	return map[string]any{
		"id":   s.ID,
		"name": s.Name,
		"slug": s.Slug,
		"kind": s.Kind,
	}
}

// SourceHeaders 解析源上配置的固定请求头。
func SourceHeaders(s *store.Source) map[string]string {
	out := map[string]string{}
	if strings.TrimSpace(s.Headers) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s.Headers), &out)
	return out
}
