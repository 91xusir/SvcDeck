package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type toolDef struct {
	name        string
	description string
	schema      map[string]any
	readOnly    bool
	destructive bool
	handler     func(context.Context, *apiClient, map[string]any) (string, error)
}

var tools = []toolDef{
	{
		name:        "list_services",
		description: "列出 SvcDeck 中所有服务及其运行态（摘要）。",
		schema:      objSchema(map[string]any{}),
		readOnly:    true,
		handler:     listServices,
	},
	{
		name:        "get_service",
		description: "读取单个服务的完整配置和运行态。",
		schema:      objSchema(map[string]any{"id": idProp()}, "id"),
		readOnly:    true,
		handler:     getService,
	},
	{
		name:        "get_status",
		description: "读取探针看板全量状态（UP/DOWN/熔断/重试/PID 等）。",
		schema:      objSchema(map[string]any{}),
		readOnly:    true,
		handler:     getStatus,
	},
	{
		name: "add_service",
		description: "新增一个裸 exe 服务。exe 必须是面板所在 Windows 机器上的绝对路径。" +
			"enabled=true 会立即按依赖启动，enabled=false 只登记配置（不启动、不探测）；省略 ID 时按 name 自动生成。" +
			"探针默认 process，可选 http/tcp。",
		schema:   objSchema(serviceProps(), "name", "exe"),
		handler:  addService,
		readOnly: false,
	},
	{
		name: "update_service",
		description: "局部更新服务：先 GET 现有配置，合并你提供的字段后再 PUT，未提供的字段不会被清空。" +
			"运行中的启动配置变更会返回 pending_restart，需要 Restart 才生效。",
		schema:  objSchema(serviceProps(), "id"),
		handler: updateService,
	},
	{
		name:        "start_service",
		description: "手动启动服务，清除 manual_stop 与熔断状态。禁用的服务需先 update_service 启用。",
		schema:      objSchema(map[string]any{"id": idProp()}, "id"),
		handler:     startService,
	},
	{
		name:        "stop_service",
		description: "停止该服务自己的进程树（taskkill /PID <pid> /T），并抑制保活直到手动 Start。破坏性操作，需用户确认。",
		schema:      objSchema(map[string]any{"id": idProp()}, "id"),
		handler:     stopService,
		destructive: true,
	},
	{
		name:        "restart_service",
		description: "先停后起该服务，清除 manual_stop 与熔断。会终止并重新拉起进程，需用户确认。禁用的服务需先启用。",
		schema:      objSchema(map[string]any{"id": idProp()}, "id"),
		handler:     restartService,
		destructive: true,
	},
	{
		name: "delete_service",
		description: "删除服务配置。有服务依赖它、或它正在运行时默认失败；" +
			"stop=true 表示先停止面板托管的进程再删除。破坏性操作，需用户确认。",
		schema: objSchema(map[string]any{
			"id":   idProp(),
			"stop": boolProp("运行中是否先停止再删除，默认 false"),
		}, "id"),
		handler:     deleteService,
		destructive: true,
	},
	{
		name:        "get_logs",
		description: "读取服务日志尾部（logs/<id>.log）。",
		schema: objSchema(map[string]any{
			"id":    idProp(),
			"lines": intProp("读取行数，默认 100，范围 1..2000", 1, 2000),
		}, "id"),
		readOnly: true,
		handler:  getLogs,
	},
	{
		name:        "get_history",
		description: "读取服务的探针历史。",
		schema: objSchema(map[string]any{
			"id":    idProp(),
			"lines": intProp("读取条数，默认 100，范围 1..500", 1, 500),
		}, "id"),
		readOnly: true,
		handler:  getHistory,
	},
	{
		name:        "restart_all",
		description: "按依赖逆序停止、正序启动所有 enabled 服务。影响面大，需用户确认。",
		schema:      objSchema(map[string]any{}),
		handler:     restartAll,
		destructive: true,
	},
}

func toolList() []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":        t.name,
			"description": t.description,
			"inputSchema": t.schema,
			"annotations": map[string]any{
				"title":           t.name,
				"readOnlyHint":    t.readOnly,
				"destructiveHint": t.destructive,
				"openWorldHint":   false,
			},
		})
	}
	return out
}

func findTool(name string) (toolDef, bool) {
	for _, t := range tools {
		if t.name == name {
			return t, true
		}
	}
	return toolDef{}, false
}

