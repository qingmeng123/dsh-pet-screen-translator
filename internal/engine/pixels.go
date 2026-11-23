package engine

import (
	"image"

	"dsh/local/screentranslator/internal/config"
)

// sigW / sigH 是帧指纹的缩略尺寸：64×48 = 3072 字节，比一张 1280×960 的 JPEG 小三个数量级，
// 但足以区分"文案变了"和"光标在闪"。
const (
	sigW = 64
	sigH = 48
)

// signature 把画面的指定区域缩成 64×48 灰度指纹。
//
// 只采样 region（默认窗口下方 45% 的对话框区）：角色动画、粒子效果、背景滚屏都在别处，
// 不该让它们参与"画面变没变"的判断 —— 否则同一段文案会被反复翻译。
// 逐点采样，不做插值：判稳不需要精度，只要稳定。
func signature(img image.Image, region config.Region) []byte {
	if img == nil {
		return nil
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil
	}

	r := region.Normalized()
	x0 := b.Min.X + int(float64(w)*r.X0)
	x1 := b.Min.X + int(float64(w)*r.X1)
	y0 := b.Min.Y + int(float64(h)*r.Y0)
	y1 := b.Min.Y + int(float64(h)*r.Y1)
	sw, sh := x1-x0, y1-y0
	if sw < 8 || sh < 8 {
		// 区域太小（或配错了）就退回整屏，宁可多翻也不要漏
		x0, x1 = b.Min.X, b.Max.X
		y0, y1 = b.Min.Y, b.Max.Y
		sw, sh = w, h
	}

	out := make([]byte, sigW*sigH)
	for y := 0; y < sigH; y++ {
		sy := y0 + y*sh/sigH
		for x := 0; x < sigW; x++ {
			sx := x0 + x*sw/sigW
			rr, gg, bb, _ := img.At(sx, sy).RGBA()
			// 0.299/0.587/0.114 的定点近似
			lum := (9798*uint32(rr) + 19235*uint32(gg) + 3735*uint32(bb)) >> 24
			out[y*sigW+x] = byte(lum)
		}
	}
	return out
}

// changedRatio 返回两张指纹中"变化明显"的像素占比（0～1）。
// 阈值用像素差 > 12（0～255），滤掉压缩噪声与轻微渐变动画。
func changedRatio(a, b []byte) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 1 // 没有可比对象，当作全变
	}
	const threshold = 12
	changed := 0
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		if d > threshold {
			changed++
		}
	}
	return float64(changed) / float64(len(a))
}

// sameScreen 判断当前画面是否"还是刚才翻过的那一屏"。
//
// 只看总变化率不行：对话框里的图标/角色一直在动，总变化率会超过任何合理阈值；
// 而"换了一段台词"的特征是**某几行出现连续成片的变化**。
// 所以规则是：
//   - 任意一行出现 ≥ 10 格连续/成片变化（≈200px 宽的一行字）→ 认为是新画面；
//   - 总变化率 ≥ 5%（整屏切换）→ 认为是新画面；
//   - 其余（零散的动画像素）→ 认为是同一屏。
func sameScreen(a, b []byte) bool {
	if len(a) == 0 || len(a) != len(b) {
		return false
	}
	const (
		cellDelta  = 12 // 单格亮度差超过它算"变了"
		rowCells   = 10 // 一行里变了这么多格 → 说明这一行内容换了
		totalRatio = 0.05
	)
	changedTotal := 0
	for y := 0; y < sigH; y++ {
		row := 0
		for x := 0; x < sigW; x++ {
			i := y*sigW + x
			d := int(a[i]) - int(b[i])
			if d < 0 {
				d = -d
			}
			if d > cellDelta {
				row++
				changedTotal++
			}
		}
		if row >= rowCells {
			return false // 有整行变了 → 新画面
		}
	}
	return float64(changedTotal)/float64(len(a)) < totalRatio
}

// blackRatio 返回指纹里"接近全黑"的格子占比。
//
// 用途：独占全屏（DirectX flip）下 GDI 抓到的是黑图，OCR 什么也读不到，
// 表现成"运行中但永远不翻译"。这里提前识别，给出明确提示而不是静默失败。
func blackRatio(sig []byte) float64 {
	if len(sig) == 0 {
		return 0
	}
	const dark = 6 // 亮度阈值（0～255）
	black := 0
	for _, v := range sig {
		if v <= dark {
			black++
		}
	}
	return float64(black) / float64(len(sig))
}
