package store

import (
	"fmt"
	"strings"
)

// 投递状态机：pending -> success
//
//	\-> failed -> (重试) -> success | dead
//
// 另有 dropped：被循环检测或过滤器拦下，不重试。
const (
	StatusPending = "pending"
	StatusSuccess = "success"
	StatusFailed  = "failed"
	StatusDead    = "dead"
	StatusDropped = "dropped"
)

type Delivery struct {
	ID           int64
	TraceID      string
	RuleID       int64
	InSourceID   int64
	OutSourceID  int64
	HopChain     string
	Status       string
	Attempt      int
	Payload      string
	Rendered     string
	Subject      string // 邮件目标的主题（已渲染）
	ReqHeaders   string // 渲染后的出站请求头（JSON），重放时据此忠实重发
	ResponseCode int
	ResponseBody string
	LastError    string
	NextRetryAt  int64
	CreatedAt    int64
	UpdatedAt    int64

	// 以下来自 JOIN，仅用于界面展示，不落库。
	RuleName      string
	InSourceName  string
	OutSourceName string
}

// deliveryFrom 是投递记录的统一数据源：列表、统计、筛选都要带上这几个 JOIN
// （关键字搜索要按规则名 / 源名匹配），所以抽出来共用，免得统计那条漏掉 JOIN。
const deliveryFrom = ` FROM deliveries d
	LEFT JOIN rules r ON r.id = d.rule_id
	LEFT JOIN sources si ON si.id = d.in_source_id
	LEFT JOIN sources so ON so.id = d.out_source_id`

const deliverySelect = `SELECT d.id, d.trace_id, d.rule_id, d.in_source_id, d.out_source_id,
	d.hop_chain, d.status, d.attempt, d.payload, d.rendered, d.subject, d.req_headers,
	d.response_code, d.response_body, d.last_error, d.next_retry_at, d.created_at, d.updated_at,
	COALESCE(r.name,''), COALESCE(si.name,''), COALESCE(so.name,'')` + deliveryFrom

