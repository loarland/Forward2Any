package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultAdminPassword 是首次启动时使用的默认管理员密码。
//
// 这么设计是为了让 `docker compose up -d` 之后立刻就能登录，零配置。
// 但默认密码本身是个风险：一个公网可达的后台配三位密码，等于把 webhook 密钥
// 和邮箱密码直接送人。所以配套做了两件事：
//  1. Settings.AdminPassDefault 记录了「当前还是默认密码」；
//  2. 只要还是默认密码，后台除设置页外的所有页面都会强制跳转到设置页要求改密。
//
// 想跳过这一切，启动时给 F2A_ADMIN_PASSWORD 设一个自己的密码即可。
const DefaultAdminPassword = "f2a"

// 设置键。集中定义，避免字符串字面量散落各处。
const (
	KeyWebPort             = "web_port"
	KeyAdminUser           = "admin_user"
	KeyAdminPassHash       = "admin_pass_hash"
	KeyAdminPassDefault    = "admin_pass_default"
	KeyBaseURL             = "base_url"
	KeyRetryMax            = "retry_max"
	KeyRetryBackoffSeconds = "retry_backoff_seconds"
	KeyPayloadMaxBytes     = "payload_max_bytes"
	KeyLogRetentionDays    = "log_retention_days"
	KeyThemeColor          = "theme_color"
	KeyThemeMode           = "theme_mode"
	KeyProxyType           = "proxy_type"
	KeyProxyAddr           = "proxy_addr"
	KeyTrustedOrigins      = "trusted_origins"
	KeyOriginCheck         = "origin_check"
	KeyTrustedProxies      = "trusted_proxies"
	KeyTimezone            = "timezone"
	KeyTurnstileEnabled    = "turnstile_enabled"
	KeyTurnstileSiteKey    = "turnstile_site_key"
	KeyTurnstileSecret     = "turnstile_secret"
)

// Settings 是设置页编辑的全部内容。
type Settings struct {
	WebPort             int
	AdminUser           string
	AdminPassHash       string
	AdminPassDefault    bool // 密码是否仍是默认值
	BaseURL             string
	RetryMax            int
	RetryBackoffSeconds int
	PayloadMaxBytes     int
	LogRetentionDays    int
	ThemeColor          string // palettes/ 下的配色名
	ThemeMode           string // auto / light / dark
	ProxyType           string // none / http / https / socks5
	ProxyAddr           string // host:port，可带 用户名:密码@
	TrustedOrigins      string // 允许列表，逗号或换行分隔
	OriginCheck         bool   // 是否校验写请求的 Origin/Referer
	TrustedProxies      string // 受信代理的 IP / 网段，逗号或换行分隔；空表示不信任任何代理
	Timezone            string // IANA 时区名，如 Asia/Shanghai；留空表示跟随系统

	// Cloudflare Turnstile（登录人机校验）。默认关闭：没配密钥时整页不加载任何外部脚本。
	TurnstileEnabled bool
	TurnstileSiteKey string // 站点密钥，会写进登录页，公开的
	TurnstileSecret  string // 密钥，只留在服务端

	// raw 保留数据库原始键值，用于区分「从未设置」与「显式设成默认值」。
	raw settingsRaw
}

func DefaultSettings() *Settings {
	return &Settings{
		WebPort:             16000,
		AdminUser:           "admin",
		BaseURL:             "http://localhost:16000",
		RetryMax:            5,
		RetryBackoffSeconds: 10,
		PayloadMaxBytes:     65536,
		LogRetentionDays:    30,
		ThemeColor:          "blue",
		ThemeMode:           "auto",
		ProxyType:           "none",
		ProxyAddr:           "",
		TrustedOrigins:      "",
		OriginCheck:         true,
		TrustedProxies:      "",
		Timezone:            "",
	}
}

type BootstrapInput struct {
	Port      int
	AdminUser string
	AdminPass string
	BaseURL   string
}

