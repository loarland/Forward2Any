package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/loarland/Forward2Any/internal/engine"
	"github.com/loarland/Forward2Any/internal/store"
)

func (s *Server) registerRules(mux *http.ServeMux) {
	mux.HandleFunc("GET /rules", s.handleRuleList)
	mux.HandleFunc("GET /rules/new", s.handleRuleForm)
	mux.HandleFunc("POST /rules", s.handleRuleCreate)
	mux.HandleFunc("GET /rules/{id}/edit", s.handleRuleForm)
	mux.HandleFunc("POST /rules/{id}", s.handleRuleUpdate)
	mux.HandleFunc("POST /rules/{id}/delete", s.handleRuleDelete)
}

type ruleRow struct {
	Rule *store.Rule
	From []string
	To   []string
}

func (s *Server) handleRuleList(w http.ResponseWriter, r *http.Request) {
	rules, err := s.store.ListRules()
	if err != nil {
		s.fail(w, "读取规则列表失败", err)
		return
	}
	sources, err := s.store.ListSources()
	if err != nil {
		s.fail(w, "读取源列表失败", err)
		return
	}
	byID := make(map[int64]string, len(sources))
	for _, src := range sources {
		byID[src.ID] = src.Name
	}

	rows := make([]ruleRow, 0, len(rules))
	for _, rule := range rules {
		rows = append(rows, ruleRow{
			Rule: rule,
			From: namesOf(rule.FromSourceIDs, byID),
			To:   namesOf(rule.ToSourceIDs, byID),
		})
	}
	s.render(w, r, "rules", map[string]any{
		"Title":    "规则",
		"Nav":      "rules",
		"Rows":     rows,
		"NoSource": len(sources) == 0,
	})
}

func (s *Server) handleRuleForm(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == "" {
		s.renderRuleForm(w, r, &store.Rule{Enabled: true}, "")
		return
	}
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rule, err := s.store.GetRule(id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, "读取规则失败", err)
		return
	}
	s.renderRuleForm(w, r, rule, "")
}

func (s *Server) renderRuleForm(w http.ResponseWriter, r *http.Request, rule *store.Rule, errMsg string) {
	sources, err := s.store.ListSources()
	if err != nil {
		s.fail(w, "读取源列表失败", err)
		return
	}
	s.render(w, r, "rule_form", map[string]any{
		"Title":      "规则",
		"Nav":        "rules",
		"Rule":       rule,
		"IsNew":      rule.ID == 0,
		"Sources":    sources,
		"FromSet":    idSet(rule.FromSourceIDs),
		"ToSet":      idSet(rule.ToSourceIDs),
		"FilterText": FiltersToText(rule.Filters),
		"Error":      errMsg,
	})
}

func (s *Server) handleRuleCreate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	rule, err := s.ruleFromForm(r)
	if err == nil {
		err = s.store.SaveRule(rule)
	}
	if err != nil {
		s.renderRuleForm(w, r, rule, err.Error())
		return
	}
	http.Redirect(w, r, "/rules?ok=saved", http.StatusSeeOther)
}

func (s *Server) handleRuleUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	if _, err := s.store.GetRule(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		s.fail(w, "读取规则失败", err)
		return
	}

	rule, err := s.ruleFromForm(r)
	rule.ID = id
	if err == nil {
		err = s.store.SaveRule(rule)
	}
	if err != nil {
		s.renderRuleForm(w, r, rule, err.Error())
		return
	}
	s.log.Info("更新规则", "规则", rule.Name, "id", id)
	http.Redirect(w, r, "/rules?ok=saved", http.StatusSeeOther)
}

func (s *Server) handleRuleDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.store.DeleteRule(id); err != nil {
		s.fail(w, "删除规则失败", err)
		return
	}
	http.Redirect(w, r, "/rules?ok=deleted", http.StatusSeeOther)
}

// ---------- 过滤器文本 <-> 结构 ----------

