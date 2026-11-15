package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store 包装 SQLite。连接数固定为 1：面板的写操作很频繁（每个探针周期都会写 runtime_state），
// 单连接可以把锁竞争降到零，同时 WAL 仍然保证崩溃安全。
type Store struct {
	db *sql.DB
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite 失败: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA foreign_keys=OFF;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("执行 %s 失败: %w", pragma, err)
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// CleanupOrphans 清理已删除服务的运行态/探测历史。
// 删除服务与在途探针可能并发，探针的 SaveRuntime 偶尔会留下孤儿行；启动时统一清掉。
func (s *Store) CleanupOrphans() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM runtime_state WHERE service_id NOT IN (SELECT id FROM services);`,
		`DELETE FROM probe_history WHERE service_id NOT IN (SELECT id FROM services);`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// migrate 只做“向前兼容”的建表迁移：新库一次建齐，旧库按 user_version 递增。
// 当前只有 v1，因此 IF NOT EXISTS 足够；后续加列时在这里追加 ALTER 分支即可。
func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version;").Scan(&version); err != nil {
		return fmt.Errorf("读取 user_version 失败: %w", err)
	}
	if version >= 1 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS services (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			group_name TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			exe TEXT NOT NULL,
			args TEXT NOT NULL DEFAULT '[]',
			cwd TEXT NOT NULL DEFAULT '',
			env TEXT NOT NULL DEFAULT '{}',
			depends_on TEXT NOT NULL DEFAULT '[]',
			probe_type TEXT NOT NULL DEFAULT 'process',
			probe_url TEXT NOT NULL DEFAULT '',
			probe_host TEXT NOT NULL DEFAULT '127.0.0.1',
			probe_port INTEGER NOT NULL DEFAULT 0,
			probe_keyword TEXT NOT NULL DEFAULT '',
			probe_expect INTEGER NOT NULL DEFAULT 0,
			probe_timeout_sec INTEGER NOT NULL DEFAULT 5,
			restart_max_retries INTEGER NOT NULL DEFAULT 5,
			restart_backoff_sec TEXT NOT NULL DEFAULT '[1,2,5,30]',
			created_at TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS runtime_state (
			service_id TEXT PRIMARY KEY,
			pid INTEGER NOT NULL DEFAULT 0,
			process_start_time INTEGER NOT NULL DEFAULT 0,
			exe_path TEXT NOT NULL DEFAULT '',
			last_up INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			consecutive_fails INTEGER NOT NULL DEFAULT 0,
			retries INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT '',
			circuit_open INTEGER NOT NULL DEFAULT 0,
			manual_stop INTEGER NOT NULL DEFAULT 0,
			next_retry_at INTEGER NOT NULL DEFAULT 0,
			last_check INTEGER NOT NULL DEFAULT 0,
			start_signature TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS probe_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			service_id TEXT NOT NULL,
			ts INTEGER NOT NULL,
			up INTEGER NOT NULL,
			rt_ms INTEGER NOT NULL DEFAULT 0,
			err TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_probe_history_service ON probe_history(service_id, id);`,
		`CREATE INDEX IF NOT EXISTS idx_services_enabled ON services(enabled, id);`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}
	if _, err := tx.Exec("PRAGMA user_version = 1;"); err != nil {
		return err
	}
	return tx.Commit()
}

