//go:build windows

package main

// 面板用 go build -ldflags="-H windowsgui" 构建：进程自己不带控制台。
//
//   - 从 cmd / PowerShell 启动：父进程有控制台，AttachConsole 接上去，
//     再把 os.Stdin/Stdout/Stderr 重新绑到 CONIN$/CONOUT$，日志才看得见；
//   - 双击 / explorer 启动：父进程没有控制台，AttachConsole 失败，保持静默；
//   - console 子系统构建（go run / 去掉了 -H windowsgui）：本来就带着控制台；
//   - 标准输出被重定向（SvcDeck.exe > out.log）：尊重重定向，不去抢控制台。
//
// 两个必须注意的点：
//  1. GUI 子系统启动时进程的标准句柄是无效的，AttachConsole 之后要重新打开设备，
//     不能继续用 Go 初始化时抓到的旧句柄；
//  2. log 包在 init 时就把当时的 os.Stderr 指针存进了默认 Logger，
//     所以重定向后必须再调用 log.SetOutput(...)，否则日志仍然写向失效的旧句柄。

import (
	"os"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procAttachConsole  = kernel32.NewProc("AttachConsole")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

const (
	attachParentProcess             = ^uintptr(0) // (DWORD)-1，ATTACH_PARENT_PROCESS
	enableVirtualTerminalProcessing = 0x0004      // ENABLE_VIRTUAL_TERMINAL_PROCESSING
)

// attachParentConsole 尝试连接父进程控制台。
// 返回 true 表示标准流可用（终端启动），false 表示没有终端（双击启动，GUI 静默）。
func attachParentConsole() bool {
	// 1) 标准输出已经被父进程重定向到文件/管道（> out.log、构建脚本采集输出）：
	//    这时 os.Stdout/os.Stderr 本来就是有效的，不需要也不应该再去抢控制台。
	if h, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE); err == nil {
		switch t, _ := syscall.GetFileType(h); t {
		case syscall.FILE_TYPE_PIPE, syscall.FILE_TYPE_DISK:
			return true
		}
	}

	// 2) 接父进程控制台。
	if r, _, err := procAttachConsole.Call(attachParentProcess); r == 0 {
		// ERROR_ACCESS_DENIED 表示本进程已经带着控制台（console 子系统构建），
		// 原有标准流仍然可用，直接按“有终端”处理。
		if errno, ok := err.(syscall.Errno); ok && errno == syscall.ERROR_ACCESS_DENIED {
			return true
		}
		// 其它错误（典型是 ERROR_INVALID_HANDLE）说明父进程没有控制台：双击/explorer 启动。
		return false
	}

	// 3) 连上了父控制台，重建标准流：CONOUT$ / CONIN$ 是当前控制台的设备名，
	//    打开后得到的句柄既能 Read/Write，也能用于 SetConsoleMode。
	if f, err := os.OpenFile("CONOUT$", os.O_RDWR, 0); err == nil {
		os.Stdout, os.Stderr = f, f
		enableVT(f.Fd()) // 立刻开启 VT 序列，\x1b[ 颜色码才生效（Win10 1511+）
	}
	if f, err := os.OpenFile("CONIN$", os.O_RDWR, 0); err == nil {
		// 接上 stdin 后，控制台里的 Ctrl+C 会以 CTRL_C_EVENT 送到本进程，走优雅退出。
		os.Stdin = f
	}
	return true
}

// enableVT 对指定的控制台句柄开启 ENABLE_VIRTUAL_TERMINAL_PROCESSING。
// 句柄无效、被重定向到管道/文件、或系统太老时静默跳过，不影响功能。
func enableVT(handle uintptr) {
	if handle == 0 || handle == ^uintptr(0) {
		return
	}
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(handle, uintptr(unsafe.Pointer(&mode))); r == 0 {
		return
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return
	}
	_, _, _ = procSetConsoleMode.Call(handle, uintptr(mode|enableVirtualTerminalProcessing))
}
