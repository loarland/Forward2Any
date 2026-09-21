// Package store 是 SQLite 持久层：打开数据库、执行迁移、各表的 CRUD。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，注册名 "sqlite"
)

var ErrNotFound = errors.New("记录不存在")

// execer 同时被 *sql.DB 和 *sql.Tx 满足，
// 让同一段写入逻辑既能单独执行也能放进事务里。
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type Store struct {
	db   *sql.DB
	Path string
}

// migrations 按顺序累积，用 PRAGMA user_version 记录已应用到第几版。
var migrations = [][]string{
	{
		`CREATE TABLE sources (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			name          TEXT    NOT NULL,
			kind          TEXT    NOT NULL,
			usage         TEXT    NOT NULL DEFAULT 'in',
			enabled       INTEGER NOT NULL DEFAULT 1,
			slug          TEXT    NOT NULL DEFAULT '',
			url           TEXT    NOT NULL DEFAULT '',
			http_method   TEXT    NOT NULL DEFAULT 'POST',
			headers       TEXT    NOT NULL DEFAULT '{}',
			auth_mode     TEXT    NOT NULL DEFAULT 'none',
			auth_header   TEXT    NOT NULL DEFAULT '',
			auth_secret   TEXT    NOT NULL DEFAULT '',
			ip_allow      TEXT    NOT NULL DEFAULT '',
			smtp_host     TEXT    NOT NULL DEFAULT '',
			smtp_port     INTEGER NOT NULL DEFAULT 587,
			smtp_user     TEXT    NOT NULL DEFAULT '',
			smtp_pass     TEXT    NOT NULL DEFAULT '',
			smtp_tls      TEXT    NOT NULL DEFAULT 'starttls',
			mail_from     TEXT    NOT NULL DEFAULT '',
			mail_to       TEXT    NOT NULL DEFAULT '',
			imap_host     TEXT    NOT NULL DEFAULT '',
			imap_port     INTEGER NOT NULL DEFAULT 993,
			imap_user     TEXT    NOT NULL DEFAULT '',
			imap_pass     TEXT    NOT NULL DEFAULT '',
			imap_tls      INTEGER NOT NULL DEFAULT 1,
			imap_folder   TEXT    NOT NULL DEFAULT 'INBOX',
			imap_interval INTEGER NOT NULL DEFAULT 60,
			created_at    INTEGER NOT NULL DEFAULT 0,
			updated_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE UNIQUE INDEX idx_sources_slug ON sources(slug) WHERE slug <> ''`,

		`CREATE TABLE rules (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			name             TEXT    NOT NULL,
			enabled          INTEGER NOT NULL DEFAULT 1,
			from_source_ids  TEXT    NOT NULL DEFAULT '[]',
			to_source_ids    TEXT    NOT NULL DEFAULT '[]',
			filters          TEXT    NOT NULL DEFAULT '[]',
			body_template    TEXT    NOT NULL DEFAULT '',
			subject_template TEXT    NOT NULL DEFAULT '',
			headers_template TEXT    NOT NULL DEFAULT '{}',
			created_at       INTEGER NOT NULL DEFAULT 0,
			updated_at       INTEGER NOT NULL DEFAULT 0
		)`,

		`CREATE TABLE deliveries (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			trace_id      TEXT    NOT NULL DEFAULT '',
			rule_id       INTEGER NOT NULL DEFAULT 0,
			in_source_id  INTEGER NOT NULL DEFAULT 0,
			out_source_id INTEGER NOT NULL DEFAULT 0,
			hop_chain     TEXT    NOT NULL DEFAULT '',
			status        TEXT    NOT NULL DEFAULT 'pending',
			attempt       INTEGER NOT NULL DEFAULT 0,
			payload       TEXT    NOT NULL DEFAULT '',
			rendered      TEXT    NOT NULL DEFAULT '',
			subject       TEXT    NOT NULL DEFAULT '',
			req_headers   TEXT    NOT NULL DEFAULT '{}',
			response_code INTEGER NOT NULL DEFAULT 0,
			response_body TEXT    NOT NULL DEFAULT '',
			last_error    TEXT    NOT NULL DEFAULT '',
			next_retry_at INTEGER NOT NULL DEFAULT 0,
			created_at    INTEGER NOT NULL DEFAULT 0,
			updated_at    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX idx_deliveries_retry ON deliveries(status, next_retry_at)`,
		`CREATE INDEX idx_deliveries_created ON deliveries(created_at DESC)`,
		`CREATE INDEX idx_deliveries_trace ON deliveries(trace_id)`,

		`CREATE TABLE settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	},
	// v2：源上多一个「走代理发送」的开关。
	// 不能改 v1 —— 已经发出去的版本就是那个形状，已有部署的库停在 user_version=1，
	// 只有这里加一条 ALTER 才能把它们升级上来。
	{
		`ALTER TABLE sources ADD COLUMN use_proxy INTEGER NOT NULL DEFAULT 0`,
	},
	// v3：Telegram 发送源。
	// Chat ID 存字符串：它既可能是数字（群和频道的 id 是负数），也可能是 @channelusername。
	// 话题 ID 也存字符串，好区分「没填」和「填了 0」，顺便把用户填错的内容原样留着报错。
	{
		`ALTER TABLE sources ADD COLUMN tg_token TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sources ADD COLUMN tg_chat_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sources ADD COLUMN tg_thread_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE sources ADD COLUMN tg_endpoint TEXT NOT NULL DEFAULT ''`,
	},
}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	path := filepath.Join(dataDir, "f2a.db")

	// WAL + busy_timeout 让「后台读」和「投递写」可以并发；
	// _txlock=immediate 让写事务一开始就拿写锁，避免升级锁时 SQLITE_BUSY。
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_txlock=immediate"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}

	s := &Store{db: db, Path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	// 库里存着 webhook 密钥和邮箱密码，收紧权限。
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// Ping 供健康检查使用。
func (s *Store) Ping() error { return s.db.Ping() }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("读取 schema 版本: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("开始迁移事务: %w", err)
		}
		for _, stmt := range migrations[i] {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("迁移 v%d 失败: %w", i+1, err)
			}
		}
		// PRAGMA 不支持占位符，这里的值来自代码内的循环下标，不是外部输入。
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("写入 schema 版本: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交迁移 v%d: %w", i+1, err)
		}
	}
	return nil
}

func nowUnix() int64 { return time.Now().Unix() }