// Bootstrap 在首次启动时把环境变量写入设置表；已存在的键一律不覆盖。
//
// 返回值 usingDefaultPassword 表示当前用的还是默认密码，由调用方提示用户。
// 传了 in.AdminPass 就不会用默认密码。
func (s *Store) Bootstrap(in BootstrapInput) (usingDefaultPassword bool, err error) {
	exist, err := s.Settings()
	if err != nil {
		return false, err
	}

	seed := map[string]string{}
	// 判断「是否首次启动」必须看原始键值：Settings() 会给缺失的键填上默认值，
	// 拿解析后的值判断的话，环境变量永远种不进去。
	setIfMissing := func(key, value string) {
		if _, ok := exist.raw[key]; !ok {
			seed[key] = value
		}
	}
	setIfMissing(KeyWebPort, strconv.Itoa(in.Port))
	setIfMissing(KeyAdminUser, in.AdminUser)
	setIfMissing(KeyBaseURL, in.BaseURL)
	setIfMissing(KeyRetryMax, strconv.Itoa(exist.RetryMax))
	setIfMissing(KeyRetryBackoffSeconds, strconv.Itoa(exist.RetryBackoffSeconds))
	setIfMissing(KeyPayloadMaxBytes, strconv.Itoa(exist.PayloadMaxBytes))
	setIfMissing(KeyLogRetentionDays, strconv.Itoa(exist.LogRetentionDays))
	setIfMissing(KeyThemeColor, exist.ThemeColor)
	setIfMissing(KeyThemeMode, exist.ThemeMode)
	setIfMissing(KeyProxyType, exist.ProxyType)
	setIfMissing(KeyProxyAddr, exist.ProxyAddr)
	setIfMissing(KeyTrustedOrigins, exist.TrustedOrigins)
	setIfMissing(KeyOriginCheck, boolStr(exist.OriginCheck))
	setIfMissing(KeyTrustedProxies, exist.TrustedProxies)
	setIfMissing(KeyTimezone, exist.Timezone)
	setIfMissing(KeyTurnstileEnabled, boolStr(exist.TurnstileEnabled))
	setIfMissing(KeyTurnstileSiteKey, exist.TurnstileSiteKey)
	setIfMissing(KeyTurnstileSecret, exist.TurnstileSecret)

	if exist.AdminPassHash == "" {
		pw := in.AdminPass
		if pw == "" {
			pw = DefaultAdminPassword
			seed[KeyAdminPassDefault] = "1"
		} else {
			seed[KeyAdminPassDefault] = "0"
		}
		hash, err := HashPassword(pw)
		if err != nil {
			return false, err
		}
		seed[KeyAdminPassHash] = hash
	}

	if err := s.SetSettings(seed); err != nil {
		return false, err
	}
	after, err := s.Settings()
	if err != nil {
		return false, err
	}
	return after.AdminPassDefault, nil
}

func (s *Store) Settings() (*Settings, error) {
	raw, err := s.rawSettings()
	if err != nil {
		return nil, err
	}
	d := DefaultSettings()
	get := func(k, def string) string {
		if v, ok := raw[k]; ok && v != "" {
			return v
		}
		return def
	}
	d.WebPort = atoi(get(KeyWebPort, strconv.Itoa(d.WebPort)), d.WebPort)
	d.AdminUser = get(KeyAdminUser, d.AdminUser)
	d.AdminPassHash = raw[KeyAdminPassHash]
	d.AdminPassDefault = raw[KeyAdminPassDefault] == "1"
	d.BaseURL = get(KeyBaseURL, d.BaseURL)
	d.RetryMax = atoi(get(KeyRetryMax, strconv.Itoa(d.RetryMax)), d.RetryMax)
	d.RetryBackoffSeconds = atoi(get(KeyRetryBackoffSeconds, strconv.Itoa(d.RetryBackoffSeconds)), d.RetryBackoffSeconds)
	d.PayloadMaxBytes = atoi(get(KeyPayloadMaxBytes, strconv.Itoa(d.PayloadMaxBytes)), d.PayloadMaxBytes)
	d.LogRetentionDays = atoi(get(KeyLogRetentionDays, strconv.Itoa(d.LogRetentionDays)), d.LogRetentionDays)
	d.ThemeColor = get(KeyThemeColor, d.ThemeColor)
	d.ThemeMode = get(KeyThemeMode, d.ThemeMode)
	d.ProxyType = get(KeyProxyType, d.ProxyType)
	d.ProxyAddr = get(KeyProxyAddr, d.ProxyAddr)
	d.TrustedOrigins = get(KeyTrustedOrigins, d.TrustedOrigins)
	d.OriginCheck = get(KeyOriginCheck, "1") == "1"
	d.TrustedProxies = strings.TrimSpace(raw[KeyTrustedProxies])
	// 时区没有「默认值」可言：留空就是跟随系统，所以不能用 get 的兜底语义。
	d.Timezone = strings.TrimSpace(raw[KeyTimezone])
	// Turnstile 默认关闭（raw 里没有这个键就是关），密钥按原样存，不做 trim 之外的加工。
	d.TurnstileEnabled = raw[KeyTurnstileEnabled] == "1"
	d.TurnstileSiteKey = strings.TrimSpace(raw[KeyTurnstileSiteKey])
	d.TurnstileSecret = strings.TrimSpace(raw[KeyTurnstileSecret])
	d.raw = raw
	return d, nil
}

