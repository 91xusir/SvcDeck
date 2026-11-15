package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *apiClient {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &apiClient{baseURL: ts.URL, hc: ts.Client()}
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func TestInitializeAndNotifications(t *testing.T) {
	s := &Server{}

	resp, ok := s.handleLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
	if !ok || resp.Error != nil {
		t.Fatalf("initialize failed: %+v", resp)
	}
	if got := resp.Result.(map[string]any)["protocolVersion"]; got != "2025-06-18" {
		t.Fatalf("protocolVersion = %v", got)
	}

	if _, ok := s.handleLine([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); ok {
		t.Fatal("notification should not produce a response")
	}
}

func TestToolsListContainsCoreTools(t *testing.T) {
	s := &Server{}
	resp, ok := s.handleLine([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	if !ok || resp.Error != nil {
		t.Fatalf("tools/list failed: %+v", resp)
	}
	result := resp.Result.(map[string]any)
	raw, _ := json.Marshal(result["tools"])
	var listed []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, item := range listed {
		names[item.Name] = true
	}
	for _, want := range []string{"add_service", "update_service", "list_services", "delete_service"} {
		if !names[want] {
			t.Fatalf("missing tool %s; got %v", want, names)
		}
	}
}

func TestAddServiceBuildsPayload(t *testing.T) {
	var captured map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/services" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writeTestJSON(w, http.StatusCreated, map[string]any{
			"ok": true,
			"service": map[string]any{
				"id": "order-api", "name": "订单服务",
			},
			"message": "服务已创建",
		})
	})

	args := map[string]any{
		"name":                "订单服务",
		"exe":                 `D:\Apps\order-api\order-api.exe`,
		"enabled":             false,
		"args":                []any{"--port=9001"},
		"env":                 map[string]any{"ASPNETCORE_ENVIRONMENT": "Production"},
		"probe_type":          "http",
		"probe_url":           "http://127.0.0.1:9001/health",
		"probe_expect":        float64(200),
		"restart_backoff_sec": []any{float64(1), float64(2), float64(5)},
	}

	text, err := addService(context.Background(), c, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "order-api") {
		t.Fatalf("unexpected output: %s", text)
	}
	if captured["name"] != "订单服务" {
		t.Fatalf("name = %v", captured["name"])
	}
	if captured["enabled"] != false {
		t.Fatalf("enabled = %v", captured["enabled"])
	}
	if captured["probe_type"] != "http" {
		t.Fatalf("probe_type = %v", captured["probe_type"])
	}
	if captured["probe_port"] != nil {
		t.Fatalf("probe_port should be omitted, got %v", captured["probe_port"])
	}
}

func TestUpdateServiceMergesExistingFields(t *testing.T) {
	var putBody map[string]any
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			writeTestJSON(w, http.StatusOK, map[string]any{
				"service": map[string]any{
					"id": "svc-1", "name": "old-name", "group": "old-group",
					"exe": `D:\Apps\svc-1\svc-1.exe`, "cwd": `D:\Apps\svc-1`,
					"enabled": true, "probe_type": "process",
					"runtime": map[string]any{"state": "up", "running": true},
				},
			})
		case http.MethodPut:
			if err := json.NewDecoder(r.Body).Decode(&putBody); err != nil {
				t.Errorf("decode body: %v", err)
			}
			writeTestJSON(w, http.StatusOK, map[string]any{
				"ok":              true,
				"service":         map[string]any{"id": "svc-1", "name": "new-name"},
				"pending_restart": false,
			})
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})

	text, err := updateService(context.Background(), c, map[string]any{"id": "svc-1", "name": "new-name"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "svc-1") {
		t.Fatalf("unexpected output: %s", text)
	}
	if putBody["name"] != "new-name" {
		t.Fatalf("name = %v", putBody["name"])
	}
	if putBody["group"] != "old-group" {
		t.Fatalf("group should be preserved, got %v", putBody["group"])
	}
	if putBody["exe"] != `D:\Apps\svc-1\svc-1.exe` {
		t.Fatalf("exe should be preserved, got %v", putBody["exe"])
	}
	if _, ok := putBody["runtime"]; ok {
		t.Fatal("runtime should not be sent back")
	}
}
func TestHTTPHandlerInitializeAndNotification(t *testing.T) {
	cfg := Config{BaseURL: "http://127.0.0.1:1", Timeout: time.Second}
	h, err := NewHTTPHandler(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`
	req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("initialize status = %d", resp.StatusCode)
	}
	var rpc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rpc); err != nil {
		t.Fatal(err)
	}
	if rpc["result"] == nil {
		t.Fatalf("initialize result missing: %v", rpc)
	}

	notif := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	req, _ = http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(notif))
	req.Header.Set("Content-Type", "application/json")
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notification status = %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL, nil)
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", resp.StatusCode)
	}
}
