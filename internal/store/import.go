package store

import (
	"fmt"
)

// ReplaceConfig 用导入的配置整体替换现有的源与规则，并写入随文件带来的设置项。
//
// rules 里的 FromSourceIDs / ToSourceIDs 放的是 sources 切片的下标（不是数据库 id），
// 导入时重新映射成新生成的 id —— 这样导出文件里不含内部主键，换机器也能用。
//
// settings 放的是已经确定要覆盖的键值（可以为空，表示文件里没带设置）。
// 三者放在一个事务里：中途失败就当作没导入过，不会留下半套配置，
// 也不会出现「源换了、设置没换」这种半截状态。
//
// 返回值是本次被终止的未完成投递条数。源和规则整体重建之后，旧投递的
// out_source_id / rule_id 全部指不到东西了，这些记录再也不可能发出去，
// 所以在这个事务里直接标成 dead 并写明原因 —— 不这么做的话它们会一直躺在
// pending 里，直到某次重试才变成一句让人摸不着头脑的「目标源已不存在」。
func (s *Store) ReplaceConfig(sources []*Source, rules []*Rule, settings map[string]string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("开始导入事务: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM rules`); err != nil {
		return 0, fmt.Errorf("清空规则: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM sources`); err != nil {
		return 0, fmt.Errorf("清空源: %w", err)
	}

	newIDs := make([]int64, len(sources))
	for i, src := range sources {
		src.ID = 0
		if err := saveSource(tx, src); err != nil {
			return 0, fmt.Errorf("导入源 %q: %w", src.Name, err)
		}
		newIDs[i] = src.ID
	}

	for _, rule := range rules {
		rule.ID = 0
		rule.FromSourceIDs = remapIDs(rule.FromSourceIDs, newIDs)
		rule.ToSourceIDs = remapIDs(rule.ToSourceIDs, newIDs)
		if err := saveRule(tx, rule); err != nil {
			return 0, fmt.Errorf("导入规则 %q: %w", rule.Name, err)
		}
	}

	res, err := tx.Exec(`UPDATE deliveries SET status=?, last_error=?, next_retry_at=0, updated_at=?
		WHERE status IN (?, ?)`,
		StatusDead, "配置导入替换后该投递已终止", nowUnix(), StatusPending, StatusFailed)
	if err != nil {
		return 0, fmt.Errorf("终止未完成投递: %w", err)
	}
	aborted, _ := res.RowsAffected()

	if err := setSettings(tx, settings); err != nil {
		return 0, fmt.Errorf("导入设置: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交导入事务: %w", err)
	}
	return aborted, nil
}

// remapIDs 把导出文件里的下标换成真实的源 id，越界的直接丢掉。
//
// 越界本该在 web 层就被挡下来（导入前逐个核对下标），这里的丢弃只是最后一道兜底：
// 真漏到这里，规则会少一个源，但不会指向一个不存在的 id。
func remapIDs(idx []int64, newIDs []int64) []int64 {
	out := make([]int64, 0, len(idx))
	for _, i := range idx {
		if i >= 0 && int(i) < len(newIDs) {
			out = append(out, newIDs[i])
		}
	}
	return out
}
