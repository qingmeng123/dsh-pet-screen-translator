package capture

import (
	"context"
	"fmt"

	"dsh/local/screentranslator/internal/engine"
)

// Shooter 把 Win32 抓屏包装成 engine.Capturer。
type Shooter struct {
	// Class 窗口类名片段（优先）
	Class string
	// Title 窗口标题片段
	Title string
	// Quality JPEG 质量
	Quality int
	// AllowBackground 为 true 时，目标窗口不在前台也抓（默认拒绝，避免抓到遮挡内容）
	AllowBackground bool
	// IgnoreForegroundTitles 里的关键字出现在前台窗口标题里时，不视为「被遮挡」。
	// 用于忽略桌宠自己的透明浮窗：它弹气泡时会抢前台，但并不真正盖住游戏。
	IgnoreForegroundTitles []string
}

// Capture 实现 engine.Capturer。
func (s *Shooter) Capture(_ context.Context) (engine.Shot, error) {
	Init()

	var rect RECT
	source := "full"
	if s.Class != "" || s.Title != "" {
		hwnd, err := FindWindow(s.Class, s.Title)
		if err != nil {
			// 窗口找不到（游戏切场景/关闭中）：明确报跳过，别假装抓到了整屏
			return engine.Shot{Source: "window", Skipped: "window-not-found"}, nil
		}
		r, err := ClientRect(hwnd)
		if err != nil {
			return engine.Shot{}, fmt.Errorf("client rect: %w", err)
		}
		if !s.AllowBackground {
			if reason := s.blocked(hwnd, r); reason != "" {
				w := int(r.Right - r.Left)
				h := int(r.Bottom - r.Top)
				return engine.Shot{Source: "window", W: w, H: h, Skipped: reason}, nil
			}
		}
		rect = r
		source = "window"
	} else {
		rect = ScreenRect()
	}

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

// blocked 判断目标窗口是否被遮挡；返回空串表示可以抓，否则返回跳过原因。
//
// 判定顺序：
//   1. 目标就是前台 → 可以抓；
//   2. 前台是桌宠浮窗（标题含 IgnoreForegroundTitles）→ 可以抓（它只是小块透明浮层）；
//   3. 否则在客户区采 9 个点，看有多少点能命回目标窗口 —— 命中率 ≥ 30% 仍可抓。
//      GDI 抓的是屏幕像素，被完全遮住时抓到的就是别人，所以必须挡住就不抓。
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