//go:build !windows

package main

// attachParentConsole 在非 Windows 平台不需要任何处理：
// 进程天然依附于启动它的终端，stdin/stdout/stderr 已经可用。
// 返回 true 表示“有可用的终端输出”，调用方据此决定日志写终端还是写文件。
func attachParentConsole() bool {
	return true
}
