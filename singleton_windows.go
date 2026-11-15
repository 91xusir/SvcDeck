//go:build windows

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// acquireSingleInstance 用命名 Mutex 保证同一时间只有一个面板进程。
// Mutex 对象随进程退出自动释放（即使崩溃），不会留下需要手工清理的陈旧锁。
// 注意：命名空间用 Local\，同一用户会话内单例；跨会话由监听端口冲突兜底。
func acquireSingleInstance(name string) (func(), error) {
	namePtr, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateMutex(nil, false, namePtr)
	if err != nil {
		if h != 0 {
			windows.CloseHandle(h) // ERROR_ALREADY_EXISTS 时也会返回有效 handle，必须关闭
		}
		if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
			return nil, fmt.Errorf("另一个 SvcDeck 面板已在运行（命名 Mutex: %s）", name)
		}
		return nil, fmt.Errorf("创建单例 Mutex 失败: %w", err)
	}
	return func() { _ = windows.CloseHandle(h) }, nil
}
