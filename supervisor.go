package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Supervisor 是“拉起者 + 探活者 + 按需重启者”，不持有任何 Job Object，
// 也不在退出路径上杀子进程。它只做三件事：
//  1. 启动前 adopt（pid + 创建时间 + exe 路径）或 spawn；
//  2. 周期探针（最终真相）；
//  3. 探针持续 down 时按服务粒度 taskkill + 退避重启，失败过多则熔断。
type Supervisor struct {
	store    *Store
	cfgStore *ConfigStore
	logDir   string

	ctx       context.Context
	cancel    context.CancelFunc
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	kick      chan struct{}

	client *http.Client

	mu    sync.RWMutex
	items map[string]*svcRT
	order []string

	metaMu    sync.Mutex
	exeCounts map[string]int
}

type svcRT struct {
	opMu sync.Mutex // 串行化 start/stop/restart/keepalive
	mu   sync.Mutex // 保护 cfg/st/运行时字段

	cfg Service
	st  RuntimeState

	rtMs       int64
	lastCheck  int64
	graceUntil time.Time

	probing         atomic.Bool
	removed         atomic.Bool
	circuitNotified bool
}

func NewSupervisor(store *Store, cfgStore *ConfigStore, logDir string) (*Supervisor, error) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Supervisor{
		store:     store,
		cfgStore:  cfgStore,
		logDir:    logDir,
		ctx:       ctx,
		cancel:    cancel,
		stopCh:    make(chan struct{}),
		kick:      make(chan struct{}, 1),
		items:     map[string]*svcRT{},
		exeCounts: map[string]int{},
		client: &http.Client{Transport: &http.Transport{
			Proxy:               nil, // 本地面板探针不走系统代理
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second,
			ForceAttemptHTTP2:   true,
		}},
	}
	services, err := store.ListServices()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := store.CleanupOrphans(); err != nil {
		log.Printf("[panel] 清理孤儿运行态失败: %v", err)
	}
	states, err := store.LoadAllRuntime()
	if err != nil {
		cancel()
		return nil, err
	}
	for _, svc := range services {
		rt := &svcRT{cfg: svc, st: states[svc.ID], lastCheck: states[svc.ID].LastCheck}
		rt.st.ServiceID = svc.ID
		s.items[svc.ID] = rt
	}
	s.setExeCounts(services)
	s.rebuildOrderLocked(services)
	log.Printf("[panel] 已加载 %d 个服务，其中 %d 个 enabled", len(services), len(s.enabledIDs()))
	return s, nil
}

func (s *Supervisor) logPath(id string) string {
	return filepath.Join(s.logDir, id+".log")
}

func (s *Supervisor) Start() {
	s.wg.Add(2)
	go s.probeLoop()
	go s.startupEnsure()
}

func (s *Supervisor) Close(ctx context.Context) {
	s.closeOnce.Do(func() {
		s.cancel()
		close(s.stopCh)
	})
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		log.Printf("[panel] 等待后台任务退出超时；子进程不会被终止")
	}
}

// ---------------- 拓扑序 ----------------

