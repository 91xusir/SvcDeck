package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Service 是一条业务服务配置，对应 services 表的一行。
// 所有 JSON 列（args/env/depends_on/restart_backoff_sec）在 DB 里都以 TEXT 存放，
// API 输入输出则使用原生 JSON 类型，便于前端直接使用。
type Service struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Group             string            `json:"group"`
	Enabled           bool              `json:"enabled"`
	Exe               string            `json:"exe"`
	Args              []string          `json:"args"`
	Cwd               string            `json:"cwd"`
	Env               map[string]string `json:"env"`
	DependsOn         []string          `json:"depends_on"`
	ProbeType         string            `json:"probe_type"`
	ProbeURL          string            `json:"probe_url"`
	ProbeHost         string            `json:"probe_host"`
	ProbePort         int               `json:"probe_port"`
	ProbeKeyword      string            `json:"probe_keyword"`
	ProbeExpect       int               `json:"probe_expect"`
	ProbeTimeoutSec   int               `json:"probe_timeout_sec"`
	RestartMaxRetries int               `json:"restart_max_retries"`
	RestartBackoffSec []int             `json:"restart_backoff_sec"`
	CreatedAt         string            `json:"created_at"`
	UpdatedAt         string            `json:"updated_at"`
}

// RuntimeState 对应 runtime_state 表。它是“接管”的依据：
// pid + process_start_time + exe_path 三者共同证明一个进程确实是本面板曾经拉起的那个进程。
type RuntimeState struct {
	ServiceID        string
	PID              uint32
	ProcessStartTime uint64 // Windows FILETIME（100ns，自 1601-01-01），0 表示未采集到
	ExePath          string
	LastUp           bool
	LastError        string
	ConsecutiveFails int
	Retries          int
	UpdatedAt        string

	CircuitOpen    bool
	ManualStop     bool
	NextRetryAt    int64 // unix 秒
	LastCheck      int64 // unix 秒
	StartSignature string
}

// RuntimeView 是暴露给前端的运行态投影。
type RuntimeView struct {
	Running          bool   `json:"running"`
	PID              uint32 `json:"pid"`
	State            string `json:"state"` // up / down / circuit / starting / stopped / disabled / unknown
	Up               bool   `json:"up"`
	RTMs             int64  `json:"rt_ms"`
	LastCheck        int64  `json:"last_check"`
	ConsecutiveFails int    `json:"consecutive_fails"`
	Retries          int    `json:"retries"`
	CircuitOpen      bool   `json:"circuit_open"`
	ManualStop       bool   `json:"manual_stop"`
	LastError        string `json:"last_error"`
	PendingRestart   bool   `json:"pending_restart"`
	ProbeDesc        string `json:"probe_desc"`
}

type ServiceView struct {
	Service
	Runtime RuntimeView `json:"runtime"`
}

type StatusItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Group     string `json:"group"`
	Enabled   bool   `json:"enabled"`
	ProbeType string `json:"probe_type"`
	RuntimeView
}

type ProbeHistoryItem struct {
	TS    int64  `json:"ts"`
	Up    bool   `json:"up"`
	RTMs  int64  `json:"rt_ms"`
	Error string `json:"error"`
}

func nowStamp() string { return time.Now().Format(time.RFC3339) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

func parseInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case int:
		return int64(n)
	default:
		return 0
	}
}

func decodeStringArray(raw string) []string {
	var out []string
	if raw == "" {
		return []string{}
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []string{}
	}
	return out
}

func decodeIntArray(raw string) []int {
	var out []int
	if raw == "" {
		return []int{}
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []int{}
	}
	return out
}

func decodeStringMap(raw string) map[string]string {
	out := map[string]string{}
	if raw == "" || raw == "null" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

// launchSignature 覆盖所有“进程启动后无法热改”的字段。
// 运行中的服务如果签名变化，前端/API 会提示需要 Restart 才能生效（面板不会擅自杀掉它）。
func launchSignature(s Service) string {
	h := sha256.New()
	fmt.Fprintf(h, "exe=%s\n", strings.ToLower(filepath.Clean(s.Exe)))
	fmt.Fprintf(h, "cwd=%s\n", strings.ToLower(filepath.Clean(s.Cwd)))
	fmt.Fprintf(h, "args=%s\n", jsonString(s.Args))
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, strings.ToUpper(k))
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "env:%s=%s\n", k, s.Env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
