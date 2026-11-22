package capture

import "testing"

// syscall.NewCallback 每注册一个就永久占用一个回调槽，Go 上限 2000 个。
// 早期版本每次 FindWindow 都 NewCallback，跑 ~2000 拍后进程会
// "fatal error: too many callback functions" 直接崩溃（2026-11-23 00:51 真踩过）。
// 这个用例反复调用，确保 callback 只注册一次。
func TestFindWindowDoesNotLeakCallbacks(t *testing.T) {
	Init()
	// 找一个必然存在的窗口：桌面壳
	found := 0
	for i := 0; i < 3000; i++ {
		if _, err := FindWindow("Progman", ""); err == nil {
			found++
		}
	}
	if found == 0 {
		t.Skip("当前环境没有 Progman 窗口，跳过（回调泄漏由 3000 次调用本身验证）")
	}
}