func topoOrder(services []Service, selected map[string]bool) ([]string, error) {
	byID := map[string]Service{}
	for _, svc := range services {
		byID[svc.ID] = svc
	}
	state := map[string]int{} // 0 未访问, 1 访问中, 2 完成
	order := make([]string, 0, len(selected))
	var visit func(id string, path []string) error
	visit = func(id string, path []string) error {
		switch state[id] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("依赖成环: %s", strings.Join(append(path, id), " -> "))
		}
		state[id] = 1
		svc, ok := byID[id]
		if ok {
			for _, dep := range svc.DependsOn {
				if !selected[dep] || dep == id {
					continue
				}
				if err := visit(dep, append(path, id)); err != nil {
					return err
				}
			}
		}
		state[id] = 2
		order = append(order, id)
		return nil
	}
	ids := make([]string, 0, len(selected))
	for _, svc := range services {
		if selected[svc.ID] {
			ids = append(ids, svc.ID)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

func (s *Supervisor) enabledIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

func (s *Supervisor) rebuildOrderLocked(services []Service) []string {
	selected := map[string]bool{}
	for _, svc := range services {
		if svc.Enabled {
			selected[svc.ID] = true
		}
	}
	order, err := topoOrder(services, selected)
	if err != nil {
		log.Printf("[panel] 计算拓扑序失败（已回退为配置顺序）: %v", err)
		order = nil
		for _, svc := range services {
			if svc.Enabled {
				order = append(order, svc.ID)
			}
		}
	}
	s.order = order
	return order
}

func (s *Supervisor) get(id string) *svcRT {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[id]
}

// setExeCounts 只用配置快照计算，不碰 rt.mu，避免与 adopt 路径形成锁序死锁。
func (s *Supervisor) setExeCounts(services []Service) {
	counts := map[string]int{}
	for _, svc := range services {
		key := strings.ToLower(filepath.Clean(svc.Exe))
		counts[key]++
	}
	s.metaMu.Lock()
	s.exeCounts = counts
	s.metaMu.Unlock()
}

// exeUnique 表示整个面板里只有这一个服务使用该 exe。
// 只有唯一时才允许“按 exe 找进程”并接管/清理；否则可能误杀同 exe 的另一个服务实例。
func (s *Supervisor) exeUnique(exe string) bool {
	s.metaMu.Lock()
	defer s.metaMu.Unlock()
	key := strings.ToLower(filepath.Clean(exe))
	return s.exeCounts[key] <= 1
}

func (s *Supervisor) Reload() error {
	services, err := s.store.ListServices()
	if err != nil {
		return err
	}
	states, err := s.store.LoadAllRuntime()
	if err != nil {
		return err
	}
	newItems := map[string]*svcRT{}
	s.mu.RLock()
	// 复制 map 头，避免后续与 Remove 的 delete 并发读写同一个 map。
	old := make(map[string]*svcRT, len(s.items))
	for id, rt := range s.items {
		old[id] = rt
	}
	s.mu.RUnlock()
	for _, svc := range services {
		if rt, ok := old[svc.ID]; ok {
			rt.mu.Lock()
			rt.cfg = svc
			rt.mu.Unlock()
			newItems[svc.ID] = rt
			continue
		}
		rt := &svcRT{cfg: svc, st: states[svc.ID], lastCheck: states[svc.ID].LastCheck}
		rt.st.ServiceID = svc.ID
		newItems[svc.ID] = rt
	}
	order := func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.items = newItems
		return s.rebuildOrderLocked(services)
	}()
	s.setExeCounts(services)
	log.Printf("[panel] 热加载完成：%d 个服务，拓扑序 %v", len(services), order)
	return nil
}

func (s *Supervisor) Remove(id string) {
	s.mu.Lock()
	rt := s.items[id]
	delete(s.items, id)
	s.mu.Unlock()
	if rt != nil {
		rt.removed.Store(true)
		rt.mu.Lock()
		rt.st.PID = 0
		rt.st.ExePath = ""
		rt.st.ProcessStartTime = 0
		rt.mu.Unlock()
	}
	go func() {
		services, err := s.store.ListServices()
		if err != nil {
			log.Printf("[panel] 删除服务后刷新拓扑失败: %v", err)
			return
		}
		s.mu.Lock()
		s.rebuildOrderLocked(services)
		s.mu.Unlock()
		s.setExeCounts(services)
	}()
}

// AutoStart 用于“用户明确启用”服务后异步拉起（不阻塞 HTTP 响应）。
// 启用动作视为用户意图，会清除 manual_stop / 熔断标记；
// 面板启动路径不会调用它，因此重启面板不会破坏上次的手动停止。
func (s *Supervisor) AutoStart(id string) {
	rt := s.get(id)
	if rt == nil {
		return
	}
	go func() {
		rt.mu.Lock()
		if !rt.cfg.Enabled {
			rt.mu.Unlock()
			return
		}
		rt.st.ManualStop = false
		rt.st.CircuitOpen = false
		rt.st.Retries = 0
		rt.st.ConsecutiveFails = 0
		rt.st.NextRetryAt = 0
		rt.st.LastError = ""
		rt.circuitNotified = false
		s.persistLocked(rt)
		rt.mu.Unlock()
		if err := s.ensureRunning(rt, "config"); err != nil {
			log.Printf("[svc:%s] 启用后自动拉起失败: %v", id, err)
		}
	}()
}

// ---------------- 探针循环 ----------------

func (s *Supervisor) probeLoop() {
	defer s.wg.Done()
	interval := s.interval()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-s.kick:
			// 配置变更后按新间隔重新计时
			interval = s.interval()
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(interval)
		case <-timer.C:
			s.tick()
			interval = s.interval()
			timer.Reset(interval)
		}
	}
}