// ---------------- JSON Schema ----------------

func objSchema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func idProp() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "服务 ID，例如 order-api；由字母/数字/下划线/短横线组成，最长 64",
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func intProp(desc string, min, max int) map[string]any {
	return map[string]any{
		"type": "integer", "description": desc,
		"minimum": min, "maximum": max,
	}
}

func strArrayProp(desc string) map[string]any {
	return map[string]any{
		"type": "array", "description": desc,
		"items": map[string]any{"type": "string"},
	}
}

func intArrayProp(desc string, min, max int) map[string]any {
	return map[string]any{
		"type": "array", "description": desc,
		"items": map[string]any{"type": "integer", "minimum": min, "maximum": max},
	}
}

func strMapProp(desc string) map[string]any {
	return map[string]any{
		"type": "object", "description": desc,
		"additionalProperties": map[string]any{"type": "string"},
	}
}

func enumProp(desc string, values ...string) map[string]any {
	return map[string]any{
		"type": "string", "description": desc, "enum": values,
	}
}

func serviceProps() map[string]any {
	return map[string]any{
		"id":                  idProp(),
		"name":                strProp("显示名称，必填"),
		"group":               strProp("分组名，可空"),
		"enabled":             boolProp("是否启用；true 会立即启动，false 只登记"),
		"exe":                 strProp("exe 绝对路径，必填；必须在面板所在机器上存在"),
		"args":                strArrayProp("启动参数数组，例如 [\"--port=9001\"]"),
		"cwd":                 strProp("工作目录；省略时默认 exe 所在目录"),
		"env":                 strMapProp("额外环境变量，例如 {\"ASPNETCORE_ENVIRONMENT\":\"Production\"}"),
		"depends_on":          strArrayProp("依赖的服务 ID 数组，必须已存在且不能成环"),
		"probe_type":          enumProp("探针类型，默认 process", "process", "http", "tcp"),
		"probe_url":           strProp("HTTP 探针地址，probe_type=http 时必填"),
		"probe_host":          strProp("TCP 探针主机，默认 127.0.0.1"),
		"probe_port":          intProp("TCP 探针端口，probe_type=tcp 时必填", 1, 65535),
		"probe_keyword":       strProp("HTTP 响应体必须包含的关键字，可空"),
		"probe_expect":        intProp("期望 HTTP 状态码，0 表示任意 2xx/3xx", 0, 599),
		"probe_timeout_sec":   intProp("探针超时秒数，默认 5", 1, 60),
		"restart_max_retries": intProp("最大连续重启次数，0 表示关闭自动保活", 0, 100),
		"restart_backoff_sec": intArrayProp("重启退避秒数，默认 [1,2,5,30]", 1, 3600),
	}
}

// ---------------- 工具实现 ----------------

func listServices(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	raw, err := api.do(ctx, http.MethodGet, "/api/services", nil, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Items []map[string]any `json:"items"`
	}
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析服务列表失败: %w", err)
	}
	out := make([]map[string]any, 0, len(resp.Items))
	for _, item := range resp.Items {
		out = append(out, summarizeService(item))
	}
	return fmt.Sprintf("共 %d 个服务\n%s", len(out), prettyJSON(out)), nil
}

func getService(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}
	raw, err := api.do(ctx, http.MethodGet, servicePath(id), nil, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Service map[string]any `json:"service"`
	}
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析服务失败: %w", err)
	}
	return prettyJSON(resp.Service), nil
}

func getStatus(ctx context.Context, api *apiClient, _ map[string]any) (string, error) {
	raw, err := api.do(ctx, http.MethodGet, "/api/status", nil, nil)
	if err != nil {
		return "", err
	}
	var resp map[string]any
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析状态失败: %w", err)
	}
	return prettyJSON(resp), nil
}

func addService(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	fields, err := buildServiceFields(args, true)
	if err != nil {
		return "", err
	}
	raw, err := api.do(ctx, http.MethodPost, "/api/services", nil, fields)
	if err != nil {
		return "", err
	}
	var resp map[string]any
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析新增结果失败: %w", err)
	}
	return "服务已创建" + serviceIDSuffix(resp) + "\n" + prettyJSON(resp), nil
}

