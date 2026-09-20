package store

import (
	"encoding/json"
	"fmt"
)

// Filter 是规则上的一条过滤条件，多条之间是 AND 关系。
type Filter struct {
	Path  string `json:"path"`
	Op    string `json:"op"`
	Value any    `json:"value"`
}

// Rule 把一组「接收源」映射到一组「目标源」。
type Rule struct {
	ID              int64
	Name            string
	Enabled         bool
	FromSourceIDs   []int64
	ToSourceIDs     []int64
	Filters         []Filter
	BodyTemplate    string
	SubjectTemplate string // 仅邮件目标用；空则由引擎给默认主题
	HeadersTemplate string // JSON 对象，值支持模板
	CreatedAt       int64
	UpdatedAt       int64
}

const ruleCols = `id, name, enabled, from_source_ids, to_source_ids, filters,
	body_template, subject_template, headers_template, created_at, updated_at`

func scanRule(sc interface{ Scan(...any) error }) (*Rule, error) {
	var (
		v                           Rule
		from, to, filters, hdrsJSON string
	)
	if err := sc.Scan(&v.ID, &v.Name, &v.Enabled, &from, &to, &filters,
		&v.BodyTemplate, &v.SubjectTemplate, &hdrsJSON, &v.CreatedAt, &v.UpdatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(from), &v.FromSourceIDs); err != nil {
		return nil, fmt.Errorf("规则 %d 的 from_source_ids 不是合法 JSON: %w", v.ID, err)
	}
	if err := json.Unmarshal([]byte(to), &v.ToSourceIDs); err != nil {
		return nil, fmt.Errorf("规则 %d 的 to_source_ids 不是合法 JSON: %w", v.ID, err)
	}
	if err := json.Unmarshal([]byte(filters), &v.Filters); err != nil {
		return nil, fmt.Errorf("规则 %d 的 filters 不是合法 JSON: %w", v.ID, err)
	}
	v.HeadersTemplate = hdrsJSON
	return &v, nil
}

func (s *Store) ListRules() ([]*Rule, error) {
	rows, err := s.db.Query(`SELECT ` + ruleCols + ` FROM rules ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("列出规则: %w", err)
	}
	defer rows.Close()

	var out []*Rule
	for rows.Next() {
		v, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) GetRule(id int64) (*Rule, error) {
	v, err := scanRule(s.db.QueryRow(`SELECT `+ruleCols+` FROM rules WHERE id = ?`, id))
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取规则 %d: %w", id, err)
	}
	return v, nil
}

// RulesForSource 返回所有「以该源为接收方」的启用规则。
//
// ponytail: 规则数量是人工维护的个位数到几十条，全量取出在内存里筛即可；
// 真到上千条再改成 SQL 侧 JSON 查询或建关联表。
func (s *Store) RulesForSource(sourceID int64) ([]*Rule, error) {
	all, err := s.ListRules()
	if err != nil {
		return nil, err
	}
	var out []*Rule
	for _, r := range all {
		if r.Enabled && containsID(r.FromSourceIDs, sourceID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) SaveRule(v *Rule) error { return saveRule(s.db, v) }

func saveRule(db execer, v *Rule) error {
	from := jsonList(v.FromSourceIDs)
	to := jsonList(v.ToSourceIDs)
	filters, err := json.Marshal(v.Filters)
	if err != nil {
		return fmt.Errorf("序列化过滤器: %w", err)
	}
	if len(v.Filters) == 0 {
		filters = []byte("[]")
	}
	hdrs := v.HeadersTemplate
	if hdrs == "" {
		hdrs = "{}"
	}

	v.UpdatedAt = nowUnix()
	if v.ID == 0 {
		v.CreatedAt = v.UpdatedAt
		res, err := db.Exec(`INSERT INTO rules (
			name, enabled, from_source_ids, to_source_ids, filters,
			body_template, subject_template, headers_template, created_at, updated_at
		) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			v.Name, v.Enabled, from, to, string(filters),
			v.BodyTemplate, v.SubjectTemplate, hdrs, v.CreatedAt, v.UpdatedAt)
		if err != nil {
			return fmt.Errorf("新建规则: %w", err)
		}
		v.ID, err = res.LastInsertId()
		return err
	}

	_, err = db.Exec(`UPDATE rules SET
		name=?, enabled=?, from_source_ids=?, to_source_ids=?, filters=?,
		body_template=?, subject_template=?, headers_template=?, updated_at=?
		WHERE id=?`,
		v.Name, v.Enabled, from, to, string(filters),
		v.BodyTemplate, v.SubjectTemplate, hdrs, v.UpdatedAt, v.ID)
	if err != nil {
		return fmt.Errorf("更新规则 %d: %w", v.ID, err)
	}
	return nil
}

func (s *Store) DeleteRule(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM rules WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除规则 %d: %w", id, err)
	}
	return nil
}

// pruneSourceFromRules 在源被删除后，把它从所有规则的源列表里摘掉。
func (s *Store) pruneSourceFromRules(sourceID int64) error {
	all, err := s.ListRules()
	if err != nil {
		return err
	}
	for _, r := range all {
		from := withoutID(r.FromSourceIDs, sourceID)
		to := withoutID(r.ToSourceIDs, sourceID)
		if len(from) == len(r.FromSourceIDs) && len(to) == len(r.ToSourceIDs) {
			continue
		}
		r.FromSourceIDs, r.ToSourceIDs = from, to
		if err := s.SaveRule(r); err != nil {
			return err
		}
	}
	return nil
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func withoutID(ids []int64, id int64) []int64 {
	out := make([]int64, 0, len(ids))
	for _, v := range ids {
		if v != id {
			out = append(out, v)
		}
	}
	return out
}

// jsonList 保证空切片序列化成 [] 而不是 null。
func jsonList(ids []int64) string {
	if len(ids) == 0 {
		return "[]"
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}