func (s *Supervisor) interval() time.Duration {
	sec := s.cfgStore.Get().ProbeIntervalSec
	if sec < 1 {
		sec = 5
	}
	return time.Duration(sec) * time.Second
}

func (s *Supervisor) tick() {
	s.mu.RLock()
	list := make([]*svcRT, 0, len(s.items))
	for _, rt := range s.items {
		list = append(list, rt)
	}
	s.mu.RUnlock()

	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, rt := range list {
		if !rt.probing.CompareAndSwap(false, true) {
			continue
		}
		cfg, st, _, _, _ := s.snapshot(rt)
		uniqueExe := s.exeUnique(cfg.Exe)
		wg.Add(1)
		sem <- struct{}{}
		go func(rt *svcRT, cfg Service, st RuntimeState, uniqueExe bool) {
			defer wg.Done()
			defer func() { <-sem }()
			defer rt.probing.Store(false)
			res := probeService(s.ctx, s.client, cfg, st, uniqueExe)
			s.applyProbe(rt, res)
		}(rt, cfg, st, uniqueExe)
	}
	wg.Wait()
}

const spawnGrace = 15 * time.Second

func (s *Supervisor) applyProbe(rt *svcRT, res ProbeResult) {
	now := time.Now()
	settings := s.cfgStore.Get()
	// 先拿配置快照再算 exe 唯一性（exeUnique 用独立的 metaMu，不会与 rt.mu 形成锁环）。
	cfg, _, _, _, _ := s.snapshot(rt)
	uniqueExe := s.exeUnique(cfg.Exe)

	var needRestart, circuitNow bool
	var circuitReason string

	rt.mu.Lock()
	if rt.removed.Load() {
		rt.mu.Unlock()
		return
	}
	rt.lastCheck = now.Unix()
	rt.rtMs = res.RTMs
	rt.st.LastCheck = now.Unix()
	rt.st.ServiceID = rt.cfg.ID
	rt.st.UpdatedAt = nowStamp()

	if res.Up {
		rt.st.LastUp = true
		rt.st.ConsecutiveFails = 0
		rt.st.Retries = 0
		rt.st.CircuitOpen = false
		rt.st.NextRetryAt = 0
		rt.st.LastError = ""
		rt.graceUntil = time.Time{}
		rt.circuitNotified = false
		s.adoptFromProbeLocked(rt, res, uniqueExe)
	} else {
		rt.st.LastUp = false
		rt.st.LastError = res.Err

		// 探针 down 时顺手清理已被系统复用的 pid，避免 Stop 杀错人。
		if rt.st.PID != 0 {
			if ok, why := verifyProcess(rt.st.PID, rt.st.ExePath, rt.st.ProcessStartTime); !ok {
				log.Printf("[svc:%s] 清理失效 pid: %s", rt.cfg.ID, why)
				rt.st.PID = 0
				rt.st.ExePath = ""
				rt.st.ProcessStartTime = 0
			}
		}

		if now.Before(rt.graceUntil) {
			// 刚 spawn，给业务服务留出启动时间；不累计失败、不触发保活。
		} else {
			rt.st.ConsecutiveFails++
			if rt.st.ConsecutiveFails >= settings.FailThreshold {
				switch {
				case rt.cfg.RestartMaxRetries <= 0:
					// 0 = 关闭自动保活，只展示 DOWN
				case rt.st.Retries >= rt.cfg.RestartMaxRetries:
					if !rt.st.CircuitOpen {
						rt.st.CircuitOpen = true
						circuitNow = true
						circuitReason = fmt.Sprintf("连续 %d 次探测失败，且保活重启已达上限 %d 次",
							rt.st.ConsecutiveFails, rt.cfg.RestartMaxRetries)
					}
				case now.Unix() >= rt.st.NextRetryAt:
					needRestart = true
				}
			}
		}
	}
	s.persistLocked(rt)
	rt.mu.Unlock()

	if settings.ProbeHistoryLimit > 0 {
		if err := s.store.AppendProbeHistory(cfg.ID, res.Up, res.RTMs, res.Err, settings.ProbeHistoryLimit); err != nil {
			log.Printf("[svc:%s] 写探测历史失败: %v", cfg.ID, err)
		}
	}

	if circuitNow {
		log.Printf("[svc:%s] 熔断: %s", cfg.ID, circuitReason)
		s.sendWebhook("circuit_open", cfg, circuitReason)
	}
	if needRestart {
		s.keepaliveRestart(rt)
	}
}