func scanDelivery(sc interface{ Scan(...any) error }) (*Delivery, error) {
	var v Delivery
	err := sc.Scan(
		&v.ID, &v.TraceID, &v.RuleID, &v.InSourceID, &v.OutSourceID,
		&v.HopChain, &v.Status, &v.Attempt, &v.Payload, &v.Rendered, &v.Subject, &v.ReqHeaders,
		&v.ResponseCode, &v.ResponseBody, &v.LastError, &v.NextRetryAt, &v.CreatedAt, &v.UpdatedAt,
		&v.RuleName, &v.InSourceName, &v.OutSourceName,
	)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func (s *Store) CreateDelivery(d *Delivery) error {
	d.CreatedAt = nowUnix()
	d.UpdatedAt = d.CreatedAt
	res, err := s.db.Exec(`INSERT INTO deliveries (
		trace_id, rule_id, in_source_id, out_source_id, hop_chain, status, attempt,
		payload, rendered, subject, req_headers, response_code, response_body, last_error, next_retry_at,
		created_at, updated_at
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.TraceID, d.RuleID, d.InSourceID, d.OutSourceID, d.HopChain, d.Status, d.Attempt,
		d.Payload, d.Rendered, d.Subject, d.ReqHeaders, d.ResponseCode, d.ResponseBody, d.LastError, d.NextRetryAt,
		d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return fmt.Errorf("写入投递记录: %w", err)
	}
	d.ID, err = res.LastInsertId()
	return err
}

func (s *Store) UpdateDelivery(d *Delivery) error {
	d.UpdatedAt = nowUnix()
	_, err := s.db.Exec(`UPDATE deliveries SET
		status=?, attempt=?, rendered=?, subject=?, req_headers=?, response_code=?, response_body=?,
		last_error=?, next_retry_at=?, updated_at=?
		WHERE id=?`,
		d.Status, d.Attempt, d.Rendered, d.Subject, d.ReqHeaders, d.ResponseCode, d.ResponseBody,
		d.LastError, d.NextRetryAt, d.UpdatedAt, d.ID)
	if err != nil {
		return fmt.Errorf("更新投递记录 %d: %w", d.ID, err)
	}
	return nil
}

func (s *Store) GetDelivery(id int64) (*Delivery, error) {
	v, err := scanDelivery(s.db.QueryRow(deliverySelect+` WHERE d.id = ?`, id))
	if isNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取投递记录 %d: %w", id, err)
	}
	return v, nil
}

type DeliveryFilter struct {
	Status     string
	InSourceID int64
	RuleID     int64
	// Keyword 在规则名、接收源名、目标源名和追踪号里做模糊匹配。
	Keyword string
	Limit   int
	Offset  int
}

// likeEscape 转义 LIKE 的通配符（% 和 _），反斜杠自己要先转。
var likeEscape = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func (f DeliveryFilter) where() (string, []any) {
	var conds []string
	var args []any
	if f.Status != "" {
		conds = append(conds, `d.status = ?`)
		args = append(args, f.Status)
	}
	if f.InSourceID != 0 {
		conds = append(conds, `d.in_source_id = ?`)
		args = append(args, f.InSourceID)
	}
	if f.RuleID != 0 {
		conds = append(conds, `d.rule_id = ?`)
		args = append(args, f.RuleID)
	}
	if f.Keyword != "" {
		// SQLite 的 LIKE 对 ASCII 本来就不区分大小写。
		// % 和 _ 在 LIKE 里是通配符，用户输入里带了就得转义，不然搜「100%」会命中一切。
		like := "%" + likeEscape.Replace(f.Keyword) + "%"
		conds = append(conds, `(r.name LIKE ? ESCAPE '\' OR si.name LIKE ? ESCAPE '\'`+
			` OR so.name LIKE ? ESCAPE '\' OR d.trace_id LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

func (s *Store) ListDeliveries(f DeliveryFilter) ([]*Delivery, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	where, args := f.where()
	args = append(args, f.Limit, f.Offset)

	rows, err := s.db.Query(deliverySelect+where+` ORDER BY d.id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("列出投递记录: %w", err)
	}
	defer rows.Close()

	var out []*Delivery
	for rows.Next() {
		v, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) CountDeliveries(f DeliveryFilter) (int, error) {
	where, args := f.where()
	var n int
	// 统计不关心分页，但 WHERE 里可能引用 JOIN 出来的名字，所以 FROM 必须一致。
	err := s.db.QueryRow(`SELECT COUNT(*)`+deliveryFrom+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计投递记录: %w", err)
	}
	return n, nil
}

// DueDeliveries 取出到期该投递的记录，涵盖两种「还没投成功」的状态：
// pending（还没试过）与 failed（试过、正在退避等待）。
// next_retry_at=0 表示立即投递，因此进程重启后遗留的记录会被自动接管。
func (s *Store) DueDeliveries(limit int) ([]*Delivery, error) {
	rows, err := s.db.Query(deliverySelect+
		` WHERE d.status IN (?, ?) AND d.next_retry_at <= ? ORDER BY d.next_retry_at LIMIT ?`,
		StatusPending, StatusFailed, nowUnix(), limit)
	if err != nil {
		return nil, fmt.Errorf("取出待投递记录: %w", err)
	}
	defer rows.Close()

	var out []*Delivery
	for rows.Next() {
		v, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ReplayDelivery 把一条记录重置为立即重投。
func (s *Store) ReplayDelivery(id int64) error {
	_, err := s.db.Exec(`UPDATE deliveries SET
		status=?, attempt=0, next_retry_at=0, last_error='', response_code=0, response_body='', updated_at=?
		WHERE id=?`, StatusPending, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("重放投递记录 %d: %w", id, err)
	}
	return nil
}

func (s *Store) DeleteDelivery(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM deliveries WHERE id = ?`, id); err != nil {
		return fmt.Errorf("删除投递记录 %d: %w", id, err)
	}
	return nil
}

// Stats 返回 since 之后各状态的投递数量。
func (s *Store) Stats(since int64) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM deliveries WHERE created_at >= ? GROUP BY status`, since)
	if err != nil {
		return nil, fmt.Errorf("统计投递状态: %w", err)
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// CleanupDeliveries 删除 before 之前的终态日志，返回删除条数。
// 还没投递成功的记录（pending / failed）不删，否则重试队列会被误清。
func (s *Store) CleanupDeliveries(before int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM deliveries WHERE created_at < ? AND status NOT IN (?, ?)`,
		before, StatusPending, StatusFailed)
	if err != nil {
		return 0, fmt.Errorf("清理历史投递: %w", err)
	}
	return res.RowsAffected()
}
