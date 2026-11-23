package capture

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"dsh/local/screentranslator/internal/engine"
)

// Shooter 把 Win32 抓屏包装成 engine.Capturer。
type Shooter struct {
	// Class 窗口类名片段（固定模式用，优先）
	Class string
	// Title 窗口标题片段（固定模式用）
	Title string
	// Auto 为 true 时跟随前台窗口（自动模式）：切到哪个游戏就翻哪个，不用改配置。
	Auto bool
	// Quality JPEG 质量
	Quality int
	// AllowBackground 为 true 时，目标窗口不在前台也抓（默认拒绝，避免抓到遮挡内容）
	AllowBackground bool
	// IgnoreForegroundTitles 里的关键字出现在前台窗口标题里时，不视为「被遮挡」。
	// 用于忽略桌宠自己的透明浮窗：它弹气泡时会抢前台，但并不真正盖住游戏。
	IgnoreForegroundTitles []string
	// IgnoreTargets 是自动模式下永不当作目标的窗口（标题或类名包含即可）：DSH、桌宠、桌面。
	IgnoreTargets []string
	// MinScreenPercent 是自动模式的最低占屏比例（0.25 = 25%），太小的窗口不当游戏
	MinScreenPercent float64
}

// Capture 实现 engine.Capturer。
func (s *Shooter) Capture(_ context.Context) (engine.Shot, error) {
	Init()

	var (
		hwnd uintptr
		rect RECT
	)
	source := "window"

	switch {
	case s.Auto:
		h, skip := s.autoTarget()
		if skip != "" {
			return engine.Shot{Source: "window", Skipped: skip}, nil
		}
		r, err := ClientRect(h)
		if err != nil {
			if errors.Is(err, ErrClientTooSmall) {
				return engine.Shot{Source: "window", Skipped: "window-minimized"}, nil
			}
			return engine.Shot{}, fmt.Errorf("client rect: %w", err)
		}
		// 自动模式抓的就是前台窗口，不需要再做遮挡判定
		hwnd, rect = h, r

	case s.Class != "" || s.Title != "":
		h, err := FindWindow(s.Class, s.Title)
		if err != nil {
			// 窗口找不到（游戏切场景/关闭中）：明确报跳过，别假装抓到了整屏
			return engine.Shot{Source: "window", Skipped: "window-not-found"}, nil
		}
		r, err := ClientRect(h)
		if err != nil {
			// 游戏被最小化（客户区 0x0）不是错误，跳过这一拍即可
			if errors.Is(err, ErrClientTooSmall) {
				return engine.Shot{Source: "window", Skipped: "window-minimized"}, nil
			}
			return engine.Shot{}, fmt.Errorf("client rect: %w", err)
		}
		if !s.AllowBackground {
			if reason := s.blocked(h, r); reason != "" {
				w := int(r.Right - r.Left)
				hh := int(r.Bottom - r.Top)
				return engine.Shot{Source: "window", W: w, H: hh, Skipped: reason}, nil
			}
		}
		hwnd, rect = h, r

	default:
		rect = ScreenRect()
		source = "full"
	}
	_ = hwnd

	img, err := Grab(rect)
	if err != nil {
		return engine.Shot{}, err
	}
	quality := s.Quality
	if quality <= 0 {
		quality = 85
	}
	jpegBytes, err := EncodeJPEG(img, quality)
	if err != nil {
		return engine.Shot{}, err
	}
	return engine.Shot{
		Image:  img,
		JPEG:   jpegBytes,
		W:      img.Bounds().Dx(),
		H:      img.Bounds().Dy(),
		Source: source,
	}, nil
}

