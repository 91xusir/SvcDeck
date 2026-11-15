package main

import (
	"strings"
	"testing"
	"time"
)

// 禁用服务不参与探测：运行态投影应隐藏所有探测结果，只保留 PID 供手动 Stop。
func TestRuntimeViewDisabledHidesProbeState(t *testing.T) {
	s := &Supervisor{}
	cfg := Service{ID: "svc-1", Enabled: false, ProbeType: "process", Exe: `C:\Apps\svc-1.exe`}
	st := RuntimeState{
		PID:              1234,
		LastUp:           true,
		LastCheck:        1700000000,
		ConsecutiveFails: 4,
		Retries:          2,
		CircuitOpen:      true,
		LastError:        "旧探针错误",
	}

	view := s.runtimeView(cfg, st, 88, 1700000000, time.Time{}, time.Now(), false)

	if view.State != "disabled" {
		t.Fatalf("state = %q, want disabled", view.State)
	}
	if view.Up {
		t.Fatal("disabled view must not report up")
	}
	if view.LastCheck != 0 || view.RTMs != 0 || view.ConsecutiveFails != 0 || view.Retries != 0 || view.CircuitOpen {
		t.Fatalf("probe fields must be cleared: %+v", view)
	}
	if view.LastError != "" {
		t.Fatalf("last_error must be empty, got %q", view.LastError)
	}
	if !view.Running || view.PID != 1234 {
		t.Fatalf("pid should be kept for manual stop: %+v", view)
	}
}

func TestRuntimeViewEnabledStillReportsProbe(t *testing.T) {
	s := &Supervisor{}
	cfg := Service{ID: "svc-1", Enabled: true, ProbeType: "process", Exe: `C:\Apps\svc-1.exe`}
	st := RuntimeState{PID: 1, LastUp: true, LastCheck: 1700000000}

	view := s.runtimeView(cfg, st, 12, 1700000000, time.Time{}, time.Now(), false)

	if view.State != "up" || !view.Up || view.LastCheck != 1700000000 {
		t.Fatalf("enabled service should keep probe state: %+v", view)
	}
}

// 停止中的禁用服务仍应显示 stopping，避免 Stop 过程看起来没有反馈。
func TestRuntimeViewDisabledStillShowsStopping(t *testing.T) {
	s := &Supervisor{}
	cfg := Service{ID: "svc-1", Enabled: false, ProbeType: "process", Exe: `C:\Apps\svc-1.exe`}
	view := s.runtimeView(cfg, RuntimeState{PID: 1}, 0, 0, time.Time{}, time.Now(), true)
	if view.State != "stopping" {
		t.Fatalf("state = %q, want stopping", view.State)
	}
}

func TestTopoOrderDetectsCycle(t *testing.T) {
	services := []Service{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	}
	_, err := topoOrder(services, map[string]bool{"a": true, "b": true})
	if err == nil || !strings.Contains(err.Error(), "依赖成环") {
		t.Fatalf("cycle should be rejected, got %v", err)
	}
}

func TestTopoOrderPutsDependencyFirst(t *testing.T) {
	services := []Service{
		{ID: "web", DependsOn: []string{"db"}},
		{ID: "db"},
	}
	order, err := topoOrder(services, map[string]bool{"web": true, "db": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "db" || order[1] != "web" {
		t.Fatalf("order = %v, want [db web]", order)
	}
}
