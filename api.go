package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

type API struct {
	store   *Store
	sup     *Supervisor
	cfg     *ConfigStore
	started time.Time
}

func NewAPI(store *Store, sup *Supervisor, cfg *ConfigStore) *API {
	return &API{store: store, sup: sup, cfg: cfg, started: time.Now()}
}

// Handler 返回 /api 子树的 Handler（路径模式以 /api/... 开头）。
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /api/services", a.listServices)
	mux.HandleFunc("POST /api/services", a.createService)
	mux.HandleFunc("GET /api/services/{id}", a.getService)
	mux.HandleFunc("PUT /api/services/{id}", a.updateService)
	mux.HandleFunc("DELETE /api/services/{id}", a.deleteService)
	mux.HandleFunc("GET /api/logs/{id}", a.logs)
	mux.HandleFunc("GET /api/history/{id}", a.history)
	mux.HandleFunc("GET /api/config", a.getConfig)
	mux.HandleFunc("PUT /api/config", a.putConfig)
	mux.HandleFunc("POST /api/start/{id}", a.startService)
	mux.HandleFunc("POST /api/stop/{id}", a.stopService)
	mux.HandleFunc("POST /api/restart/{id}", a.restartService)
	mux.HandleFunc("POST /api/restart-all", a.restartAll)
	return a.recoverMW(a.authMW(mux))
}