// 过滤器用一行一条的文本编辑，比动态表单行更好写也更好复制：
//
//	action eq push
//	repository.private eq false
//	exists ref
var validFilterOps = map[string]bool{
	"eq": true, "ne": true, "gt": true, "lt": true,
	"contains": true, "not_contains": true,
	"exists": true, "not_exists": true,
	"regex": true, "in": true,
}

// ParseFilters 解析过滤器文本。空行与 # 开头的注释会被忽略。
func ParseFilters(text string) ([]store.Filter, error) {
	var out []store.Filter
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, fmt.Errorf("过滤器第 %d 行格式不对，应为「路径 操作符 [值]」", i+1)
		}
		f := store.Filter{Path: fields[0], Op: fields[1]}
		if !validFilterOps[f.Op] {
			return nil, fmt.Errorf("过滤器第 %d 行的操作符 %q 不认识", i+1, f.Op)
		}
		if len(fields) > 2 {
			// ponytail: 值里的连续空格会被合并成一个。真需要保留时再换成分隔符语法。
			f.Value = engine.ParseFilterValue(strings.Join(fields[2:], " "))
		}
		if f.Op != "exists" && f.Op != "not_exists" && len(fields) < 3 {
			return nil, fmt.Errorf("过滤器第 %d 行的 %q 操作符需要给出值", i+1, f.Op)
		}
		out = append(out, f)
	}
	return out, nil
}

// FiltersToText 把过滤器渲染回文本，供表单回填。
func FiltersToText(fs []store.Filter) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Path)
		b.WriteByte(' ')
		b.WriteString(f.Op)
		if f.Value != nil {
			b.WriteByte(' ')
			b.WriteString(filterValueText(f.Value))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func filterValueText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// ---------- 表单 -> 模型 ----------

func (s *Server) ruleFromForm(r *http.Request) (*store.Rule, error) {
	from, err := formIntList(r, "from_source_ids")
	if err != nil {
		return &store.Rule{}, errors.New("接收源选择不合法")
	}
	to, err := formIntList(r, "to_source_ids")
	if err != nil {
		return &store.Rule{}, errors.New("目标源选择不合法")
	}
	filters, err := ParseFilters(r.PostFormValue("filters"))
	if err != nil {
		return &store.Rule{}, err
	}

	rule := &store.Rule{
		Name:            formValue(r, "name"),
		Enabled:         formBool(r, "enabled"),
		FromSourceIDs:   from,
		ToSourceIDs:     to,
		Filters:         filters,
		BodyTemplate:    strings.TrimSpace(r.PostFormValue("body_template")),
		SubjectTemplate: strings.TrimSpace(r.PostFormValue("subject_template")),
		HeadersTemplate: strings.TrimSpace(r.PostFormValue("headers_template")),
	}

	if rule.Name == "" {
		return rule, errors.New("规则名称不能为空")
	}
	if len(rule.FromSourceIDs) == 0 {
		return rule, errors.New("至少要选一个接收源")
	}
	if len(rule.ToSourceIDs) == 0 {
		return rule, errors.New("至少要选一个目标源")
	}
	if strings.TrimSpace(rule.HeadersTemplate) == "" {
		rule.HeadersTemplate = "{}"
	}
	// 模板问题在保存时就报出来，别等到真来了请求才发现。
	// 注意这里只校验语法，不拿假 payload 去执行 —— 见 engine.ValidateTemplate 的说明。
	if err := engine.ValidateTemplate("报文", rule.BodyTemplate); err != nil {
		return rule, err
	}
	if err := engine.ValidateTemplate("主题", rule.SubjectTemplate); err != nil {
		return rule, err
	}
	if err := engine.ValidateHeaderTemplates(rule.HeadersTemplate); err != nil {
		return rule, err
	}
	return rule, nil
}

// sampleTemplateData 只用在校验模板语法，内容无所谓。
func idSet(ids []int64) map[int64]bool {
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func namesOf(ids []int64, byID map[int64]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := byID[id]; ok {
			out = append(out, name)
		} else {
			out = append(out, fmt.Sprintf("已删除的源 #%d", id))
		}
	}
	return out
}
