// Package capture 用 Win32 API 抓屏：定位窗口、取客户区、BitBlt 取像素、编码 JPEG。
//
// 不依赖 cgo，全部走 syscall + LazyDLL。抓的是"屏幕上那块矩形"（GDI 语义），
// 因此目标窗口被遮挡时抓到的是上层内容 —— 调用方需要自己判断前台状态。
package capture

import (
	"errors"
	"fmt"
	"image"
	"syscall"
	"unsafe"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	procDPI  = user32.NewProc("SetProcessDPIAware")
	procEnum = user32.NewProc("EnumWindows")
	procText = user32.NewProc("GetWindowTextW")
	procCls  = user32.NewProc("GetClassNameW")
	procVis  = user32.NewProc("IsWindowVisible")
	procCR   = user32.NewProc("GetClientRect")
	procC2S  = user32.NewProc("ClientToScreen")
	procFG   = user32.NewProc("GetForegroundWindow")
	procDC   = user32.NewProc("GetDC")
	procRDC  = user32.NewProc("ReleaseDC")
	procSM   = user32.NewProc("GetSystemMetrics")
	procWFP  = user32.NewProc("WindowFromPoint")
	procAnc  = user32.NewProc("GetAncestor")

	procCreateDC  = gdi32.NewProc("CreateCompatibleDC")
	procCreateBmp = gdi32.NewProc("CreateCompatibleBitmap")
	procDIBSection = gdi32.NewProc("CreateDIBSection")
	procSelect    = gdi32.NewProc("SelectObject")
	procBitBlt    = gdi32.NewProc("BitBlt")
	procDeleteObj = gdi32.NewProc("DeleteObject")
	procDeleteDC  = gdi32.NewProc("DeleteDC")
	procDIBits    = gdi32.NewProc("GetDIBits")
)

// RECT 对应 Win32 的矩形（逻辑像素）。
type RECT struct {
	Left, Top, Right, Bottom int32
}

// POINT 对应 Win32 的点。
type POINT struct {
	X, Y int32
}

// BITMAPINFOHEADER 是 GetDIBits 需要的头。
type BITMAPINFOHEADER struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

// BITMAPINFO 只用到 1 个颜色表项（32bpp 不需要调色板）。
type BITMAPINFO struct {
	Header BITMAPINFOHEADER
	Colors [1]uint32
}

const (
	srcCopy      = 0x00CC0020
	dibRGBColors = 0
	smCXScreen   = 0
	smCYScreen   = 1
)

// Init 声明进程 DPI 感知。不声明时拿到的是缩放后的逻辑像素，截图会糊、OCR 会掉分辨率。
func Init() {
	_, _, _ = procDPI.Call()
}

// ScreenRect 返回主屏（物理像素）矩形。
func ScreenRect() RECT {
	w, _, _ := procSM.Call(smCXScreen)
	h, _, _ := procSM.Call(smCYScreen)
	return RECT{Left: 0, Top: 0, Right: int32(w), Bottom: int32(h)}
}

// Foreground 返回当前前台窗口句柄。
func Foreground() uintptr {
	h, _, _ := procFG.Call()
	return h
}

// FindWindow 按类名或标题片段（不区分大小写）找第一个可见窗口。类名优先。
func FindWindow(class, title string) (uintptr, error) {
	var found uintptr
	var matched string
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		if v, _, _ := procVis.Call(hwnd); v == 0 {
			return 1 // 继续枚举
		}
		if class != "" {
			if name := windowText(procCls, hwnd); containsFold(name, class) {
				found, matched = hwnd, "class:"+class
				return 0
			}
		}
		if title != "" {
			if name := windowText(procText, hwnd); containsFold(name, title) {
				found, matched = hwnd, "title:"+title
				return 0
			}
		}
		return 1
	})
	_, _, _ = procEnum.Call(cb, 0)
	if found == 0 {
		return 0, ErrWindowNotFound
	}
	_ = matched
	return found, nil
}

