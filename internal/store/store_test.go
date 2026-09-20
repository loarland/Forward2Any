package store

import (
	"testing"
	"time"
)

// 环境变量只在首次启动时作为引导数据；之后一律以数据库为准。
// 这里同时盯住两个曾经的坑：端口没种进去、以及每次启动都被环境变量覆盖。
func TestBootstrapSeedsOnlyOnce(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	usedDefault, err := st.Bootstrap(BootstrapInput{
		Port: 18080, AdminUser: "alice", AdminPass: "pw-123456", BaseURL: "http://example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if usedDefault {
		t.Error("显式提供了密码，就不该标记为「使用默认密码」")
	}

	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.WebPort != 18080 {
		t.Errorf("环境变量里的端口应当生效，得到 %d", s.WebPort)
	}
	if s.AdminUser != "alice" || s.BaseURL != "http://example.test" {
		t.Errorf("引导值不对: 用户=%q 基址=%q", s.AdminUser, s.BaseURL)
	}
	if !CheckPassword(s.AdminPassHash, "pw-123456") {
		t.Error("引导密码校验不过")
	}

	// 模拟第二次启动，环境变量换了值
	if _, err := st.Bootstrap(BootstrapInput{
		Port: 9999, AdminUser: "bob", AdminPass: "other-pw-1", BaseURL: "http://other.test",
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if s2.WebPort != 18080 {
		t.Errorf("端口被环境变量覆盖了: %d", s2.WebPort)
	}
	if s2.AdminUser != "alice" {
		t.Errorf("用户名被环境变量覆盖了: %q", s2.AdminUser)
	}
	if s2.BaseURL != "http://example.test" {
		t.Errorf("回调基址被环境变量覆盖了: %q", s2.BaseURL)
	}
	if !CheckPassword(s2.AdminPassHash, "pw-123456") {
		t.Error("密码被环境变量覆盖了")
	}
}

// 不给密码时落到默认密码，并标记出来（后台据此强制改密）。
func TestBootstrapFallsBackToDefaultPassword(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	in := BootstrapInput{Port: 16000, AdminUser: "admin", BaseURL: "http://localhost:16000"}
	usedDefault, err := st.Bootstrap(in)
	if err != nil {
		t.Fatal(err)
	}
	if !usedDefault {
		t.Error("没有提供密码时应当标记为「使用默认密码」")
	}

	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !s.AdminPassDefault {
		t.Error("AdminPassDefault 应当为真")
	}
	if !CheckPassword(s.AdminPassHash, DefaultAdminPassword) {
		t.Errorf("默认密码应当是 %q", DefaultAdminPassword)
	}

	// 再次启动不该改变任何东西
	usedDefault2, err := st.Bootstrap(in)
	if err != nil {
		t.Fatal(err)
	}
	if !usedDefault2 {
		t.Error("没改过密码时，重启后仍应标记为使用默认密码")
	}
}

// 改过密码之后，标记必须清掉，否则后台会一直被锁在设置页。
func TestBootstrapDefaultFlagClearedAfterPasswordChange(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	in := BootstrapInput{Port: 16000, AdminUser: "admin", BaseURL: "http://localhost:16000"}
	if _, err := st.Bootstrap(in); err != nil {
		t.Fatal(err)
	}

	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword("a-real-password")
	if err != nil {
		t.Fatal(err)
	}
	s.AdminPassHash = hash
	s.AdminPassDefault = false
	if err := st.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	usedDefault, err := st.Bootstrap(in)
	if err != nil {
		t.Fatal(err)
	}
	if usedDefault {
		t.Error("密码已改过，重启后不该再标记为使用默认密码")
	}
}

func TestReplaceConfigRemapsSourceIndices(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 先塞点旧数据，确认导入是「整体替换」而不是追加
	old := &Source{Name: "旧源", Kind: "webhook", Usage: "in", Enabled: true, Slug: "old-one", Headers: "{}", HTTPMethod: "POST"}
	if err := st.SaveSource(old); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveRule(&Rule{
		Name: "旧规则", Enabled: true,
		FromSourceIDs: []int64{old.ID}, ToSourceIDs: []int64{old.ID},
	}); err != nil {
		t.Fatal(err)
	}

	sources := []*Source{
		{Name: "接收", Kind: "webhook", Usage: "in", Enabled: true, Slug: "a", Headers: "{}", HTTPMethod: "POST"},
		{Name: "目标", Kind: "webhook", Usage: "out", Enabled: true, URL: "http://x", Headers: "{}", HTTPMethod: "POST"},
	}
	rules := []*Rule{
		{Name: "新规则", Enabled: true, FromSourceIDs: []int64{0}, ToSourceIDs: []int64{1}},
		{Name: "越界引用", Enabled: true, FromSourceIDs: []int64{0}, ToSourceIDs: []int64{9}},
	}
	if err := st.ReplaceConfig(sources, rules); err != nil {
		t.Fatal(err)
	}

	got, err := st.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("导入后应当只剩 2 个源，得到 %d", len(got))
	}

	gotRules, err := st.ListRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(gotRules) != 2 {
		t.Fatalf("应当有 2 条规则，得到 %d", len(gotRules))
	}

	// 下标 0/1 要映射成新生成的源 id
	if len(gotRules[0].FromSourceIDs) != 1 || gotRules[0].FromSourceIDs[0] != got[0].ID {
		t.Errorf("接收源下标没有映射成新 id: %#v（新源 id=%d）", gotRules[0].FromSourceIDs, got[0].ID)
	}
	if len(gotRules[0].ToSourceIDs) != 1 || gotRules[0].ToSourceIDs[0] != got[1].ID {
		t.Errorf("目标源下标没有映射成新 id: %#v（新源 id=%d）", gotRules[0].ToSourceIDs, got[1].ID)
	}
	// 越界引用应当被丢掉，而不是留下指向不存在源的 id
	if len(gotRules[1].ToSourceIDs) != 0 {
		t.Errorf("越界下标应被丢弃，得到 %#v", gotRules[1].ToSourceIDs)
	}

	found, err := st.RulesForSource(got[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Errorf("按源查规则应返回 2 条，得到 %d", len(found))
	}
}

func TestDeleteSourcePrunesRules(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	a := &Source{Name: "A", Kind: "webhook", Usage: "in", Enabled: true, Slug: "a", Headers: "{}", HTTPMethod: "POST"}
	b := &Source{Name: "B", Kind: "webhook", Usage: "out", Enabled: true, URL: "http://b", Headers: "{}", HTTPMethod: "POST"}
	for _, s := range []*Source{a, b} {
		if err := st.SaveSource(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveRule(&Rule{
		Name: "r", Enabled: true,
		FromSourceIDs: []int64{a.ID}, ToSourceIDs: []int64{b.ID},
	}); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteSource(b.ID); err != nil {
		t.Fatal(err)
	}
	rules, err := st.ListRules()
	if err != nil {
		t.Fatal(err)
	}
	if len(rules[0].ToSourceIDs) != 0 {
		t.Errorf("被删的源应当从规则里摘掉，得到 %#v", rules[0].ToSourceIDs)
	}
	if len(rules[0].FromSourceIDs) != 1 {
		t.Errorf("不该误删其它源引用，得到 %#v", rules[0].FromSourceIDs)
	}
}

func TestSourceBySlugAndNotFound(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.GetSource(12345); err != ErrNotFound {
		t.Errorf("不存在的源应返回 ErrNotFound，得到 %v", err)
	}
	if _, err := st.SourceBySlug("nope"); err != ErrNotFound {
		t.Errorf("不存在的 slug 应返回 ErrNotFound，得到 %v", err)
	}

	src := &Source{Name: "同名", Kind: "webhook", Usage: "in", Enabled: true, Headers: "{}", HTTPMethod: "POST"}
	src.Slug = SuggestSlug(src.Name)
	if err := st.SaveSource(src); err != nil {
		t.Fatal(err)
	}
	if got, err := st.SourceBySlug(src.Slug); err != nil || got.ID != src.ID {
		t.Errorf("按 slug 查不到刚存的源: %v", err)
	}
}

// 重试队列必须同时覆盖 pending（还没试过）和 failed（试过、在退避等待），
// 但绝不能捞起已成功/已放弃/已拦截的记录。
func TestDueDeliveriesCoversPendingAndFailed(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now().Unix()
	add := func(status string, nextRetry int64) *Delivery {
		d := &Delivery{Status: status, NextRetryAt: nextRetry, Payload: "x"}
		if err := st.CreateDelivery(d); err != nil {
			t.Fatal(err)
		}
		return d
	}

	pendingDue := add(StatusPending, now-1)
	failedDue := add(StatusFailed, now-1)
	add(StatusFailed, now+3600) // 还没到重试时间
	add(StatusSuccess, now-1)
	add(StatusDead, now-1)
	add(StatusDropped, now-1)

	due, err := st.DueDeliveries(50)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[int64]bool, len(due))
	for _, d := range due {
		got[d.ID] = true
	}

	if !got[pendingDue.ID] {
		t.Error("到期的 pending 应当被取出")
	}
	if !got[failedDue.ID] {
		t.Error("到期的 failed 应当被取出（否则重试链会断掉）")
	}
	if len(due) != 2 {
		t.Errorf("应当只取出这 2 条，得到 %d 条", len(due))
	}
}

// 日志清理只能删终态记录，未投递完成的必须留下。
func TestCleanupKeepsUnfinishedDeliveries(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	longAgo := time.Now().Unix() - 86400*90
	add := func(status string) int64 {
		d := &Delivery{Status: status, Payload: "x"}
		if err := st.CreateDelivery(d); err != nil {
			t.Fatal(err)
		}
		// 把创建时间回填到很久以前，好让它落入清理范围
		if _, err := st.db.Exec(`UPDATE deliveries SET created_at = ? WHERE id = ?`, longAgo, d.ID); err != nil {
			t.Fatal(err)
		}
		return d.ID
	}

	keepPending := add(StatusPending)
	keepFailed := add(StatusFailed)
	dropSuccess := add(StatusSuccess)
	dropDead := add(StatusDead)

	n, err := st.CleanupDeliveries(time.Now().Unix() - 86400)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("应当清掉 2 条终态记录，实际 %d", n)
	}

	for _, id := range []int64{keepPending, keepFailed} {
		if _, err := st.GetDelivery(id); err != nil {
			t.Errorf("未投递完成的记录 %d 不该被清理: %v", id, err)
		}
	}
	for _, id := range []int64{dropSuccess, dropDead} {
		if _, err := st.GetDelivery(id); err != ErrNotFound {
			t.Errorf("终态记录 %d 应当被清理，实际 %v", id, err)
		}
	}
}