func updateService(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}

	raw, err := api.do(ctx, http.MethodGet, servicePath(id), nil, nil)
	if err != nil {
		return "", err
	}
	var current struct {
		Service map[string]any `json:"service"`
	}
	if err := decodeRaw(raw, &current); err != nil {
		return "", fmt.Errorf("解析现有配置失败: %w", err)
	}
	if current.Service == nil {
		return "", fmt.Errorf("服务 %s 不存在", id)
	}

	fields, err := buildServiceFields(args, false)
	if err != nil {
		return "", err
	}
	delete(fields, "id")

	merged := make(map[string]any, len(current.Service)+len(fields))
	for k, v := range current.Service {
		if k == "runtime" {
			continue
		}
		merged[k] = v
	}
	for k, v := range fields {
		merged[k] = v
	}
	merged["id"] = id

	raw, err = api.do(ctx, http.MethodPut, servicePath(id), nil, merged)
	if err != nil {
		return "", err
	}
	var resp map[string]any
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析更新结果失败: %w", err)
	}
	return "配置已保存" + serviceIDSuffix(resp) + "\n" + prettyJSON(resp), nil
}

func startService(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}
	return postAction(ctx, api, "/api/start/"+url.PathEscape(id))
}

func stopService(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}
	return postAction(ctx, api, "/api/stop/"+url.PathEscape(id))
}

func restartService(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}
	return postAction(ctx, api, "/api/restart/"+url.PathEscape(id))
}

func deleteService(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}
	q := url.Values{}
	stop, ok, err := optionalBool(args, "stop")
	if err != nil {
		return "", err
	}
	if ok && stop {
		q.Set("stop", "true")
	}
	raw, err := api.do(ctx, http.MethodDelete, servicePath(id), q, nil)
	if err != nil {
		return "", err
	}
	var resp map[string]any
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析删除结果失败: %w", err)
	}
	return prettyJSON(resp), nil
}

func getLogs(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}
	lines := 100
	if n, ok, err := optionalInt(args, "lines"); err != nil {
		return "", err
	} else if ok {
		lines = n
	}
	q := url.Values{}
	q.Set("lines", strconv.Itoa(lines))

	raw, err := api.do(ctx, http.MethodGet, "/api/logs/"+url.PathEscape(id), q, nil)
	if err != nil {
		return "", err
	}
	var resp struct {
		Path  string   `json:"path"`
		Bytes int64    `json:"bytes"`
		Lines []string `json:"lines"`
	}
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析日志失败: %w", err)
	}
	text := strings.Join(resp.Lines, "\n")
	if strings.TrimSpace(text) == "" {
		text = "(日志为空)"
	}
	return fmt.Sprintf("日志 %s（%d bytes）\n%s", resp.Path, resp.Bytes, text), nil
}

func getHistory(ctx context.Context, api *apiClient, args map[string]any) (string, error) {
	id, err := requiredString(args, "id")
	if err != nil {
		return "", err
	}
	lines := 100
	if n, ok, err := optionalInt(args, "lines"); err != nil {
		return "", err
	} else if ok {
		lines = n
	}
	q := url.Values{}
	q.Set("lines", strconv.Itoa(lines))

	raw, err := api.do(ctx, http.MethodGet, "/api/history/"+url.PathEscape(id), q, nil)
	if err != nil {
		return "", err
	}
	var resp map[string]any
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析探测历史失败: %w", err)
	}
	return prettyJSON(resp), nil
}

func restartAll(ctx context.Context, api *apiClient, _ map[string]any) (string, error) {
	raw, err := api.do(ctx, http.MethodPost, "/api/restart-all", nil, nil)
	if err != nil {
		return "", err
	}
	var resp map[string]any
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析全部重启结果失败: %w", err)
	}
	return prettyJSON(resp), nil
}

func postAction(ctx context.Context, api *apiClient, path string) (string, error) {
	raw, err := api.do(ctx, http.MethodPost, path, nil, nil)
	if err != nil {
		return "", err
	}
	var resp map[string]any
	if err := decodeRaw(raw, &resp); err != nil {
		return "", fmt.Errorf("解析操作结果失败: %w", err)
	}
	return prettyJSON(resp), nil
}

// ---------------- 参数与输出辅助 ----------------