// ClientRect 返回窗口客户区在屏幕坐标系里的矩形（物理像素）。
func ClientRect(hwnd uintptr) (RECT, error) {
	var r RECT
	if ok, _, _ := procCR.Call(hwnd, uintptr(unsafe.Pointer(&r))); ok == 0 {
		return RECT{}, fmt.Errorf("GetClientRect failed")
	}
	if r.Right <= 0 || r.Bottom <= 0 {
		return RECT{}, ErrClientTooSmall
	}
	var p POINT
	if ok, _, _ := procC2S.Call(hwnd, uintptr(unsafe.Pointer(&p))); ok == 0 {
		return RECT{}, fmt.Errorf("ClientToScreen failed")
	}
	return RECT{Left: p.X, Top: p.Y, Right: p.X + (r.Right - r.Left), Bottom: p.Y + (r.Bottom - r.Top)}, nil
}

// Grab 抓取屏幕上的指定矩形，返回 RGBA 图像。
func Grab(r RECT) (*image.RGBA, error) {
	w := int(r.Right - r.Left)
	h := int(r.Bottom - r.Top)
	if w <= 0 || h <= 0 {
		return nil, fmt.Errorf("bad rect %dx%d", w, h)
	}
	screenDC, _, _ := procDC.Call(0)
	if screenDC == 0 {
		return nil, fmt.Errorf("GetDC failed")
	}
	defer procRDC.Call(0, screenDC)

	memDC, _, _ := procCreateDC.Call(screenDC)
	if memDC == 0 {
		return nil, fmt.Errorf("CreateCompatibleDC failed")
	}
	defer procDeleteDC.Call(memDC)

	// 直接用 DIB section：像素内存由 GDI 提供，BitBlt 之后可以从这块内存读，
	// 不经过 GetDIBits（后者对 compatible bitmap 经常静默失败）。
	bi := BITMAPINFO{}
	bi.Header.Size = uint32(unsafe.Sizeof(bi.Header))
	bi.Header.Width = int32(w)
	bi.Header.Height = -int32(h) // 负数 = 自顶向下
	bi.Header.Planes = 1
	bi.Header.BitCount = 32
	bi.Header.Compression = 0

	var bits unsafe.Pointer
	hbmp, _, _ := procDIBSection.Call(screenDC, uintptr(unsafe.Pointer(&bi)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 || bits == nil {
		return nil, fmt.Errorf("CreateDIBSection failed")
	}
	defer procDeleteObj.Call(hbmp)

	old, _, _ := procSelect.Call(memDC, hbmp)
	if ok, _, _ := procBitBlt.Call(memDC, 0, 0, uintptr(w), uintptr(h), screenDC, uintptr(r.Left), uintptr(r.Top), srcCopy); ok == 0 {
		procSelect.Call(memDC, old)
		return nil, fmt.Errorf("BitBlt failed")
	}
	procSelect.Call(memDC, old)

	buf := unsafe.Slice((*byte)(bits), w*h*4)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		src := buf[y*w*4:]
		dst := img.Pix[y*img.Stride:]
		for x := 0; x < w; x++ {
			// BGRA → RGBA，A 置为不透明
			dst[x*4+0] = src[x*4+2]
			dst[x*4+1] = src[x*4+1]
			dst[x*4+2] = src[x*4+0]
			dst[x*4+3] = 0xFF
		}
	}
	return img, nil
}

func windowText(proc *syscall.LazyProc, hwnd uintptr) string {
	buf := make([]uint16, 512)
	n, _, _ := proc.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

// containsFold 做不区分大小写的子串匹配（避免依赖 strings.ToLower 的分配开销在这里无所谓，但保持简单）。
func containsFold(s, sub string) bool {
	if sub == "" {
		return true
	}
	ls, lsub := lowerASCII(s), lowerASCII(sub)
	return len(lsub) <= len(ls) && indexOf(ls, lsub) >= 0
}

func lowerASCII(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// 预定义错误：调用方据此决定"跳过这拍"还是"回退整屏"。
var (
	ErrWindowNotFound = errors.New("window not found")
	ErrClientTooSmall = errors.New("client area too small")
)

// WindowFromPoint 返回屏幕坐标点处最上层的窗口。
// Win64 上 POINT 是按值传的 8 字节结构：低 32 位 x，高 32 位 y。
func WindowFromPoint(x, y int32) uintptr {
	packed := uintptr(uint32(x)) | uintptr(uint32(y))<<32
	h, _, _ := procWFP.Call(packed)
	return h
}

// RootWindow 返回窗口的顶层祖先。
func RootWindow(hwnd uintptr) uintptr {
	const gaRoot = 2
	r, _, _ := procAnc.Call(hwnd, gaRoot)
	if r == 0 {
		return hwnd
	}
	return r
}