const serviceColumns = `id, name, group_name, enabled, exe, args, cwd, env, depends_on,
	probe_type, probe_url, probe_host, probe_port, probe_keyword, probe_expect,
	probe_timeout_sec, restart_max_retries, restart_backoff_sec, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanService(sc rowScanner) (Service, error) {
	var (
		s                                 Service
		enabled                           int
		args, env, deps, backoff, keyword string
	)
	err := sc.Scan(&s.ID, &s.Name, &s.Group, &enabled, &s.Exe, &args, &s.Cwd, &env, &deps,
		&s.ProbeType, &s.ProbeURL, &s.ProbeHost, &s.ProbePort, &keyword, &s.ProbeExpect,
		&s.ProbeTimeoutSec, &s.RestartMaxRetries, &backoff, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return s, err
	}
	s.Enabled = enabled != 0
	s.Args = decodeStringArray(args)
	s.Env = decodeStringMap(env)
	s.DependsOn = decodeStringArray(deps)
	s.RestartBackoffSec = decodeIntArray(backoff)
	s.ProbeKeyword = keyword
	return s, nil
}

func (s *Store) ListServices() ([]Service, error) {
	rows, err := s.db.Query(`SELECT ` + serviceColumns + ` FROM services ORDER BY group_name, name, id;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Service
	for rows.Next() {
		svc, err := scanService(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (s *Store) GetService(id string) (Service, bool, error) {
	row := s.db.QueryRow(`SELECT `+serviceColumns+` FROM services WHERE id = ?;`, id)
	svc, err := scanService(row)
	if err == sql.ErrNoRows {
		return Service{}, false, nil
	}
	if err != nil {
		return Service{}, false, err
	}
	return svc, true, nil
}

func (s *Store) InsertService(v Service) error {
	_, err := s.db.Exec(`INSERT INTO services (`+serviceColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?);`,
		v.ID, v.Name, v.Group, boolInt(v.Enabled), v.Exe, jsonString(v.Args), v.Cwd,
		jsonString(v.Env), jsonString(v.DependsOn), v.ProbeType, v.ProbeURL,
		v.ProbeHost, v.ProbePort, v.ProbeKeyword, v.ProbeExpect, v.ProbeTimeoutSec,
		v.RestartMaxRetries, jsonString(v.RestartBackoffSec), v.CreatedAt, v.UpdatedAt)
	return err
}

func (s *Store) UpdateService(v Service) error {
	_, err := s.db.Exec(`UPDATE services SET
		name=?, group_name=?, enabled=?, exe=?, args=?, cwd=?, env=?, depends_on=?,
		probe_type=?, probe_url=?, probe_host=?, probe_port=?, probe_keyword=?, probe_expect=?,
		probe_timeout_sec=?, restart_max_retries=?, restart_backoff_sec=?, updated_at=?
		WHERE id=?;`,
		v.Name, v.Group, boolInt(v.Enabled), v.Exe, jsonString(v.Args), v.Cwd,
		jsonString(v.Env), jsonString(v.DependsOn), v.ProbeType, v.ProbeURL,
		v.ProbeHost, v.ProbePort, v.ProbeKeyword, v.ProbeExpect, v.ProbeTimeoutSec,
		v.RestartMaxRetries, jsonString(v.RestartBackoffSec), v.UpdatedAt, v.ID)
	return err
}

func (s *Store) DeleteService(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM runtime_state WHERE service_id=?;`,
		`DELETE FROM probe_history WHERE service_id=?;`,
		`DELETE FROM services WHERE id=?;`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- runtime_state ----

func scanRuntime(sc rowScanner) (RuntimeState, error) {
	var (
		st                      RuntimeState
		pid                     int64
		startTime               int64
		lastUp, circuit, manual int
	)
	err := sc.Scan(&st.ServiceID, &pid, &startTime, &st.ExePath, &lastUp, &st.LastError,
		&st.ConsecutiveFails, &st.Retries, &st.UpdatedAt, &circuit, &manual,
		&st.NextRetryAt, &st.LastCheck, &st.StartSignature)
	if err != nil {
		return st, err
	}
	if pid < 0 {
		pid = 0
	}
	if startTime < 0 {
		startTime = 0
	}
	st.PID = uint32(pid)
	st.ProcessStartTime = uint64(startTime)
	st.LastUp = lastUp != 0
	st.CircuitOpen = circuit != 0
	st.ManualStop = manual != 0
	return st, nil
}

const runtimeColumns = `service_id, pid, process_start_time, exe_path, last_up, last_error,
	consecutive_fails, retries, updated_at, circuit_open, manual_stop, next_retry_at,
	last_check, start_signature`

func (s *Store) LoadRuntime(id string) (RuntimeState, bool, error) {
	row := s.db.QueryRow(`SELECT `+runtimeColumns+` FROM runtime_state WHERE service_id=?;`, id)
	st, err := scanRuntime(row)
	if err == sql.ErrNoRows {
		return RuntimeState{}, false, nil
	}
	if err != nil {
		return RuntimeState{}, false, err
	}
	return st, true, nil
}

func (s *Store) LoadAllRuntime() (map[string]RuntimeState, error) {
	rows, err := s.db.Query(`SELECT ` + runtimeColumns + ` FROM runtime_state;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RuntimeState{}
	for rows.Next() {
		st, err := scanRuntime(rows)
		if err != nil {
			return nil, err
		}
		out[st.ServiceID] = st
	}
	return out, rows.Err()
}

func (s *Store) SaveRuntime(st RuntimeState) error {
	_, err := s.db.Exec(`INSERT INTO runtime_state (`+runtimeColumns+`)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(service_id) DO UPDATE SET
			pid=excluded.pid,
			process_start_time=excluded.process_start_time,
			exe_path=excluded.exe_path,
			last_up=excluded.last_up,
			last_error=excluded.last_error,
			consecutive_fails=excluded.consecutive_fails,
			retries=excluded.retries,
			updated_at=excluded.updated_at,
			circuit_open=excluded.circuit_open,
			manual_stop=excluded.manual_stop,
			next_retry_at=excluded.next_retry_at,
			last_check=excluded.last_check,
			start_signature=excluded.start_signature;`,
		st.ServiceID, int64(st.PID), int64(st.ProcessStartTime), st.ExePath, boolInt(st.LastUp),
		st.LastError, st.ConsecutiveFails, st.Retries, st.UpdatedAt, boolInt(st.CircuitOpen),
		boolInt(st.ManualStop), st.NextRetryAt, st.LastCheck, st.StartSignature)
	return err
}

// ---- settings ----

func (s *Store) LoadSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM settings;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) SaveSettings(rows map[string]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for k, v := range rows {
		if _, err := tx.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value;`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ---- probe_history（可选，保留最近 N 条/服务）----

func (s *Store) AppendProbeHistory(serviceID string, up bool, rtMs int64, errText string, limit int) error {
	if limit <= 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO probe_history(service_id, ts, up, rt_ms, err)
		VALUES(?,?,?,?,?);`, serviceID, time.Now().UnixMilli(), boolInt(up), rtMs, errText); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM probe_history WHERE service_id=? AND id NOT IN (
		SELECT id FROM probe_history WHERE service_id=? ORDER BY id DESC LIMIT ?);`,
		serviceID, serviceID, limit); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ProbeHistory(serviceID string, limit int) ([]ProbeHistoryItem, error) {
	if limit <= 0 || limit > 2000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT ts, up, rt_ms, err FROM probe_history
		WHERE service_id=? ORDER BY id DESC LIMIT ?;`, serviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProbeHistoryItem{}
	for rows.Next() {
		var item ProbeHistoryItem
		var up int
		if err := rows.Scan(&item.TS, &up, &item.RTMs, &item.Error); err != nil {
			return nil, err
		}
		item.Up = up != 0
		out = append(out, item)
	}
	return out, rows.Err()
}