func buildServiceFields(args map[string]any, requireCore bool) (map[string]any, error) {
	out := map[string]any{}

	for _, key := range []string{"id", "name", "group", "exe", "cwd", "probe_type", "probe_url", "probe_host", "probe_keyword"} {
		v, ok, err := optionalString(args, key)
		if err != nil {
			return nil, err
		}
		if ok {
			out[key] = v
		}
	}

	if v, ok, err := optionalBool(args, "enabled"); err != nil {
		return nil, err
	} else if ok {
		out["enabled"] = v
	}

	for _, key := range []string{"probe_port", "probe_expect", "probe_timeout_sec", "restart_max_retries"} {
		v, ok, err := optionalInt(args, key)
		if err != nil {
			return nil, err
		}
		if ok {
			out[key] = v
		}
	}

	if v, ok, err := optionalStringSlice(args, "args"); err != nil {
		return nil, err
	} else if ok {
		out["args"] = v
	}
	if v, ok, err := optionalStringSlice(args, "depends_on"); err != nil {
		return nil, err
	} else if ok {
		out["depends_on"] = v
	}
	if v, ok, err := optionalIntSlice(args, "restart_backoff_sec"); err != nil {
		return nil, err
	} else if ok {
		out["restart_backoff_sec"] = v
	}
	if v, ok, err := optionalStringMap(args, "env"); err != nil {
		return nil, err
	} else if ok {
		out["env"] = v
	}

	if requireCore {
		if name, _ := out["name"].(string); strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("name 不能为空")
		}
		if exe, _ := out["exe"].(string); strings.TrimSpace(exe) == "" {
			return nil, fmt.Errorf("exe 不能为空")
		}
	}
	return out, nil
}

func requiredString(args map[string]any, key string) (string, error) {
	v, ok, err := optionalString(args, key)
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("%s 不能为空", key)
	}
	return v, nil
}

func optionalString(args map[string]any, key string) (string, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", false, fmt.Errorf("%s 必须是字符串", key)
	}
	return s, true, nil
}

func optionalBool(args map[string]any, key string) (bool, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return false, false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, false, fmt.Errorf("%s 必须是布尔值", key)
	}
	return b, true, nil
}

func optionalInt(args map[string]any, key string) (int, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0, false, fmt.Errorf("%s 必须是整数", key)
		}
		return int(i), true, nil
	case float64:
		if math.Trunc(n) != n {
			return 0, false, fmt.Errorf("%s 必须是整数", key)
		}
		return int(n), true, nil
	case int:
		return n, true, nil
	case int64:
		return int(n), true, nil
	default:
		return 0, false, fmt.Errorf("%s 必须是整数", key)
	}
}

func optionalStringSlice(args map[string]any, key string) ([]string, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false, fmt.Errorf("%s 必须是字符串数组", key)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s 的每个元素必须是字符串", key)
		}
		out = append(out, s)
	}
	return out, true, nil
}

func optionalIntSlice(args map[string]any, key string) ([]int, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false, fmt.Errorf("%s 必须是整数数组", key)
	}
	out := make([]int, 0, len(arr))
	for _, item := range arr {
		n, ok := item.(float64)
		if !ok || math.Trunc(n) != n {
			return nil, false, fmt.Errorf("%s 的每个元素必须是整数", key)
		}
		out = append(out, int(n))
	}
	return out, true, nil
}

func optionalStringMap(args map[string]any, key string) (map[string]string, bool, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, false, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("%s 必须是字符串对象", key)
	}
	out := make(map[string]string, len(m))
	for k, item := range m {
		s, ok := item.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s.%s 必须是字符串", key, k)
		}
		out[k] = s
	}
	return out, true, nil
}

func summarizeService(m map[string]any) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"id", "name", "group", "enabled", "exe", "args", "cwd", "probe_type", "probe_url", "probe_host", "probe_port"} {
		if v, ok := m[key]; ok && v != nil {
			out[key] = v
		}
	}
	if rt, ok := m["runtime"].(map[string]any); ok {
		r := map[string]any{}
		for _, key := range []string{"running", "pid", "state", "up", "rt_ms", "last_check", "last_error", "pending_restart", "probe_desc"} {
			if v, ok := rt[key]; ok {
				r[key] = v
			}
		}
		out["runtime"] = r
	}
	return out
}

func serviceIDSuffix(resp map[string]any) string {
	svc, ok := resp["service"].(map[string]any)
	if !ok {
		return ""
	}
	id, ok := svc["id"].(string)
	if !ok || id == "" {
		return ""
	}
	return "：" + id
}

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