// adoptFromProbeLocked 只在 rt.mu 持有期间调用。
// 探针 up 但 DB 里没有可用 pid（例如 DB 被清过、或服务是手工启动的）时，
// 按 exe 找回进程并接管。exe 被多个服务共享时无法安全判断归属，因此只标 UP 不绑定 pid。
func (s *Supervisor) adoptFromProbeLocked(rt *svcRT, res ProbeResult, uniqueExe bool) {
	if rt.st.PID != 0 {
		return
	}
	if !uniqueExe {
		return
	}
	pid := res.FoundPID
	start := res.FoundStart
	exe := res.FoundExe
	if pid == 0 {
		pids := findProcessesByExe(rt.cfg.Exe)
		if len(pids) == 0 {
			return
		}
		pid, start, exe = pids[0].PID, pids[0].StartTime, pids[0].Exe
	}
	rt.st.PID = pid
	rt.st.ProcessStartTime = start
	rt.st.ExePath = exe
	if rt.st.StartSignature == "" {
		rt.st.StartSignature = launchSignature(rt.cfg)
	}
	log.Printf("[svc:%s] 探针 UP，按 exe 接管 pid %d", rt.cfg.ID, pid)
}

func (s *Supervisor) snapshot(rt *svcRT) (Service, RuntimeState, int64, int64, time.Time) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.cfg, rt.st, rt.rtMs, rt.lastCheck, rt.graceUntil
}

func (s *Supervisor) persistLocked(rt *svcRT) {
	if rt.removed.Load() {
		return
	}
	rt.st.UpdatedAt = nowStamp()
	if err := s.store.SaveRuntime(rt.st); err != nil {
		log.Printf("[svc:%s] 保存运行态失败: %v", rt.cfg.ID, err)
	}
}

func (s *Supervisor) runtimeView(cfg Service, st RuntimeState, rtMs, lastCheck int64, grace time.Time, now time.Time) RuntimeView {
	graceActive := !grace.IsZero() && now.Before(grace)
	state := "down"
	switch {
	case st.LastUp:
		state = "up"
	case !cfg.Enabled:
		state = "disabled"
	case st.CircuitOpen:
		state = "circuit"
	case st.ManualStop:
		state = "stopped"
	case st.PID != 0 && graceActive:
		state = "starting"
	case lastCheck == 0:
		state = "unknown"
	}
	pending := false
	if st.PID != 0 && st.StartSignature != "" {
		pending = launchSignature(cfg) != st.StartSignature
	}
	return RuntimeView{
		Running:          st.PID != 0,
		PID:              st.PID,
		State:            state,
		Up:               st.LastUp,
		RTMs:             rtMs,
		LastCheck:        lastCheck,
		ConsecutiveFails: st.ConsecutiveFails,
		Retries:          st.Retries,
		CircuitOpen:      st.CircuitOpen,
		ManualStop:       st.ManualStop,
		LastError:        st.LastError,
		PendingRestart:   pending,
		ProbeDesc:        probeDescription(cfg),
	}
}

func (s *Supervisor) viewFor(rt *svcRT) ServiceView {
	cfg, st, rtMs, lastCheck, grace := s.snapshot(rt)
	return ServiceView{
		Service: cfg,
		Runtime: s.runtimeView(cfg, st, rtMs, lastCheck, grace, time.Now()),
	}
}