// autoTarget 挑出"当前该翻译的窗口"：前台窗口，且不是 DSH/桌宠/桌面，且占屏够大。
// 返回 skip 非空表示这一拍不翻。
func (s *Shooter) autoTarget() (uintptr, string) {
	hwnd := Foreground()
	if hwnd == 0 {
		return 0, "no-target"
	}
	// 桌宠自己的浮窗常常抢前台，真正在下面玩游戏的窗口要往前找
	if s.isIgnoredTarget(windowText(procText, hwnd), windowText(procCls, hwnd)) {
		if root := RootWindow(hwnd); root != 0 && root != hwnd {
			hwnd = root
		}
	}
	title := windowText(procText, hwnd)
	class := windowText(procCls, hwnd)
	if s.isIgnoredTarget(title, class) {
		return 0, "no-target"
	}

	r, err := ClientRect(hwnd)
	if err != nil {
		if errors.Is(err, ErrClientTooSmall) {
			return 0, "window-minimized"
		}
		return 0, "no-target"
	}
	w := r.Right - r.Left
	h := r.Bottom - r.Top
	screen := ScreenRect()
	sw := screen.Right - screen.Left
	sh := screen.Bottom - screen.Top
	if sw <= 0 || sh <= 0 {
		return hwnd, ""
	}
	minPct := s.MinScreenPercent
	if minPct <= 0 {
		minPct = 0.25
	}
	if float64(w*h)/float64(sw*sh) < minPct {
		return 0, "target-too-small"
	}
	return hwnd, ""
}

// isIgnoredTarget 判断某个窗口是否永远不该被当作翻译目标。
func (s *Shooter) isIgnoredTarget(title, class string) bool {
	if strings.TrimSpace(title) == "" && strings.TrimSpace(class) == "" {
		return true
	}
	// 桌面 / 任务栏 / 输入法
	for _, cls := range []string{"Progman", "WorkerW", "Shell_TrayWnd", "Windows.UI.Core.CoreWindow"} {
		if strings.EqualFold(class, cls) {
			return true
		}
	}
	for _, sub := range s.IgnoreTargets {
		if sub == "" {
			continue
		}
		if containsFold(title, sub) || containsFold(class, sub) {
			return true
		}
	}
	return false
}

// blocked 判断目标窗口是否被遮挡；返回空串表示可以抓，否则返回跳过原因。
//
// 判定顺序：
//  1. 目标就是前台 → 可以抓；
//  2. 前台是桌宠浮窗（标题含 IgnoreForegroundTitles）→ 可以抓（它只是小块透明浮层）；
//  3. 否则在客户区采 9 个点，看有多少点能命回目标窗口 —— 命中率 ≥ 88% 仍可抓。
//     GDI 抓的是屏幕像素，被完全遮住时抓到的就是别人，所以必须挡住就不抓。
func (s *Shooter) blocked(hwnd uintptr, r RECT) string {
	// 只有"目标自己就是前台"才能直接放行。
	// 不能因为"前台是桌宠浮窗"就放行：桌宠可能在最上层，而它下面还压着别的窗口，
	// 这种情况下抓到的像素属于被压住的窗口（实测翻出过 DSH 聊天内容）。
	if Foreground() == hwnd {
		return ""
	}

	w := r.Right - r.Left
	h := r.Bottom - r.Top
	if w <= 0 || h <= 0 {
		return "bad-rect"
	}
	visible, total := 0, 0
	for _, fy := range []int32{25, 50, 75} {
		for _, fx := range []int32{25, 50, 75} {
			total++
			x := r.Left + w*fx/100
			y := r.Top + h*fy/100
			hit := WindowFromPoint(x, y)
			if hit == 0 || hit == hwnd || RootWindow(hit) == hwnd {
				visible++
				continue
			}
			if s.ignored(windowText(procText, RootWindow(hit))) {
				visible++
			}
		}
	}
	// 阈值从 30% 提到 88%（9 点里至少 8 点属于游戏）：
	// 30% 时即使 70% 画面被别的窗口盖住也会照抓，抓到的就是混合内容，
	// 实测翻出过 DSH 聊天窗里的文字。
	if visible*100/total >= 88 {
		return ""
	}
	return "occluded"
}

// ignored 判断窗口标题是否属于"不算遮挡"的浮层。
func (s *Shooter) ignored(title string) bool {
	for _, sub := range s.IgnoreForegroundTitles {
		if sub != "" && containsFold(title, sub) {
			return true
		}
	}
	return false
}
