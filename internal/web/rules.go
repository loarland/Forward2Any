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
		"PageSize": rulesPageSize,
		"Sizes":    perPageOptions,
	})
}

func (s *Server) handleRuleForm(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("id") == "" {
		s.renderRuleForm(w, r, &store.Rule{Enabled: true}, "", "")
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
	s.renderRuleForm(w, r, rule, FiltersToText(rule.Filters), "")
}

// renderRuleForm 里的 filterText 单独传：校验失败时要原样回填用户写的那段文本，
// 而 rule.Filters 只放解析成功的部分（解析失败时它是空的）。
func (s *Server) renderRuleForm(w http.ResponseWriter, r *http.Request, rule *store.Rule, filterText, errMsg string) {
	sources, err := s.store.ListSources()
	if err != nil {
		s.fail(w, "读取源列表失败", err)
		return
	}
	from, fromHidden := pickOptions(sources, idSet(rule.FromSourceIDs), "from")
	to, toHidden := pickOptions(sources, idSet(rule.ToSourceIDs), "to")
	s.render(w, r, "rule_form", map[string]any{
		"Title": "规则",
		"Nav":   "rules",
		"Rule":  rule,
		"IsNew": rule.ID == 0,
		// 两栏只有名字和选项不同，所以做成同一张牌的两面，模板里共用一段 markup。
		"Pickers": []rulePicker{
			{
				Role: "from", Input: "from_source_ids", Title: "接收源", RoleText: "接收",
				Hint: "这些源收到消息时触发本规则", Options: from, Hidden: fromHidden, Count: len(rule.FromSourceIDs),
			},
			{
				Role: "to", Input: "to_source_ids", Title: "目标源", RoleText: "发送",
				Hint: "消息会被转发到这些源", Options: to, Hidden: toHidden, Count: len(rule.ToSourceIDs),
			},
		},
		"NoSource":   len(sources) == 0,
		"FilterText": filterText,
		"Error":      errMsg,
	})
}

// rulePicker 是规则表单里「选源」的一栏。
type rulePicker struct {
	Role     string // from / to，同时当 DOM 的 data 属性和 id 用
	Input    string // 勾选框的 name，也就是提交时的字段名
	Title    string
	RoleText string // 「接收」/「发送」，拼提示语用
	Hint     string
	Options  []ruleSourceOption
	Hidden   int // 因为用途不合适、没列出来的源数量
	Count    int // 已选数量：渲染时给个初值，之后由 JS 维护
}

// ruleSourceOption 是「选源」列表里的一项。
type ruleSourceOption struct {
	*store.Source
	Checked bool
	// Eligible 表示这个源现在的用途能不能担任这一侧的角色。
	// 不能的默认不列出来，但已经选中的仍然要显示（带一句警告）——
	// 否则用户打开表单随手一保存，规则里那个 ID 就被悄悄抹掉了。
	Eligible bool
	Warn     string
}

// pickOptions 挑出这一栏要列的源，外加被藏起来的数量。
//
// 依据跟服务端实际执行时的那套判断一致：接收侧认 CanReceive（hook 对非接收用途的源直接 404，
// 邮件源也不会被轮询），发送侧认 CanSend（引擎对不能发送的目标源会跳过并记一条警告）。
func pickOptions(sources []*store.Source, chosen map[int64]bool, role string) ([]ruleSourceOption, int) {
	send := role == "to"
	// 用途不合适的源排在前面：它是这一栏里唯一需要用户动手处理的一项。
	var bad, ok []ruleSourceOption
	hidden := 0
	for _, src := range sources {
		eligible := src.CanReceive()
		warn := "用途不含接收，收到消息也不会触发本规则"
		if send {
			eligible = src.CanSend()
			warn = "用途不含发送，转发到它时会被跳过"
		}
		if !eligible && !chosen[src.ID] {
			hidden++
			continue
		}
		opt := ruleSourceOption{Source: src, Checked: chosen[src.ID], Eligible: eligible}
		if !eligible {
			opt.Warn = warn
			bad = append(bad, opt)
			continue
		}
		ok = append(ok, opt)
	}
	return append(bad, ok...), hidden
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
		s.renderRuleForm(w, r, rule, r.PostFormValue("filters"), err.Error())
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
		s.renderRuleForm(w, r, rule, r.PostFormValue("filters"), err.Error())
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
	// 先把表单里的所有字段收下来，再去校验。以前是每步校验失败就返回一个空 Rule，
	// 结果过滤条件写错一行，名称、源选择、模板全被清空，用户得从头再填一遍。
	rule := &store.Rule{
		Name:            formValue(r, "name"),
		Enabled:         formBool(r, "enabled"),
		BodyTemplate:    strings.TrimSpace(r.PostFormValue("body_template")),
		SubjectTemplate: strings.TrimSpace(r.PostFormValue("subject_template")),
		HeadersTemplate: strings.TrimSpace(r.PostFormValue("headers_template")),
	}

	from, err := formIntList(r, "from_source_ids")
	if err != nil {
		return rule, errors.New("接收源选择不合法")
	}
	to, err := formIntList(r, "to_source_ids")
	if err != nil {
		return rule, errors.New("目标源选择不合法")
	}
	rule.FromSourceIDs, rule.ToSourceIDs = from, to

	filters, err := ParseFilters(r.PostFormValue("filters"))
	if err != nil {
		return rule, err
	}
	rule.Filters = filters

	// 「至少各选一个源」是表单自己的要求（下拉里没选就提交不了）。
	// 不放进 validateRule 是因为库里允许存在两边为空的规则：删掉最后一个源之后，
	// 那条规则会被摘成空的留在库里等着改，导出再导入时不该因此被拒。
	if len(rule.FromSourceIDs) == 0 {
		return rule, errors.New("至少要选一个接收源")
	}
	if len(rule.ToSourceIDs) == 0 {
		return rule, errors.New("至少要选一个目标源")
	}
	if err := validateRule(rule); err != nil {
		return rule, err
	}
	return rule, nil
}

// validateRule 是规则本身的合法性检查，表单保存和配置导入走同一套 ——
// 手工改过的配置文件不该能塞进一条界面上根本存不下的规则
// （模板语法、请求头 JSON、过滤条件这些要等到真投递时才炸的东西）。
//
// 会就地补上默认值（空的请求头模板当成 "{}"），跟表单保存的行为一致。
func validateRule(rule *store.Rule) error {
	if strings.TrimSpace(rule.Name) == "" {
		return errors.New("规则名称不能为空")
	}
	if strings.TrimSpace(rule.HeadersTemplate) == "" {
		rule.HeadersTemplate = "{}"
	}
	// 模板问题在保存时就报出来，别等到真来了请求才发现。
	// 注意这里只校验语法，不拿假 payload 去执行 —— 见 engine.ValidateTemplate 的说明。
	if err := engine.ValidateTemplate("报文", rule.BodyTemplate); err != nil {
		return err
	}
	if err := engine.ValidateTemplate("主题", rule.SubjectTemplate); err != nil {
		return err
	}
	if err := engine.ValidateHeaderTemplates(rule.HeadersTemplate); err != nil {
		return err
	}
	// 过滤条件从表单进来时已经被 ParseFilters 查过一遍；从配置文件进来的是
	// 结构化数据，没有那道关卡，所以在这里统一核一遍。
	for i, f := range rule.Filters {
		if strings.TrimSpace(f.Path) == "" {
			return fmt.Errorf("过滤条件第 %d 条没有路径", i+1)
		}
		if !validFilterOps[f.Op] {
			return fmt.Errorf("过滤条件第 %d 条的操作符 %q 不认识", i+1, f.Op)
		}
		if f.Op != "exists" && f.Op != "not_exists" && f.Value == nil {
			return fmt.Errorf("过滤条件第 %d 条的 %q 操作符需要给出值", i+1, f.Op)
		}
	}
	return nil
}

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
