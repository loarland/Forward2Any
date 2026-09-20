package store

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"strconv"
)

// 设置键。集中定义，避免字符串字面量散落各处。
const (
	KeyWebPort             = "web_port"
	KeyAdminUser           = "admin_user"
	KeyAdminPassHash       = "admin_pass_hash"
	KeyBaseURL             = "base_url"
	KeyRetryMax            = "retry_max"
	KeyRetryBackoffSeconds = "retry_backoff_seconds"
	KeyPayloadMaxBytes     = "payload_max_bytes"
	KeyLogRetentionDays    = "log_retention_days"
)

// Settings 是设置页编辑的全部内容。
type Settings struct {
	WebPort             int
	AdminUser           string
	AdminPassHash       string
	BaseURL             string
	RetryMax            int
	RetryBackoffSeconds int
	PayloadMaxBytes     int
	LogRetentionDays    int

	// raw 保留数据库原始键值，用于区分「从未设置」与「显式设成默认值」。
	raw settingsRaw
}

func DefaultSettings() *Settings {
	return &Settings{
		WebPort:             8080,
		AdminUser:           "admin",
		BaseURL:             "http://localhost:8080",
		RetryMax:            5,
		RetryBackoffSeconds: 10,
		PayloadMaxBytes:     65536,
		LogRetentionDays:    30,
	}
}

type BootstrapInput struct {
	Port      int
	AdminUser string
	AdminPass string
	BaseURL   string
}

// Bootstrap 在首次启动时把环境变量写入设置表；已存在的键一律不覆盖。
// 若管理员密码既无环境变量也无历史记录，则随机生成并返回，由调用方打印一次。
func (s *Store) Bootstrap(in BootstrapInput) (generatedPassword string, err error) {
	exist, err := s.Settings()
	if err != nil {
		return "", err
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

	if exist.AdminPassHash == "" {
		pw := in.AdminPass
		if pw == "" {
			pw, err = RandomPassword(16)
			if err != nil {
				return "", err
			}
			generatedPassword = pw
		}
		hash, err := HashPassword(pw)
		if err != nil {
			return "", err
		}
		seed[KeyAdminPassHash] = hash
	}

	return generatedPassword, s.SetSettings(seed)
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
	d.BaseURL = get(KeyBaseURL, d.BaseURL)
	d.RetryMax = atoi(get(KeyRetryMax, strconv.Itoa(d.RetryMax)), d.RetryMax)
	d.RetryBackoffSeconds = atoi(get(KeyRetryBackoffSeconds, strconv.Itoa(d.RetryBackoffSeconds)), d.RetryBackoffSeconds)
	d.PayloadMaxBytes = atoi(get(KeyPayloadMaxBytes, strconv.Itoa(d.PayloadMaxBytes)), d.PayloadMaxBytes)
	d.LogRetentionDays = atoi(get(KeyLogRetentionDays, strconv.Itoa(d.LogRetentionDays)), d.LogRetentionDays)
	d.raw = raw
	return d, nil
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
	for k, v := range kv {
		if _, err := tx.Exec(
			`INSERT INTO settings(key, value) VALUES(?, ?)
			 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return fmt.Errorf("写入设置 %s: %w", k, err)
		}
	}
	return tx.Commit()
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
		KeyBaseURL:             v.BaseURL,
		KeyRetryMax:            strconv.Itoa(v.RetryMax),
		KeyRetryBackoffSeconds: strconv.Itoa(v.RetryBackoffSeconds),
		KeyPayloadMaxBytes:     strconv.Itoa(v.PayloadMaxBytes),
		KeyLogRetentionDays:    strconv.Itoa(v.LogRetentionDays),
	})
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

// RandomPassword 生成 n 位随机密码，字母表去掉了易混字符（0/O、1/l/I）。
func RandomPassword(n int) (string, error) {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("生成随机密码: %w", err)
		}
		out[i] = alphabet[idx.Int64()]
	}
	return string(out), nil
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
