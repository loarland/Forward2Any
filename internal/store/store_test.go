package store

import (
	"database/sql"
	"path/filepath"
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

// 关键字搜索要能命中 JOIN 出来的规则名和源名，并且 CountDeliveries 的条数
// 必须和 ListDeliveries 一致 —— 统计那条 SQL 一旦漏掉 JOIN，这里就会炸。
func TestDeliveryKeywordFilter(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	mkSource := func(name string) *Source {
		src := &Source{Name: name, Kind: "webhook", Usage: "in", Enabled: true,
			Slug: name, HTTPMethod: "POST", Headers: "{}", AuthMode: "none"}
		if err := st.SaveSource(src); err != nil {
			t.Fatal(err)
		}
		return src
	}
	in := mkSource("github-in")
	out := mkSource("feishu-out")
	rule := &Rule{Name: "推送到飞书", Enabled: true,
		FromSourceIDs: []int64{in.ID}, ToSourceIDs: []int64{out.ID}}
	if err := st.SaveRule(rule); err != nil {
		t.Fatal(err)
	}
	other := &Rule{Name: "别的规则", Enabled: true,
		FromSourceIDs: []int64{in.ID}, ToSourceIDs: []int64{out.ID}}
	if err := st.SaveRule(other); err != nil {
		t.Fatal(err)
	}

	for _, ruleID := range []int64{rule.ID, rule.ID, other.ID} {
		d := &Delivery{RuleID: ruleID, InSourceID: in.ID, OutSourceID: out.ID,
			Status: StatusSuccess, Payload: "x", TraceID: "trace-abc"}
		if err := st.CreateDelivery(d); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		name    string
		keyword string
		want    int
	}{
		{"空关键字不过滤", "", 3},
		{"按规则名匹配", "飞书", 2},
		{"按接收源名匹配", "github", 3},
		{"按目标源名匹配", "feishu", 3},
		{"按追踪号匹配", "trace-abc", 3},
		{"匹配不到就是 0", "不存在的名字", 0},
		// LIKE 的通配符必须被转义，否则搜 % 会命中一切。
		{"百分号不是通配符", "%", 0},
		{"下划线不是通配符", "_", 0},
	}
	for _, c := range cases {
		f := DeliveryFilter{Keyword: c.keyword}
		list, err := st.ListDeliveries(f)
		if err != nil {
			t.Fatalf("%s: 列表查询失败: %v", c.name, err)
		}
		n, err := st.CountDeliveries(f)
		if err != nil {
			t.Fatalf("%s: 统计失败: %v", c.name, err)
		}
		if len(list) != c.want || n != c.want {
			t.Errorf("%s: keyword=%q 期望 %d 条，列表得到 %d、统计得到 %d",
				c.name, c.keyword, c.want, len(list), n)
		}
	}

	// 关键字和状态是 AND 关系。
	f := DeliveryFilter{Keyword: "飞书", Status: StatusDead}
	if n, _ := st.CountDeliveries(f); n != 0 {
		t.Errorf("关键字叠加状态筛选应当同时生效，得到 %d 条", n)
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

// 外观设置是 KV 表里的两个新键，不加迁移 —— 老库读不到就用默认值，写一次就有。
func TestThemeSettingsRoundTrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	// 全新库里没有这两个键，应当拿到默认值。
	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.ThemeColor != "blue" || s.ThemeMode != "auto" {
		t.Errorf("默认外观应为 blue/auto，实际 %q/%q", s.ThemeColor, s.ThemeMode)
	}

	s.ThemeColor = "jade"
	s.ThemeMode = "dark"
	if err := st.SaveSettings(s); err != nil {
		t.Fatal(err)
	}

	again, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if again.ThemeColor != "jade" || again.ThemeMode != "dark" {
		t.Errorf("外观没存住：%q/%q", again.ThemeColor, again.ThemeMode)
	}

	// 只改外观不该动到别的设置。
	if again.WebPort != s.WebPort || again.BaseURL != s.BaseURL {
		t.Error("保存外观时改动了其它设置")
	}
}

// Bootstrap 第一次跑要把外观的默认值种进库，否则「首次启动」的判断会一直认为缺键。
func TestBootstrapSeedsTheme(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Bootstrap(BootstrapInput{Port: 16000, AdminUser: "admin", AdminPass: "pw12345678"}); err != nil {
		t.Fatal(err)
	}
	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.raw[KeyThemeColor]; !ok {
		t.Error("Bootstrap 应当把 theme_color 种进库")
	}
	if _, ok := s.raw[KeyThemeMode]; !ok {
		t.Error("Bootstrap 应当把 theme_mode 种进库")
	}
}

// 代理是 KV 表里的两个新键，同样不需要迁移；ProxyURL 是引擎唯一读它的入口。
func TestProxySettingsRoundTrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.ProxyType != "none" || s.ProxyAddr != "" {
		t.Errorf("默认应当是不使用代理，实际 %q/%q", s.ProxyType, s.ProxyAddr)
	}
	if s.ProxyURL() != "" {
		t.Errorf("没启用代理时不该给出代理地址，实际 %q", s.ProxyURL())
	}

	s.ProxyType, s.ProxyAddr = "socks5", "user:pw@127.0.0.1:1080"
	if err := st.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	again, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if got := again.ProxyURL(); got != "socks5://user:pw@127.0.0.1:1080" {
		t.Errorf("代理没存住或拼错：%q", got)
	}
	// 只改代理不该动到别的设置。
	if again.WebPort != s.WebPort || again.ThemeColor != s.ThemeColor {
		t.Error("保存代理时改动了其它设置")
	}

	// 类型不认识（可能是人手改库改出来的）时当作没配，不能拼出奇怪的地址。
	again.ProxyType = "gopher"
	if got := again.ProxyURL(); got != "" {
		t.Errorf("未知代理类型应当当作没配，实际 %q", got)
	}
	// 类型对但没填地址，同样当作没配。
	again.ProxyType, again.ProxyAddr = "http", ""
	if got := again.ProxyURL(); got != "" {
		t.Errorf("没填地址时不该给出代理地址，实际 %q", got)
	}
}

// 源上的「走代理发送」要能存能读 —— 它是 v2 迁移新加的列。
func TestSourceUseProxyRoundTrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	src := &Source{
		Name: "走代理", Kind: "webhook", Usage: "out", Enabled: true,
		URL: "http://example.com/x", HTTPMethod: "POST", Headers: "{}", UseProxy: true,
	}
	if err := st.SaveSource(src); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSource(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UseProxy {
		t.Error("use_proxy 没有存住")
	}

	got.UseProxy = false
	if err := st.SaveSource(got); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetSource(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.UseProxy {
		t.Error("取消勾选后 use_proxy 没有更新")
	}
}

// 从 v1 升上来的老库：use_proxy 由 v2 的 ALTER 补上，老数据必须原样还在。
func TestMigrateAddsUseProxyToExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f2a.db")

	// 手工造一个停在 user_version=1 的库：只跑第一版迁移，再塞一行老数据。
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range migrations[0] {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	// v1 的 sources 还没有 use_proxy 列
	if _, err := db.Exec(`INSERT INTO sources (name, kind, usage, enabled, slug, url, http_method, headers)
		VALUES ('老源', 'webhook', 'out', 1, '', 'http://example.com/old', 'POST', '{}')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 v1 老库失败: %v", err)
	}
	defer st.Close()

	// 读一遍就会扫 use_proxy 列：列没补上这里直接报错。
	list, err := st.ListSources()
	if err != nil {
		t.Fatalf("升级后读不出老库的源: %v", err)
	}
	if len(list) != 1 || list[0].Name != "老源" {
		t.Fatalf("迁移把老数据弄丢了: %+v", list)
	}
	if list[0].UseProxy {
		t.Error("老数据的 use_proxy 应当是 0")
	}
	if list[0].URL != "http://example.com/old" {
		t.Errorf("老字段被改动了: %q", list[0].URL)
	}

	// 补上的列要真能写
	list[0].UseProxy = true
	if err := st.SaveSource(list[0]); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetSource(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !again.UseProxy {
		t.Error("升级后的库写不进 use_proxy")
	}
}

// Telegram 那几个字段要能存能读，包括「话题 ID 留空」和「端点留空」这两种默认状态。
func TestSourceTelegramRoundTrip(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	src := &Source{
		Name: "TG", Kind: "telegram", Usage: "out", Enabled: true, Headers: "{}",
		TgToken: "123456:ABC-DEF", TgChatID: "-1001234567890", TgThreadID: "42",
		TgEndpoint: "https://api.telegram.org/bot",
	}
	if err := st.SaveSource(src); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSource(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TgToken != src.TgToken || got.TgChatID != src.TgChatID ||
		got.TgThreadID != src.TgThreadID || got.TgEndpoint != src.TgEndpoint {
		t.Errorf("Telegram 字段没有原样存住：%+v", got)
	}

	// 清空是可表达的：话题 ID 和端点都能回到空串
	got.TgThreadID = ""
	got.TgEndpoint = ""
	if err := st.SaveSource(got); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetSource(src.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.TgThreadID != "" || again.TgEndpoint != "" {
		t.Errorf("清空后没有存住：话题 %q 端点 %q", again.TgThreadID, again.TgEndpoint)
	}
}

// 从 v2 升上来的老库（比如已经跑过代理那一版）：Telegram 的列由 v3 的 ALTER 补上，
// 老数据的字段一个都不能动。
func TestMigrateAddsTelegramColumnsToExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f2a.db")

	// 手工造一个停在 user_version=2 的库：跑前两版迁移，再塞一行老数据。
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmts := range migrations[:2] {
		for _, stmt := range stmts {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
	// v2 的 sources 有 use_proxy，但还没有 tg_* 那几列
	if _, err := db.Exec(`INSERT INTO sources (name, kind, usage, enabled, url, http_method, headers, use_proxy)
		VALUES ('老源', 'webhook', 'out', 1, 'http://example.com/v2', 'POST', '{}', 1)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 v2 老库失败: %v", err)
	}
	defer st.Close()

	list, err := st.ListSources()
	if err != nil {
		t.Fatalf("升级后读不出老库的源: %v", err)
	}
	if len(list) != 1 || list[0].Name != "老源" {
		t.Fatalf("迁移把老数据弄丢了: %+v", list)
	}
	if list[0].TgToken != "" || list[0].TgChatID != "" || list[0].TgEndpoint != "" {
		t.Errorf("老数据的 tg_* 应当是空的: %+v", list[0])
	}
	if !list[0].UseProxy || list[0].URL != "http://example.com/v2" {
		t.Errorf("v2 的字段被改动了: %+v", list[0])
	}

	// 补上的列要真能写
	list[0].Kind = "telegram"
	list[0].TgToken = "1:x"
	list[0].TgChatID = "@c"
	if err := st.SaveSource(list[0]); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetSource(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.TgToken != "1:x" || again.TgChatID != "@c" {
		t.Errorf("升级后的库写不进 tg_* 字段: %+v", again)
	}
}

// 从 v3 升上来的老库：渠道那两列由 v4 的 ALTER 补上，老数据一个都不能动。
func TestMigrateAddsChannelColumnsToExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f2a.db")

	// 手工造一个停在 user_version=3 的库：跑前三版迁移，再塞一行老数据。
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmts := range migrations[:3] {
		for _, stmt := range stmts {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}
	// v3 的 sources 有 tg_*，但还没有 channel_* 那两列
	if _, err := db.Exec(`INSERT INTO sources (name, kind, usage, enabled, url, http_method, headers, tg_token, tg_chat_id)
		VALUES ('老 TG 源', 'telegram', 'out', 1, '', 'POST', '{}', '1:x', '@c')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 v3 老库失败: %v", err)
	}
	defer st.Close()

	list, err := st.ListSources()
	if err != nil {
		t.Fatalf("升级后读不出老库的源: %v", err)
	}
	if len(list) != 1 || list[0].Name != "老 TG 源" {
		t.Fatalf("迁移把老数据弄丢了: %+v", list)
	}
	if list[0].ChannelSecret != "" || list[0].ChannelTarget != "" {
		t.Errorf("老数据的 channel_* 应当是空的: %+v", list[0])
	}
	if list[0].TgToken != "1:x" || list[0].TgChatID != "@c" {
		t.Errorf("v3 的字段被改动了: %+v", list[0])
	}

	// 补上的列要真能写
	list[0].Kind = "dingtalk"
	list[0].ChannelSecret = "SECabc"
	list[0].ChannelTarget = "123456"
	if err := st.SaveSource(list[0]); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetSource(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ChannelSecret != "SECabc" || again.ChannelTarget != "123456" {
		t.Errorf("升级后的库写不进 channel_* 字段: %+v", again)
	}
}

// 内置渠道只能发送，不能接收 —— 界面上把它们放进接收源栏是走不通的。
func TestChannelKindsAreSendOnly(t *testing.T) {
	for _, kind := range ChannelKinds {
		s := &Source{Kind: kind, Usage: "both"}
		if s.CanReceive() {
			t.Errorf("%s 不该能接收", kind)
		}
		if !s.CanSend() {
			t.Errorf("%s 应当能发送", kind)
		}
		if !IsChannelKind(kind) || !IsSendOnlyKind(kind) {
			t.Errorf("%s 应当被认成内置渠道/只能发送", kind)
		}
		if KindLabel(kind) == kind {
			t.Errorf("%s 没有中文名", kind)
		}
	}
	if IsChannelKind("webhook") || IsSendOnlyKind("email") {
		t.Error("webhook / email 不该被当成内置渠道")
	}
	if len(AllKinds) != 3+len(ChannelKinds) {
		t.Errorf("类型列表应当是 3 个通用类型 + %d 个渠道，实际 %d 个", len(ChannelKinds), len(AllKinds))
	}
}

// 从 v4 升上来的老库：源上的默认模板两列由 v5 的 ALTER 补上，老数据不能动。
func TestMigrateAddsDefaultTemplateColumnsToExistingDB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f2a.db")

	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmts := range migrations[:4] {
		for _, stmt := range stmts {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 4`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sources (name, kind, usage, enabled, url, http_method, headers, channel_secret, channel_target)
		VALUES ('老钉钉源', 'dingtalk', 'out', 1, 'https://oapi.dingtalk.com/robot/send?access_token=t', 'POST', '{}', 'SEC', '123')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(dir)
	if err != nil {
		t.Fatalf("升级 v4 老库失败: %v", err)
	}
	defer st.Close()

	list, err := st.ListSources()
	if err != nil {
		t.Fatalf("升级后读不出老库的源: %v", err)
	}
	if len(list) != 1 || list[0].Name != "老钉钉源" {
		t.Fatalf("迁移把老数据弄丢了: %+v", list)
	}
	if list[0].DefaultBodyTemplate != "" || list[0].DefaultSubjectTemplate != "" {
		t.Errorf("老数据的默认模板应当是空的: %+v", list[0])
	}
	if list[0].ChannelSecret != "SEC" || list[0].ChannelTarget != "123" {
		t.Errorf("v4 的字段被改动了: %+v", list[0])
	}

	// 补上的列要真能写
	list[0].Kind = "webhook"
	list[0].DefaultBodyTemplate = `{{.Payload.content}}`
	list[0].DefaultSubjectTemplate = `{{.Payload.title}}`
	if err := st.SaveSource(list[0]); err != nil {
		t.Fatal(err)
	}
	again, err := st.GetSource(list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.DefaultBodyTemplate != `{{.Payload.content}}` || again.DefaultSubjectTemplate != `{{.Payload.title}}` {
		t.Errorf("升级后的库写不进默认模板字段: %+v", again)
	}
}

// 时区设置：留空跟随系统，认得的名字解析得开，写错的名字退回系统时区（保存时会被拦下）。
func TestSettingsLocation(t *testing.T) {
	def := DefaultSettings()
	if def.Timezone != "" {
		t.Errorf("默认应当是跟随系统（空串），实际 %q", def.Timezone)
	}
	if got := def.Location(); got != time.Local {
		t.Errorf("留空时应当返回系统时区，实际 %v", got)
	}

	sh := &Settings{Timezone: "Asia/Shanghai"}
	if got := sh.Location().String(); got != "Asia/Shanghai" {
		t.Errorf("Asia/Shanghai 应当解析成对应时区，实际 %q", got)
	}
	// 故意写错：不能 panic，也不能让页面打不开
	bad := &Settings{Timezone: "Asia/NotACity"}
	if got := bad.Location(); got != time.Local {
		t.Errorf("坏名字应当退回系统时区，实际 %v", got)
	}
	// 前后空白要容忍（表单里粘贴过来常见）
	if got := (&Settings{Timezone: "  Asia/Tokyo  "}).Location().String(); got != "Asia/Tokyo" {
		t.Errorf("时区名两边的空白应当忽略，实际 %q", got)
	}
}

// 时区要能存能读。
func TestSaveTimezoneSetting(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.Timezone != "" {
		t.Fatalf("新库的时区应当是空的，实际 %q", s.Timezone)
	}
	s.Timezone = "Asia/Shanghai"
	if err := st.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	again, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if again.Timezone != "Asia/Shanghai" {
		t.Errorf("时区没存下来，实际 %q", again.Timezone)
	}
	if again.Location().String() != "Asia/Shanghai" {
		t.Errorf("读回来的时区解析不对：%v", again.Location())
	}
}

// Turnstile 三个设置项：默认关闭、能存能读、关掉时不要求密钥。
func TestTurnstileSettings(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	s, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.TurnstileEnabled || s.TurnstileSiteKey != "" || s.TurnstileSecret != "" {
		t.Fatalf("新库应当是关闭且没有密钥，实际 %+v", s)
	}

	s.TurnstileEnabled = true
	s.TurnstileSiteKey = "0xSITE"
	s.TurnstileSecret = "SEC"
	if err := st.SaveSettings(s); err != nil {
		t.Fatal(err)
	}
	again, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !again.TurnstileEnabled || again.TurnstileSiteKey != "0xSITE" || again.TurnstileSecret != "SEC" {
		t.Errorf("Turnstile 设置没存下来：%+v", again)
	}

	// 关掉开关，密钥要留着（下次再开不用重填）
	again.TurnstileEnabled = false
	if err := st.SaveSettings(again); err != nil {
		t.Fatal(err)
	}
	off, err := st.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if off.TurnstileEnabled {
		t.Error("关掉之后又变成开着了")
	}
	if off.TurnstileSiteKey != "0xSITE" || off.TurnstileSecret != "SEC" {
		t.Errorf("关掉开关不该把密钥清掉：%+v", off)
	}
}
