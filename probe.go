package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type ProbeResult struct {
	Up         bool
	RTMs       int64
	Err        string
	Desc       string
	FoundPID   uint32
	FoundStart uint64
	FoundExe   string
}

func resolveProbePort(cfg Service) int {
	switch cfg.ProbeType {
	case "tcp":
		return cfg.ProbePort
	case "http":
		u, err := url.Parse(cfg.ProbeURL)
		if err != nil {
			return 0
		}
		if p := u.Port(); p != "" {
			n, _ := strconv.Atoi(p)
			return n
		}
		if u.Scheme == "https" {
			return 443
		}
		if u.Scheme == "http" {
			return 80
		}
	}
	return 0
}

func isLocalHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.Trim(h, "[]")
	switch h {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0":
		return true
	}
	return strings.HasPrefix(h, "127.")
}

// spawnPortGuardPort 返回本机端口；只有探测目标是本机时才需要在 spawn 前检查端口占用。
func spawnPortGuardPort(cfg Service) int {
	switch cfg.ProbeType {
	case "tcp":
		if isLocalHost(cfg.ProbeHost) {
			return cfg.ProbePort
		}
	case "http":
		u, err := url.Parse(cfg.ProbeURL)
		if err == nil && isLocalHost(u.Hostname()) {
			return resolveProbePort(cfg)
		}
	}
	return 0
}

func probeDescription(cfg Service) string {
	switch cfg.ProbeType {
	case "http":
		expect := "2xx/3xx"
		if cfg.ProbeExpect > 0 {
			expect = strconv.Itoa(cfg.ProbeExpect)
		}
		desc := fmt.Sprintf("HTTP GET %s · 期望 %s", cfg.ProbeURL, expect)
		if cfg.ProbeKeyword != "" {
			desc += fmt.Sprintf(" · 含 %q", cfg.ProbeKeyword)
		}
		return desc
	case "tcp":
		return fmt.Sprintf("TCP %s:%d", cfg.ProbeHost, cfg.ProbePort)
	default:
		return fmt.Sprintf("进程存在 · %s", filepath.Base(cfg.Exe))
	}
}

func probeTimeout(cfg Service) time.Duration {
	sec := cfg.ProbeTimeoutSec
	if sec <= 0 {
		sec = 5
	}
	return time.Duration(sec) * time.Second
}

func trimErr(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// probeService 是“最终真相”。adopt 判定先查 pid，但探针 up 时一律认为服务在跑，
// 绝不重复拉起；只有探针 down 才进入 spawn / 保活路径。
// allowExeFallback 为 false 时（exe 被多个服务共享），进程探针不会把“别人的同 exe 实例”
// 当成自己 UP，也不会按 exe 接管 pid。否则两个服务各跑一份同名 exe 会互相误认。
func probeService(ctx context.Context, client *http.Client, cfg Service, st RuntimeState, allowExeFallback bool) (res ProbeResult) {
	start := time.Now()
	timeout := probeTimeout(cfg)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res = ProbeResult{Desc: probeDescription(cfg)}
	// 命名返回值 + defer：让每条 return 路径都带上真实耗时。
	defer func() { res.RTMs = time.Since(start).Milliseconds() }()

	switch cfg.ProbeType {
	case "http":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.ProbeURL, nil)
		if err != nil {
			res.Err = trimErr(err.Error())
			return res
		}
		req.Header.Set("User-Agent", "SvcDeck-Probe/1.0")
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := client.Do(req)
		if err != nil {
			res.Err = trimErr(err.Error())
			if ctx.Err() == context.DeadlineExceeded {
				res.Err = fmt.Sprintf("探测超时（%s）", timeout)
			}
			return res
		}
		defer resp.Body.Close()
		if cfg.ProbeExpect > 0 {
			if resp.StatusCode != cfg.ProbeExpect {
				res.Err = fmt.Sprintf("HTTP %d，期望 %d", resp.StatusCode, cfg.ProbeExpect)
				return res
			}
		} else if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			res.Err = fmt.Sprintf("HTTP %d（期望 2xx/3xx）", resp.StatusCode)
			return res
		}
		if cfg.ProbeKeyword != "" {
			body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
			if err != nil {
				res.Err = "读取响应体失败: " + trimErr(err.Error())
				return res
			}
			if !strings.Contains(string(body), cfg.ProbeKeyword) {
				res.Err = fmt.Sprintf("响应体不包含关键词 %q", cfg.ProbeKeyword)
				return res
			}
		}
		res.Up = true
		return res

	case "tcp":
		d := net.Dialer{}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(cfg.ProbeHost, strconv.Itoa(cfg.ProbePort)))
		if err != nil {
			res.Err = trimErr(err.Error())
			if ctx.Err() == context.DeadlineExceeded {
				res.Err = fmt.Sprintf("连接超时（%s）", timeout)
			}
			return res
		}
		conn.Close()
		res.Up = true
		return res

	default: // process
		if st.PID != 0 {
			if ok, why := verifyProcess(st.PID, st.ExePath, st.ProcessStartTime); ok {
				res.Up = true
				res.Desc = fmt.Sprintf("进程存在 · pid %d · %s", st.PID, filepath.Base(st.ExePath))
				return res
			} else {
				res.Err = why
			}
		}
		if !allowExeFallback {
			if res.Err == "" {
				res.Err = "服务尚无已托管的 pid，且 exe 被多个服务共享，无法按 exe 归属"
			} else {
				res.Err += "；且 exe 被多个服务共享，无法按 exe 接管"
			}
			return res
		}
		pids := findProcessesByExe(cfg.Exe)
		if len(pids) == 0 {
			if res.Err == "" {
				res.Err = "进程不存在: " + filepath.Base(cfg.Exe)
			} else {
				res.Err += "；且按 exe 未找到存活进程"
			}
			return res
		}
		p := pids[0]
		res.Up = true
		res.FoundPID = p.PID
		res.FoundStart = p.StartTime
		res.FoundExe = p.Exe
		res.Desc = fmt.Sprintf("进程存在 · pid %d · %s", p.PID, filepath.Base(p.Exe))
		return res
	}
}
