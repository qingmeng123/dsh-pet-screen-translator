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

// 自动模式的目标筛选：DSH、桌宠、桌面类永不作为目标；普通游戏窗口通过。
func TestAutoTargetIgnoreRules(t *testing.T) {
	s := &Shooter{
		Auto:          true,
		IgnoreTargets: []string{"DeepSeek Harness", "dsh-pet"},
	}
	cases := []struct {
		title, class string
		ignored      bool
	}{
		{"翻译需求 — DeepSeek Harness", "Chrome_WidgetWin_1", true},
		{"dsh-pet 桌宠（桌面模式）", "Chrome_WidgetWin_1", true},
		{"", "Progman", true},
		{"", "Shell_TrayWnd", true},
		{"Watch_Dogs", "Watch_Dogs", false},
		{"DELTARUNE", "YYGameMakerYY", false},
	}
	for _, c := range cases {
		if got := s.isIgnoredTarget(c.title, c.class); got != c.ignored {
			t.Errorf("isIgnoredTarget(%q,%q)=%v，期望 %v", c.title, c.class, got, c.ignored)
		}
	}
}