func (s *Supervisor) Services() []ServiceView {
	s.mu.RLock()
	list := make([]*svcRT, 0, len(s.items))
	for _, rt := range s.items {
		list = append(list, rt)
	}
	s.mu.RUnlock()
	out := make([]ServiceView, 0, len(list))
	for _, rt := range list {
		out = append(out, s.viewFor(rt))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Supervisor) Status() []StatusItem {
	views := s.Services()
	out := make([]StatusItem, 0, len(views))
	for _, v := range views {
		out = append(out, StatusItem{
			ID:          v.ID,
			Name:        v.Name,
			Group:       v.Group,
			Enabled:     v.Enabled,
			ProbeType:   v.ProbeType,
			RuntimeView: v.Runtime,
		})
	}
	return out
}

func (s *Supervisor) ServiceView(id string) (ServiceView, bool) {
	rt := s.get(id)
	if rt == nil {
		return ServiceView{}, false
	}
	return s.viewFor(rt), true
}

func (s *Supervisor) LogPath(id string) string { return s.logPath(id) }

// ---------------- 启动 / 接管 / 拉起 ----------------

func (s *Supervisor) startupEnsure() {
	defer s.wg.Done()
	order := s.enabledIDs()
	log.Printf("[panel] 启动接管开始，拓扑序 %v", order)
	for _, id := range order {
		select {
		case <-s.stopCh:
			return
		default:
		}
		rt := s.get(id)
		if rt == nil {
			continue
		}
		cfg, st, _, _, _ := s.snapshot(rt)
		if !cfg.Enabled {
			continue
		}
		if st.ManualStop {
			log.Printf("[svc:%s] 上次为手动停止，跳过自动拉起", id)
			continue
		}
		if st.CircuitOpen {
			log.Printf("[svc:%s] 处于熔断状态，跳过自动拉起（请手动 Start 或 Restart 复位）", id)
			continue
		}
		if s.cfgStore.Get().WaitDeps {
			s.waitForDeps(rt, cfg)
		}
		if err := s.ensureRunning(rt, "startup"); err != nil {
			log.Printf("[svc:%s] 启动失败: %v", id, err)
		}
	}
	log.Printf("[panel] 启动接管流程结束")
}

func (s *Supervisor) waitForDeps(rt *svcRT, cfg Service) {
	if len(cfg.DependsOn) == 0 {
		return
	}
	deadline := time.Now().Add(20 * time.Second)
	log.Printf("[svc:%s] 等待依赖服务探针 UP: %v", cfg.ID, cfg.DependsOn)
	for time.Now().Before(deadline) {
		select {
		case <-s.stopCh:
			return
		default:
		}
		allUp := true
		for _, dep := range cfg.DependsOn {
			drt := s.get(dep)
			if drt == nil {
				continue
			}
			dcfg, dst, _, _, _ := s.snapshot(drt)
			if !dcfg.Enabled {
				continue // 禁用/不存在的依赖不阻塞启动
			}
			if !dst.LastUp {
				allUp = false
				break
			}
		}
		if allUp {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	log.Printf("[svc:%s] 等待依赖超时，继续启动", cfg.ID)
}

func (s *Supervisor) ensureRunning(rt *svcRT, reason string) error {
	rt.opMu.Lock()
	defer rt.opMu.Unlock()
	return s.ensureRunningLocked(rt, reason)
}

// ensureRunningLocked 必须持有 rt.opMu。
// 顺序严格为：adopt（pid 校验）→ 探针判定 → spawn。探针 up 时绝不 spawn，避免重复拉起。
func (s *Supervisor) ensureRunningLocked(rt *svcRT, reason string) error {
	if rt.removed.Load() {
		return fmt.Errorf("服务已删除")
	}
	if err := s.ctx.Err(); err != nil {
		return fmt.Errorf("面板正在退出，取消启动")
	}
	cfg, st, _, _, _ := s.snapshot(rt)
	uniqueExe := s.exeUnique(cfg.Exe)

	if st.PID != 0 {
		if ok, why := verifyProcess(st.PID, st.ExePath, st.ProcessStartTime); ok {
			log.Printf("[svc:%s] adopt 成功：pid %d 仍是 %s（%s）", cfg.ID, st.PID, st.ExePath, reason)
			rt.mu.Lock()
			rt.st.LastError = ""
			rt.st.UpdatedAt = nowStamp()
			s.persistLocked(rt)
			rt.mu.Unlock()
			return nil
		} else {
			log.Printf("[svc:%s] adopt 失败（%s）：%s；将清理记录后再判断", cfg.ID, reason, why)
			rt.mu.Lock()
			rt.st.PID = 0
			rt.st.ExePath = ""
			rt.st.ProcessStartTime = 0
			s.persistLocked(rt)
			rt.mu.Unlock()
			st.PID = 0
			st.ExePath = ""
			st.ProcessStartTime = 0
		}
	}

	// 探针是最终真相：若进程已经在跑（例如面板重启前 DB 写入丢失），直接接管。
	res := probeService(s.ctx, s.client, cfg, st, uniqueExe)
	if res.Up {
		rt.mu.Lock()
		rt.st.LastUp = true
		rt.st.LastCheck = time.Now().Unix()
		rt.lastCheck = rt.st.LastCheck
		rt.rtMs = res.RTMs
		rt.st.LastError = ""
		rt.st.ConsecutiveFails = 0
		rt.st.Retries = 0
		rt.st.CircuitOpen = false
		rt.graceUntil = time.Time{}
		s.adoptFromProbeLocked(rt, res, uniqueExe)
		s.persistLocked(rt)
		rt.mu.Unlock()
		log.Printf("[svc:%s] 探针 UP，无需启动（%s）", cfg.ID, reason)
		return nil
	}

	return s.spawnLocked(rt, reason)
}

func (s *Supervisor) graceDuration(cfg Service) time.Duration {
	g := spawnGrace
	if d := probeTimeout(cfg) + 5*time.Second; d > g {
		g = d
	}
	return g
}

func (s *Supervisor) spawnLocked(rt *svcRT, reason string) error {
	cfg, st, _, _, _ := s.snapshot(rt)
	if rt.removed.Load() {
		return fmt.Errorf("服务已删除")
	}

	// 端口占用保护：多半是上次残留。只按 DB 记录/唯一 exe 清理，绝不杀未知进程。
	if port := spawnPortGuardPort(cfg); port > 0 && portInUse(port) {
		if err := s.clearPortResidual(rt, cfg, st, port); err != nil {
			rt.mu.Lock()
			rt.st.LastError = err.Error()
			s.persistLocked(rt)
			rt.mu.Unlock()
			return err
		}
	}

	rotateLogIfNeeded(s.logPath(cfg.ID), s.cfgStore.Get().LogMaxMB)
	pid, startTime, err := spawnDetached(cfg, s.logPath(cfg.ID))
	if err != nil {
		rt.mu.Lock()
		rt.st.LastError = err.Error()
		rt.st.LastUp = false
		s.persistLocked(rt)
		rt.mu.Unlock()
		return err
	}

	rt.mu.Lock()
	rt.st.PID = pid
	rt.st.ProcessStartTime = startTime
	rt.st.ExePath = cfg.Exe
	rt.st.LastUp = false
	rt.st.LastError = ""
	rt.st.StartSignature = launchSignature(cfg)
	rt.graceUntil = time.Now().Add(s.graceDuration(cfg))
	rt.rtMs = 0
	s.persistLocked(rt)
	rt.mu.Unlock()
	log.Printf("[svc:%s] 已拉起 pid %d（%s，%s）", cfg.ID, pid, cfg.Exe, reason)
	return nil
}

func (s *Supervisor) clearPortResidual(rt *svcRT, cfg Service, st RuntimeState, port int) error {
	grace := time.Duration(s.cfgStore.Get().StopGraceSec) * time.Second
	if grace <= 0 {
		grace = 5 * time.Second
	}
	log.Printf("[svc:%s] 端口 %d 被占用，尝试清理残留进程", cfg.ID, port)

	// 1) DB 记录的 pid，必须通过 adopt 三重校验才允许杀
	if st.PID != 0 {
		if ok, why := verifyProcess(st.PID, st.ExePath, st.ProcessStartTime); ok {
			if err := terminateGracefully(st.PID, grace); err != nil {
				return fmt.Errorf("清理残留 pid %d 失败: %w", st.PID, err)
			}
		} else {
			log.Printf("[svc:%s] 记录的 pid 已失效（%s），不杀", cfg.ID, why)
		}
		rt.mu.Lock()
		rt.st.PID = 0
		rt.st.ExePath = ""
		rt.st.ProcessStartTime = 0
		rt.mu.Unlock()
	}

	// 2) 若 exe 在整个面板里唯一，可按 exe 找回残留实例（手工启动的同一服务）
	if portInUse(port) && s.exeUnique(cfg.Exe) {
		for _, p := range findProcessesByExe(cfg.Exe) {
			log.Printf("[svc:%s] 清理按 exe 找到的残留 pid %d", cfg.ID, p.PID)
			if err := terminateGracefully(p.PID, grace); err != nil {
				return fmt.Errorf("清理残留 pid %d 失败: %w", p.PID, err)
			}
		}
	}

	deadline := time.Now().Add(5 * time.Second)
	for portInUse(port) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if portInUse(port) {
		return fmt.Errorf("端口 %d 仍被占用，占用者不是本服务可确认的进程；为避免误杀已取消启动", port)
	}
	return nil
}

func (s *Supervisor) stopLocked(rt *svcRT, markManual bool) error {
	cfg, st, _, _, _ := s.snapshot(rt)
	grace := time.Duration(s.cfgStore.Get().StopGraceSec) * time.Second
	if grace <= 0 {
		grace = 5 * time.Second
	}

	pid := uint32(0)
	if st.PID != 0 {
		if ok, why := verifyProcess(st.PID, st.ExePath, st.ProcessStartTime); ok {
			pid = st.PID
		} else {
			log.Printf("[svc:%s] Stop 时发现 pid 失效（%s），忽略", cfg.ID, why)
		}
	}
	if pid == 0 && s.exeUnique(cfg.Exe) {
		if pids := findProcessesByExe(cfg.Exe); len(pids) > 0 {
			pid = pids[0].PID
			log.Printf("[svc:%s] Stop 按 exe 找回 pid %d", cfg.ID, pid)
		}
	}

	if pid == 0 && st.LastUp {
		return fmt.Errorf("服务探针为 UP，但无法定位其进程（exe 可能被多个服务共享）；请在目标机上确认后再操作")
	}

	var stopErr error
	if pid != 0 {
		// 只杀这个服务自己的树：taskkill /PID <pid> /T；先不带 /F，超时再 /F。
		stopErr = terminateGracefully(pid, grace)
		if stopErr != nil {
			rt.mu.Lock()
			rt.st.LastError = stopErr.Error()
			s.persistLocked(rt)
			rt.mu.Unlock()
			return stopErr
		}
	}

	rt.mu.Lock()
	rt.st.PID = 0
	rt.st.ProcessStartTime = 0
	rt.st.ExePath = ""
	rt.st.LastUp = false
	rt.st.LastError = ""
	rt.st.ConsecutiveFails = 0
	rt.st.Retries = 0
	rt.st.NextRetryAt = 0
	rt.st.CircuitOpen = false
	rt.st.StartSignature = ""
	if markManual {
		rt.st.ManualStop = true
	}
	rt.graceUntil = time.Time{}
	rt.circuitNotified = false
	s.persistLocked(rt)
	rt.mu.Unlock()
	log.Printf("[svc:%s] 已停止（pid %d）", cfg.ID, pid)
	return nil
}

func (s *Supervisor) stopForRestartLocked(rt *svcRT) error {
	return s.stopLocked(rt, false)
}

// keepaliveRestart 是唯一会自动 taskkill 的“保活”路径。它先停止旧进程树，再 spawn。
func (s *Supervisor) keepaliveRestart(rt *svcRT) {
	rt.opMu.Lock()
	defer rt.opMu.Unlock()

	cfg, st, _, _, _ := s.snapshot(rt)
	if rt.removed.Load() || !cfg.Enabled || st.ManualStop || st.CircuitOpen {
		return
	}

	log.Printf("[svc:%s] 保活重启（retries=%d）", cfg.ID, st.Retries)
	if err := s.stopForRestartLocked(rt); err != nil {
		s.recordAttemptFailure(rt, fmt.Errorf("保活重启前停止旧进程失败: %w", err))
		return
	}
	if err := s.spawnLocked(rt, "keepalive"); err != nil {
		s.recordAttemptFailure(rt, fmt.Errorf("保活启动失败: %w", err))
		return
	}
	s.recordAttemptAfterSpawn(rt)
}

func (s *Supervisor) recordAttemptAfterSpawn(rt *svcRT) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.st.Retries++
	backoff := rt.cfg.RestartBackoffSec
	if len(backoff) == 0 {
		backoff = []int{1, 2, 5, 30}
	}
	idx := rt.st.Retries - 1
	if idx >= len(backoff) {
		idx = len(backoff) - 1
	}
	if idx >= 0 {
		rt.st.NextRetryAt = time.Now().Add(time.Duration(backoff[idx]) * time.Second).Unix()
	}
	s.persistLocked(rt)
}

func (s *Supervisor) recordAttemptFailure(rt *svcRT, cause error) {
	var notify bool
	var reason string
	rt.mu.Lock()
	cfg := rt.cfg
	rt.st.LastError = cause.Error()
	rt.st.LastUp = false
	rt.st.Retries++
	backoff := rt.cfg.RestartBackoffSec
	if len(backoff) == 0 {
		backoff = []int{1, 2, 5, 30}
	}
	idx := rt.st.Retries - 1
	if idx >= len(backoff) {
		idx = len(backoff) - 1
	}
	if idx >= 0 {
		rt.st.NextRetryAt = time.Now().Add(time.Duration(backoff[idx]) * time.Second).Unix()
	}
	if rt.cfg.RestartMaxRetries > 0 && rt.st.Retries >= rt.cfg.RestartMaxRetries && !rt.st.CircuitOpen {
		rt.st.CircuitOpen = true
		notify = !rt.circuitNotified
		rt.circuitNotified = true
		reason = "保活重启失败次数达到上限"
	}
	s.persistLocked(rt)
	rt.mu.Unlock()
	if notify {
		log.Printf("[svc:%s] 熔断: %s", cfg.ID, reason)
		s.sendWebhook("circuit_open", cfg, reason)
	}
}

// ---------------- 对外动作 ----------------

func (s *Supervisor) StartService(id string) error {
	rt := s.get(id)
	if rt == nil {
		return fmt.Errorf("服务不存在: %s", id)
	}
	rt.opMu.Lock()
	defer rt.opMu.Unlock()
	rt.mu.Lock()
	rt.st.ManualStop = false
	rt.st.CircuitOpen = false
	rt.st.Retries = 0
	rt.st.ConsecutiveFails = 0
	rt.st.NextRetryAt = 0
	rt.st.LastError = ""
	rt.circuitNotified = false
	s.persistLocked(rt)
	rt.mu.Unlock()
	return s.ensureRunningLocked(rt, "manual-start")
}

func (s *Supervisor) StopService(id string) error {
	rt := s.get(id)
	if rt == nil {
		return fmt.Errorf("服务不存在: %s", id)
	}
	rt.opMu.Lock()
	defer rt.opMu.Unlock()
	return s.stopLocked(rt, true)
}

func (s *Supervisor) RestartService(id string) error {
	rt := s.get(id)
	if rt == nil {
		return fmt.Errorf("服务不存在: %s", id)
	}
	rt.opMu.Lock()
	defer rt.opMu.Unlock()
	rt.mu.Lock()
	rt.st.ManualStop = false
	rt.st.CircuitOpen = false
	rt.st.Retries = 0
	rt.st.ConsecutiveFails = 0
	rt.st.NextRetryAt = 0
	rt.st.LastError = ""
	rt.circuitNotified = false
	s.persistLocked(rt)
	rt.mu.Unlock()

	if err := s.stopForRestartLocked(rt); err != nil {
		return err
	}
	if err := s.spawnLocked(rt, "manual-restart"); err != nil {
		return err
	}
	rt.mu.Lock()
	rt.st.Retries = 0
	rt.st.NextRetryAt = 0
	s.persistLocked(rt)
	rt.mu.Unlock()
	return nil
}

// RestartAll 先按依赖逆序停止，再按依赖正序拉起；只处理 enabled 服务。
func (s *Supervisor) RestartAll() ([]string, []string) {
	order := s.enabledIDs()
	var errs []string
	for i := len(order) - 1; i >= 0; i-- {
		rt := s.get(order[i])
		if rt == nil {
			continue
		}
		cfg, _, _, _, _ := s.snapshot(rt)
		if !cfg.Enabled {
			continue
		}
		rt.opMu.Lock()
		rt.mu.Lock()
		rt.st.ManualStop = false
		rt.st.CircuitOpen = false
		rt.st.Retries = 0
		rt.st.ConsecutiveFails = 0
		rt.st.NextRetryAt = 0
		rt.st.LastError = ""
		rt.circuitNotified = false
		s.persistLocked(rt)
		rt.mu.Unlock()
		err := s.stopForRestartLocked(rt)
		rt.opMu.Unlock()
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", order[i], err))
		}
	}
	for _, id := range order {
		rt := s.get(id)
		if rt == nil {
			continue
		}
		cfg, _, _, _, _ := s.snapshot(rt)
		if !cfg.Enabled {
			continue
		}
		if err := s.ensureRunning(rt, "restart-all"); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", id, err))
		}
	}
	return order, errs
}

func (s *Supervisor) NotifyConfigChanged() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// ---------------- Webhook ----------------

type webhookPayload struct {
	Event       string `json:"event"`
	ServiceID   string `json:"service_id"`
	ServiceName string `json:"service_name"`
	Message     string `json:"message"`
	Time        string `json:"time"`
}

func (s *Supervisor) sendWebhook(event string, cfg Service, message string) {
	u := strings.TrimSpace(s.cfgStore.Get().WebhookURL)
	if u == "" {
		return
	}
	payload := webhookPayload{
		Event: event, ServiceID: cfg.ID, ServiceName: cfg.Name,
		Message: message, Time: time.Now().Format(time.RFC3339),
	}
	body := jsonString(payload)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(body))
		if err != nil {
			log.Printf("[webhook] 构造请求失败: %v", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			log.Printf("[webhook] 发送失败: %v", err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("[webhook] 返回 %d", resp.StatusCode)
		}
	}()
}
