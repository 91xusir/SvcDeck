package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Settings 是全局设置。存放在 settings(key/value) 表里，每个字段一行。
type Settings struct {
	ProbeIntervalSec   int    `json:"probe_interval_sec"`
	FailThreshold      int    `json:"fail_threshold"`
	WebhookURL         string `json:"webhook_url"`
	ListenAddr         string `json:"listen_addr"`
	Token              string `json:"token"`
	WaitDeps           bool   `json:"wait_deps"`
	StopGraceSec       int    `json:"stop_grace_sec"`
	LogMaxMB           int    `json:"log_max_mb"`
	AllowDeleteRunning bool   `json:"allow_delete_running"`
	ProbeHistoryLimit  int    `json:"probe_history_limit"`
}

func defaultSettings() Settings {
	return Settings{
		ProbeIntervalSec:   5,
		FailThreshold:      3,
		WebhookURL:         "",
		ListenAddr:         "127.0.0.1:8088",
		Token:              "",
		WaitDeps:           false,
		StopGraceSec:       8,
		LogMaxMB:           10,
		AllowDeleteRunning: false,
		ProbeHistoryLimit:  200,
	}
}

func (s Settings) validate() error {
	if s.ProbeIntervalSec < 1 || s.ProbeIntervalSec > 3600 {
		return fmt.Errorf("probe_interval_sec 必须在 1..3600 之间")
	}
	if s.FailThreshold < 1 || s.FailThreshold > 100 {
		return fmt.Errorf("fail_threshold 必须在 1..100 之间")
	}
	if s.StopGraceSec < 1 || s.StopGraceSec > 120 {
		return fmt.Errorf("stop_grace_sec 必须在 1..120 之间")
	}
	if s.LogMaxMB < 1 || s.LogMaxMB > 1024 {
		return fmt.Errorf("log_max_mb 必须在 1..1024 之间")
	}
	if s.ProbeHistoryLimit < 0 || s.ProbeHistoryLimit > 10000 {
		return fmt.Errorf("probe_history_limit 必须在 0..10000 之间")
	}
	if s.WebhookURL != "" {
		u, err := url.Parse(s.WebhookURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("webhook_url 必须是 http(s) 地址")
		}
	}
	if s.ListenAddr == "" {
		return fmt.Errorf("listen_addr 不能为空")
	}
	if _, portStr, err := net.SplitHostPort(s.ListenAddr); err != nil {
		return fmt.Errorf("listen_addr 必须形如 127.0.0.1:8088（%v）", err)
	} else if p, err := strconv.Atoi(portStr); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("listen_addr 端口必须在 1..65535")
	}
	if len(s.Token) > 256 {
		return fmt.Errorf("token 过长（最多 256 字符）")
	}
	return nil
}

// ConfigStore 提供无锁读取的配置快照，配置变更后立即生效（监听地址除外，需重启进程）。
type ConfigStore struct {
	mu  sync.RWMutex
	cur Settings
}

func NewConfigStore(s Settings) *ConfigStore { return &ConfigStore{cur: s} }

func (c *ConfigStore) Get() Settings {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cur
}

func (c *ConfigStore) Set(s Settings) {
	c.mu.Lock()
	c.cur = s
	c.mu.Unlock()
}

func configToRows(s Settings) map[string]string {
	return map[string]string{
		"probe_interval_sec":   strconv.Itoa(s.ProbeIntervalSec),
		"fail_threshold":       strconv.Itoa(s.FailThreshold),
		"webhook_url":          s.WebhookURL,
		"listen_addr":          s.ListenAddr,
		"token":                s.Token,
		"wait_deps":            strconv.FormatBool(s.WaitDeps),
		"stop_grace_sec":       strconv.Itoa(s.StopGraceSec),
		"log_max_mb":           strconv.Itoa(s.LogMaxMB),
		"allow_delete_running": strconv.FormatBool(s.AllowDeleteRunning),
		"probe_history_limit":  strconv.Itoa(s.ProbeHistoryLimit),
	}
}

func settingsFromRows(rows map[string]string) Settings {
	s := defaultSettings()
	setInt := func(key string, dst *int) {
		if v, ok := rows[key]; ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				*dst = n
			}
		}
	}
	setBool := func(key string, dst *bool) {
		if v, ok := rows[key]; ok {
			if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
				*dst = b
			}
		}
	}
	setInt("probe_interval_sec", &s.ProbeIntervalSec)
	setInt("fail_threshold", &s.FailThreshold)
	setInt("stop_grace_sec", &s.StopGraceSec)
	setInt("log_max_mb", &s.LogMaxMB)
	setInt("probe_history_limit", &s.ProbeHistoryLimit)
	setBool("wait_deps", &s.WaitDeps)
	setBool("allow_delete_running", &s.AllowDeleteRunning)
	if v, ok := rows["webhook_url"]; ok {
		s.WebhookURL = strings.TrimSpace(v)
	}
	if v, ok := rows["listen_addr"]; ok && strings.TrimSpace(v) != "" {
		s.ListenAddr = strings.TrimSpace(v)
	}
	if v, ok := rows["token"]; ok {
		s.Token = strings.TrimSpace(v)
	}
	return s
}

// settingsFromJSON 用于 /api/config 的 PUT 请求；未提供的字段会保留当前值。
func settingsFromJSON(raw []byte, cur Settings) (Settings, error) {
	out := cur
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return out, fmt.Errorf("配置 JSON 无效: %w", err)
	}
	return out, out.validate()
}