func (a *API) fail(w http.ResponseWriter, status int, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	writeJSON(w, status, map[string]any{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[api] panic: %v (%s %s)", rec, r.Method, r.URL.Path)
				a.fail(w, http.StatusInternalServerError, "内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *API) authMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := a.cfg.Get().Token
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		given := strings.TrimSpace(r.Header.Get("X-Panel-Token"))
		if given == "" {
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				given = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
			}
		}
		if subtle.ConstantTimeCompare([]byte(given), []byte(token)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="svcdeck"`)
			a.fail(w, http.StatusUnauthorized, "访问令牌无效，请在页面中输入 Token")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func decodeBody(r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 2<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("请求体 JSON 无效: %w", err)
	}
	return nil
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"name":       "SvcDeck",
		"version":    "0.1.0",
		"pid":        os.Getpid(),
		"uptime_sec": int64(time.Since(a.started).Seconds()),
	})
}

func (a *API) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items":              a.sup.Status(),
		"server_time":        time.Now().Unix(),
		"probe_interval_sec": a.cfg.Get().ProbeIntervalSec,
	})
}

func (a *API) listServices(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": a.sup.Services()})
}

func (a *API) getService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	view, ok := a.sup.ServiceView(id)
	if !ok {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": view})
}

func (a *API) createService(w http.ResponseWriter, r *http.Request) {
	var in Service
	if err := decodeBody(r, &in); err != nil {
		a.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	svc, err := a.normalizeService(in, true)
	if err != nil {
		a.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	svc.CreatedAt = nowStamp()
	svc.UpdatedAt = svc.CreatedAt
	if err := a.store.InsertService(svc); err != nil {
		a.fail(w, http.StatusConflict, "写入服务失败（ID 可能已存在）: %v", err)
		return
	}
	if err := a.sup.Reload(); err != nil {
		log.Printf("[api] 新增服务后热加载失败: %v", err)
	}
	if svc.Enabled {
		a.sup.AutoStart(svc.ID)
	}
	view, _ := a.sup.ServiceView(svc.ID)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "service": view, "message": "服务已创建"})
}

func (a *API) updateService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, exists, err := a.store.GetService(id)
	if err != nil {
		a.fail(w, http.StatusInternalServerError, "读取服务失败: %v", err)
		return
	}
	if !exists {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	var in Service
	if err := decodeBody(r, &in); err != nil {
		a.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	in.ID = id // ID 不允许通过 PUT 改变
	svc, err := a.normalizeService(in, false)
	if err != nil {
		a.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	svc.CreatedAt = old.CreatedAt
	svc.UpdatedAt = nowStamp()
	if err := a.store.UpdateService(svc); err != nil {
		a.fail(w, http.StatusInternalServerError, "保存服务失败: %v", err)
		return
	}
	if err := a.sup.Reload(); err != nil {
		log.Printf("[api] 编辑服务后热加载失败: %v", err)
	}
	if !old.Enabled && svc.Enabled {
		a.sup.AutoStart(svc.ID)
	}
	launchChanged := launchSignature(old) != launchSignature(svc)
	view, _ := a.sup.ServiceView(svc.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"service":         view,
		"pending_restart": view.Runtime.Running && launchChanged,
		"message":         "配置已保存并热加载",
	})
}

func (a *API) deleteService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, exists, err := a.store.GetService(id)
	if err != nil {
		a.fail(w, http.StatusInternalServerError, "读取服务失败: %v", err)
		return
	}
	if !exists {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	all, err := a.store.ListServices()
	if err != nil {
		a.fail(w, http.StatusInternalServerError, "读取服务列表失败: %v", err)
		return
	}
	for _, other := range all {
		for _, dep := range other.DependsOn {
			if dep == id {
				a.fail(w, http.StatusConflict, "服务 %s 依赖 %s，请先移除依赖关系", other.ID, id)
				return
			}
		}
	}
	stop := false
	if raw := r.URL.Query().Get("stop"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			a.fail(w, http.StatusBadRequest, "stop 参数必须是 true/false")
			return
		}
		stop = v
	}
	view, _ := a.sup.ServiceView(id)
	// “在跑”以面板托管的 pid 为准。探针 UP 但 pid 为空（例如探针指向外部 HTTP 端点）时，
	// 面板没有可杀的进程，删除只移除配置，不应被 Stop 流程挡住。
	running := view.Runtime.Running
	allowStop := stop || a.cfg.Get().AllowDeleteRunning
	if running && !allowStop {
		a.fail(w, http.StatusConflict, "服务正在运行。请先 Stop，或使用 DELETE ?stop=true，或在全局设置中允许删除运行中的服务")
		return
	}
	if running && allowStop {
		if err := a.sup.StopService(id); err != nil {
			a.fail(w, http.StatusConflict, "删除前停止服务失败: %v", err)
			return
		}
	}
	if err := a.store.DeleteService(id); err != nil {
		a.fail(w, http.StatusInternalServerError, "删除服务失败: %v", err)
		return
	}
	a.sup.Remove(id)
	log.Printf("[api] 已删除服务 %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "服务已删除"})
}

func (a *API) startService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.sup.ServiceView(id); !ok {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	if err := a.sup.StartService(id); err != nil {
		a.fail(w, http.StatusInternalServerError, "启动失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "启动指令已执行"})
}

func (a *API) stopService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.sup.ServiceView(id); !ok {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	if err := a.sup.StopService(id); err != nil {
		a.fail(w, http.StatusConflict, "停止失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已停止（该服务在手动 Start 前不会被保活拉起）"})
}

func (a *API) restartService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.sup.ServiceView(id); !ok {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	if err := a.sup.RestartService(id); err != nil {
		a.fail(w, http.StatusInternalServerError, "重启失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已重启"})
}

func (a *API) restartAll(w http.ResponseWriter, _ *http.Request) {
	order, errs := a.sup.RestartAll()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     len(errs) == 0,
		"order":  order,
		"errors": errs,
		"message": func() string {
			if len(errs) == 0 {
				return "全部重启完成"
			}
			return "部分服务重启失败"
		}(),
	})
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.sup.ServiceView(id); !ok {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	lines := 100
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			a.fail(w, http.StatusBadRequest, "lines 必须是整数")
			return
		}
		lines = n
	}
	if lines < 1 {
		lines = 1
	}
	if lines > 2000 {
		lines = 2000
	}
	items, size, err := readTailLines(a.sup.LogPath(id), lines)
	if err != nil {
		a.fail(w, http.StatusInternalServerError, "读取日志失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "lines": items, "bytes": size, "path": filepath.ToSlash(a.sup.LogPath(id)),
	})
}

func readTailLines(path string, maxLines int) ([]string, int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, 0, nil
		}
		return nil, 0, err
	}
	if st.IsDir() {
		return nil, 0, fmt.Errorf("%s 是目录", path)
	}
	const window = 256 * 1024
	size := st.Size()
	start := int64(0)
	if size > window {
		start = size - window
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	buf := make([]byte, size-start)
	if len(buf) > 0 {
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return nil, 0, err
		}
	}
	text := strings.ReplaceAll(string(buf), "\r\n", "\n")
	all := strings.Split(text, "\n")
	if start > 0 && len(all) > 0 {
		all = all[1:] // 丢弃窗口截断处的半行
	}
	if len(all) > 0 && all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	if len(all) > maxLines {
		all = all[len(all)-maxLines:]
	}
	if all == nil {
		all = []string{}
	}
	return all, size, nil
}

func (a *API) history(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := a.sup.ServiceView(id); !ok {
		a.fail(w, http.StatusNotFound, "服务不存在: %s", id)
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("lines"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			a.fail(w, http.StatusBadRequest, "lines 必须在 1..500 之间")
			return
		}
		limit = n
	}
	items, err := a.store.ProbeHistory(id, limit)
	if err != nil {
		a.fail(w, http.StatusInternalServerError, "读取探测历史失败: %v", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (a *API) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"config": a.cfg.Get()})
}

func (a *API) putConfig(w http.ResponseWriter, r *http.Request) {
	old := a.cfg.Get()
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		a.fail(w, http.StatusBadRequest, "读取请求体失败: %v", err)
		return
	}
	next, err := settingsFromJSON(body, old)
	if err != nil {
		a.fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	if err := a.store.SaveSettings(configToRows(next)); err != nil {
		a.fail(w, http.StatusInternalServerError, "保存设置失败: %v", err)
		return
	}
	a.cfg.Set(next)
	a.sup.NotifyConfigChanged()
	restartRequired := []string{}
	if next.ListenAddr != old.ListenAddr {
		restartRequired = append(restartRequired, "listen_addr")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"config":           next,
		"restart_required": restartRequired,
		"message":          "设置已保存（监听地址变更需重启面板进程）",
	})
}

// ---------------- 输入归一化与校验 ----------------

func slugID(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return slug
}

func normalizeArgs(in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	total := 0
	for _, a := range in {
		if len(a) > 8192 {
			return nil, fmt.Errorf("单个参数过长（最多 8192 字符）")
		}
		total += len(a) + 1
		out = append(out, a)
	}
	if total > 30000 {
		return nil, fmt.Errorf("参数总长度过长（最多 30000 字符）")
	}
	return out, nil
}

func normalizeEnv(in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" {
			return nil, fmt.Errorf("环境变量名不能为空")
		}
		if strings.ContainsAny(key, "=\x00") {
			return nil, fmt.Errorf("环境变量名 %q 非法", key)
		}
		if len(key) > 256 || len(v) > 8192 {
			return nil, fmt.Errorf("环境变量 %s 过长", key)
		}
		out[key] = v
	}
	return out, nil
}

func (a *API) normalizeService(in Service, creating bool) (Service, error) {
	all, err := a.store.ListServices()
	if err != nil {
		return Service{}, fmt.Errorf("读取现有服务失败: %w", err)
	}
	in.ID = strings.TrimSpace(in.ID)
	in.Name = strings.TrimSpace(in.Name)
	in.Group = strings.TrimSpace(in.Group)
	if in.Name == "" {
		return Service{}, fmt.Errorf("名称不能为空")
	}
	if len(in.Name) > 120 {
		return Service{}, fmt.Errorf("名称最多 120 字符")
	}
	if len(in.Group) > 60 {
		return Service{}, fmt.Errorf("分组最多 60 字符")
	}
	if creating && in.ID == "" {
		base := slugID(in.Name)
		if base == "" {
			base = "svc"
		}
		ids := map[string]bool{}
		for _, s := range all {
			ids[s.ID] = true
		}
		candidate := base
		for i := 2; ids[candidate]; i++ {
			candidate = fmt.Sprintf("%s-%d", base, i)
		}
		in.ID = candidate
	}
	if !idPattern.MatchString(in.ID) {
		return Service{}, fmt.Errorf("ID 只能由字母/数字/下划线/短横线组成，最长 64，且不能以符号开头")
	}
	if creating {
		if _, exists, err := a.store.GetService(in.ID); err != nil {
			return Service{}, err
		} else if exists {
			return Service{}, fmt.Errorf("ID %q 已存在", in.ID)
		}
	}

	exe := strings.TrimSpace(in.Exe)
	if exe == "" {
		return Service{}, fmt.Errorf("启动 exe 路径不能为空")
	}
	absExe, err := filepath.Abs(exe)
	if err != nil {
		return Service{}, fmt.Errorf("exe 路径无效: %w", err)
	}
	stat, err := os.Stat(absExe)
	if err != nil {
		return Service{}, fmt.Errorf("exe 不存在或不可访问: %s", absExe)
	}
	if stat.IsDir() {
		return Service{}, fmt.Errorf("exe 路径是目录: %s", absExe)
	}
	in.Exe = filepath.Clean(absExe)

	args, err := normalizeArgs(in.Args)
	if err != nil {
		return Service{}, err
	}
	in.Args = args

	env, err := normalizeEnv(in.Env)
	if err != nil {
		return Service{}, err
	}
	in.Env = env

	cwd := strings.TrimSpace(in.Cwd)
	if cwd == "" {
		cwd = filepath.Dir(in.Exe)
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return Service{}, fmt.Errorf("cwd 无效: %w", err)
	}
	cst, err := os.Stat(absCwd)
	if err != nil || !cst.IsDir() {
		return Service{}, fmt.Errorf("cwd 不存在或不是目录: %s", absCwd)
	}
	in.Cwd = filepath.Clean(absCwd)

	// 依赖：去重、存在性、自依赖、成环
	seen := map[string]bool{}
	deps := make([]string, 0, len(in.DependsOn))
	for _, d := range in.DependsOn {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		if d == in.ID {
			return Service{}, fmt.Errorf("服务不能依赖自身")
		}
		seen[d] = true
		deps = append(deps, d)
	}
	in.DependsOn = deps
	existing := map[string]Service{}
	for _, s := range all {
		existing[s.ID] = s
	}
	existing[in.ID] = in // 覆盖/新增
	for _, d := range deps {
		if _, ok := existing[d]; !ok {
			return Service{}, fmt.Errorf("依赖的服务不存在: %s", d)
		}
	}
	selected := map[string]bool{}
	svcList := make([]Service, 0, len(existing))
	for id, s := range existing {
		selected[id] = true
		svcList = append(svcList, s)
	}
	if _, err := topoOrder(svcList, selected); err != nil {
		return Service{}, fmt.Errorf("依赖关系非法: %w", err)
	}

	in.ProbeType = strings.ToLower(strings.TrimSpace(in.ProbeType))
	switch in.ProbeType {
	case "":
		in.ProbeType = "process"
	case "process":
	case "http":
		u, err := url.Parse(strings.TrimSpace(in.ProbeURL))
		if err != nil {
			return Service{}, fmt.Errorf("probe_url 无效: %v", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return Service{}, fmt.Errorf("probe_url 必须是 http/https 地址")
		}
		if u.Host == "" {
			return Service{}, fmt.Errorf("probe_url 缺少主机名")
		}
		in.ProbeURL = strings.TrimSpace(in.ProbeURL)
		if in.ProbeExpect < 0 || in.ProbeExpect > 599 {
			return Service{}, fmt.Errorf("probe_expect 必须在 0..599 之间（0 表示 2xx/3xx）")
		}
		if len(in.ProbeKeyword) > 256 {
			return Service{}, fmt.Errorf("probe_keyword 最多 256 字符")
		}
		in.ProbeKeyword = strings.TrimSpace(in.ProbeKeyword)
	case "tcp":
		if strings.TrimSpace(in.ProbeHost) == "" {
			in.ProbeHost = "127.0.0.1"
		}
		in.ProbeHost = strings.TrimSpace(in.ProbeHost)
		if in.ProbePort < 1 || in.ProbePort > 65535 {
			return Service{}, fmt.Errorf("probe_port 必须在 1..65535 之间")
		}
		// 探测目标端口与 host 参与端口占用保护；非法 host 由探针报错
	default:
		return Service{}, fmt.Errorf("probe_type 必须是 http / tcp / process")
	}

	if in.ProbeTimeoutSec <= 0 {
		in.ProbeTimeoutSec = 5
	}
	if in.ProbeTimeoutSec > 60 {
		return Service{}, fmt.Errorf("probe_timeout_sec 必须在 1..60 之间")
	}
	if in.RestartMaxRetries < 0 || in.RestartMaxRetries > 100 {
		return Service{}, fmt.Errorf("restart_max_retries 必须在 0..100 之间（0 表示关闭自动保活）")
	}
	if len(in.RestartBackoffSec) == 0 {
		in.RestartBackoffSec = []int{1, 2, 5, 30}
	}
	if len(in.RestartBackoffSec) > 10 {
		return Service{}, fmt.Errorf("restart_backoff_sec 最多 10 个元素")
	}
	for _, v := range in.RestartBackoffSec {
		if v < 1 || v > 3600 {
			return Service{}, fmt.Errorf("restart_backoff_sec 每个值必须在 1..3600 秒之间")
		}
	}
	return in, nil
}
