//go:build windows

package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// 关键约束 1：DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP。
// 面板只是“拉起者”，子进程没有控制台、也不与面板的进程组/生命周期绑定：
// 面板正常退出、崩溃、被 taskkill（不带 /T）都不会影响业务进程。
const detachedCreationFlags = windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP

const stillActive = 259

type ProcInfo struct {
	PID       uint32
	Exe       string
	StartTime uint64 // FILETIME
	Alive     bool
}

// processInfo 打开进程并读取镜像路径 / 创建时间 / 存活状态。
// 只请求 PROCESS_QUERY_LIMITED_INFORMATION（普通用户即可查询自己的进程）。
func processInfo(pid uint32) (ProcInfo, error) {
	var info ProcInfo
	info.PID = pid
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return info, err
	}
	defer windows.CloseHandle(h)

	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return info, err
	}
	if code != stillActive {
		return info, nil // 句柄还能打开，但进程已经退出
	}

	buf := make([]uint16, 32768)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err == nil && size > 0 {
		info.Exe = windows.UTF16ToString(buf[:size])
	}

	var creation, exitTime, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exitTime, &kernel, &user); err == nil {
		info.StartTime = uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	}
	info.Alive = true
	return info, nil
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

func processAlive(pid uint32) bool {
	if pid == 0 || pid == 4 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

// verifyProcess 是 adopt 的核心校验，三重条件缺一不可：
//  1. pid 仍然存活；
//  2. 进程镜像路径与 DB 记录的 exe_path 一致（防 PID 被系统复用给别的程序）；
//  3. 进程创建时间与 DB 记录的 FILETIME 一致（PID 复用后创建时间必然不同）。
func verifyProcess(pid uint32, wantExe string, wantStart uint64) (bool, string) {
	if pid == 0 {
		return false, "pid 为空"
	}
	info, err := processInfo(pid)
	if err != nil {
		return false, fmt.Sprintf("无法打开 pid %d: %v", pid, err)
	}
	if !info.Alive {
		return false, fmt.Sprintf("pid %d 已退出", pid)
	}
	if wantExe != "" && !samePath(info.Exe, wantExe) {
		return false, fmt.Sprintf("pid %d 的镜像是 %q，与记录 %q 不符（疑似 PID 复用）", pid, info.Exe, wantExe)
	}
	if wantStart != 0 && info.StartTime != wantStart {
		return false, fmt.Sprintf("pid %d 创建时间 %d 与记录 %d 不符（疑似 PID 复用）", pid, info.StartTime, wantStart)
	}
	return true, ""
}

// findProcessesByExe 枚举系统进程，返回镜像路径匹配的进程（按创建时间倒序）。
// 用于“探针 up 但 DB pid 丢失”时按 exe 重新接管，以及清理端口残留。
// 调用方必须自行确认该 exe 在服务配置里是唯一的，否则可能误认/误杀同 exe 的其它服务实例。
func findProcessesByExe(exe string) []ProcInfo {
	if strings.TrimSpace(exe) == "" {
		return nil
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)

	self := uint32(os.Getpid())
	var out []ProcInfo
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return nil
	}
	for {
		pid := entry.ProcessID
		if pid != 0 && pid != 4 && pid != self {
			if info, err := processInfo(pid); err == nil && info.Alive && samePath(info.Exe, exe) {
				out = append(out, info)
			}
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	// 新的实例优先（通常就是当前应该接管的那一个）
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].StartTime > out[j-1].StartTime; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func buildChildEnv(overrides map[string]string) []string {
	if len(overrides) == 0 {
		return nil // 继承面板环境
	}
	base := os.Environ()
	keys := map[string]bool{}
	for k := range overrides {
		keys[strings.ToUpper(k)] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, kv := range base {
		i := strings.Index(kv, "=")
		if i <= 0 {
			out = append(out, kv)
			continue
		}
		if keys[strings.ToUpper(kv[:i])] {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func rotateLogIfNeeded(logPath string, maxMB int) {
	if maxMB <= 0 {
		return
	}
	st, err := os.Stat(logPath)
	if err != nil || st.Size() < int64(maxMB)*1024*1024 {
		return
	}
	old := logPath + ".1"
	_ = os.Remove(old)
	_ = os.Rename(logPath, old)
}

// spawnDetached 拉起子进程。注意：
//   - 不调用 cmd.Wait()（面板不是生命周期绑定者），Start 后立即 Release 句柄；
//   - stdout/stderr 追加到 logs/<id>.log，子进程自己持有文件句柄，面板关闭自身副本不影响它；
//   - 返回 pid 与创建时间，供 runtime_state 持久化，之后 adopt 时逐项比对。
func spawnDetached(svc Service, logPath string) (uint32, uint64, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, 0, fmt.Errorf("创建日志目录失败: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, 0, fmt.Errorf("打开日志文件失败: %w", err)
	}
	defer f.Close()

	fmt.Fprintf(f, "\n===== %s panel spawn: %s =====\n", time.Now().Format("2006-01-02 15:04:05"), svc.ID)

	cmd := exec.Command(svc.Exe, svc.Args...)
	cmd.Dir = svc.Cwd
	cmd.Env = buildChildEnv(svc.Env)
	cmd.Stdin = nil
	cmd.Stdout = f
	cmd.Stderr = f
	// 关键：DETACHED_PROCESS 让子进程没有控制台，面板退出/崩溃不会带走它。
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedCreationFlags}
	if err := cmd.Start(); err != nil {
		return 0, 0, fmt.Errorf("启动 %s 失败: %w", svc.Exe, err)
	}
	pid := uint32(cmd.Process.Pid)
	// Release 只关闭面板侧的进程句柄，不会结束进程；之后不再 Wait。
	_ = cmd.Process.Release()

	// 进程可能极短暂存活，尽最大努力拿创建时间；拿不到就存 0，adopt 时退化为只校验 exe 路径。
	var start uint64
	for i := 0; i < 20; i++ {
		if info, err := processInfo(pid); err == nil && info.Alive {
			start = info.StartTime
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	return pid, start, nil
}

func taskkillPath() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		p := filepath.Join(root, "System32", "taskkill.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "taskkill"
}

// taskkillServiceTree 只杀指定 pid 自己的进程树（/T），绝不影响其它服务和面板。
// force=false 时先发“优雅终止”信号；force=true 才 /F。
func taskkillServiceTree(pid uint32, force bool) error {
	if pid == 0 {
		return nil
	}
	args := []string{"/PID", strconv.Itoa(int(pid)), "/T"}
	if force {
		args = append(args, "/F")
	}
	cmd := exec.Command(taskkillPath(), args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.CombinedOutput()
	if err != nil {
		if !processAlive(pid) {
			return nil // 已经退出了，视为成功
		}
		forceArg := ""
		if force {
			forceArg = " /F"
		}
		msg := strings.TrimSpace(string(out))
		hint := ""
		lower := strings.ToLower(msg)
		if strings.Contains(msg, "拒绝访问") || strings.Contains(lower, "access is denied") ||
			strings.Contains(msg, "拒绝存取") || strings.Contains(msg, "拒绝") {
			hint = "（权限不足：面板拿不到终止该进程的权限。常见原因：面板以普通权限运行而业务进程以管理员运行、" +
				"面板本身运行在受限沙箱/容器中、业务进程属于其他用户或会话。请以相同或更高权限运行面板，" +
				"或先在任务管理器里手工结束该进程）"
		}
		return fmt.Errorf("taskkill /PID %d /T%s: %v: %s%s", pid, forceArg, err, msg, hint)
	}
	return nil
}

// terminateProcessDirect 只对单个 pid 调 TerminateProcess，不做 /T 树遍历。
// /T 会遍历整棵树，任何一个子进程拒绝访问都会让 taskkill 整条命令失败（exit 255），
// 但根进程往往仍然可杀，这时直接终止根进程是唯一还能救回来的退路。
func terminateProcessDirect(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	return windows.TerminateProcess(h, 1)
}

func waitProcessGone(pid uint32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !processAlive(pid)
}

// terminateGracefully 先 taskkill /PID /T（不带 /F），超过 grace 再 /F；
// 如果整树命令失败（典型：某个子进程拒绝访问）或根进程还活着，再直接对根进程补一刀。
func terminateGracefully(pid uint32, grace time.Duration) error {
	if !processAlive(pid) {
		return nil
	}
	_ = taskkillServiceTree(pid, false)
	if waitProcessGone(pid, grace) {
		return nil
	}
	treeErr := taskkillServiceTree(pid, true)
	if waitProcessGone(pid, 3*time.Second) {
		// /T 报错但根进程已经退出：按成功处理，残留子进程由调用方/日志感知。
		if treeErr != nil {
			log.Printf("[panel] taskkill 整树失败但根进程 %d 已退出: %v", pid, treeErr)
		}
		return nil
	}

	// 树杀之后进程还在：直接终止根进程，绕过 /T 的整树遍历。
	if derr := terminateProcessDirect(pid); derr == nil {
		if waitProcessGone(pid, 3*time.Second) {
			if treeErr != nil {
				log.Printf("[panel] taskkill 整树失败（%v），已改为直接终止根进程 %d；可能有子进程残留", treeErr, pid)
			}
			return nil
		}
	}
	if treeErr != nil {
		return treeErr // 已带“权限不足”提示
	}
	return fmt.Errorf("pid %d 在强制结束（taskkill /T /F + TerminateProcess）后仍未退出", pid)
}

// portInUse 只用于本机端口。先尝试连接 loopback（最可靠地发现“已有监听者”），
// 再尝试绑定 0.0.0.0（发现只绑定了网卡/IPv6 的情况）。
func portInUse(port int) bool {
	if port <= 0 {
		return false
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		c.Close()
		return true
	}
	addr6 := net.JoinHostPort("::1", strconv.Itoa(port))
	if c, err := net.DialTimeout("tcp", addr6, 300*time.Millisecond); err == nil {
		c.Close()
		return true
	}
	ln, err := net.Listen("tcp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}