// ProxyURL 把代理设置拼成 http.Transport 认得的形式；没启用代理时返回空串。
//
// 三种类型都由标准库直接支持（socks5 也支持，账号密码写在地址里即可），
// 所以这里不需要额外依赖。地址的合法性在设置页保存时校验。
func (s *Settings) ProxyURL() string {
	switch s.ProxyType {
	case "http", "https", "socks5":
	default:
		return ""
	}
	if s.ProxyAddr == "" {
		return ""
	}
	return s.ProxyType + "://" + s.ProxyAddr
}

// raw 保存数据库里的原始键值，用于区分「未设置」与「设成了默认值」。
type settingsRaw map[string]string

func (s *Store) rawSettings() (settingsRaw, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings`)
	if err != nil {
		return nil, fmt.Errorf("读取设置: %w", err)
	}
	defer rows.Close()
	out := settingsRaw{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SetSettings(kv map[string]string) error {
	if len(kv) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := setSettings(tx, kv); err != nil {
		return err
	}
	return tx.Commit()
}

// setSettings 是键值写入的实现，跟着调用方的事务走 ——
// 配置导入要把设置和源、规则写在同一个事务里，不能各写各的。
func setSettings(db execer, kv map[string]string) error {
	for k, v := range kv {
		if _, err := db.Exec(
			`INSERT INTO settings(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return fmt.Errorf("写入设置 %s: %w", k, err)
		}
	}
	return nil
}

func (s *Store) SetSetting(key, value string) error {
	return s.SetSettings(map[string]string{key: value})
}

// SaveSettings 持久化设置页提交的全部字段。
func (s *Store) SaveSettings(v *Settings) error {
	return s.SetSettings(map[string]string{
		KeyWebPort:             strconv.Itoa(v.WebPort),
		KeyAdminUser:           v.AdminUser,
		KeyAdminPassHash:       v.AdminPassHash,
		KeyAdminPassDefault:    boolStr(v.AdminPassDefault),
		KeyBaseURL:             v.BaseURL,
		KeyRetryMax:            strconv.Itoa(v.RetryMax),
		KeyRetryBackoffSeconds: strconv.Itoa(v.RetryBackoffSeconds),
		KeyPayloadMaxBytes:     strconv.Itoa(v.PayloadMaxBytes),
		KeyLogRetentionDays:    strconv.Itoa(v.LogRetentionDays),
		KeyThemeColor:          v.ThemeColor,
		KeyThemeMode:           v.ThemeMode,
		KeyProxyType:           v.ProxyType,
		KeyProxyAddr:           v.ProxyAddr,
		KeyTrustedOrigins:      v.TrustedOrigins,
		KeyOriginCheck:         boolStr(v.OriginCheck),
		KeyTrustedProxies:      v.TrustedProxies,
		KeyTimezone:            v.Timezone,
		KeyTurnstileEnabled:    boolStr(v.TurnstileEnabled),
		KeyTurnstileSiteKey:    v.TurnstileSiteKey,
		KeyTurnstileSecret:     v.TurnstileSecret,
	})
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// RandomHex 生成 n 字节的随机十六进制串（2n 个字符）。
func RandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("生成随机数: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// 让 database/sql 的哨兵错误与包内错误统一。
func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

// 时区缓存：LoadLocation 每次都要去找并解析 tzdata，而展示层是按行调用的，
// 同一个名字解析一次就够了。
var locCache sync.Map // map[string]*time.Location

// Location 返回设置里配的时区。
//
// 留空表示跟随系统（容器里通常是 UTC）；名字不认识时也退回系统时区 ——
// 保存时已经校验过，这里的兜底只是为了别让界面因为一个坏值整页打不开。
func (s *Settings) Location() *time.Location {
	name := strings.TrimSpace(s.Timezone)
	if name == "" {
		return time.Local
	}
	if v, ok := locCache.Load(name); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.Local
	}
	locCache.Store(name, loc)
	return loc
}
