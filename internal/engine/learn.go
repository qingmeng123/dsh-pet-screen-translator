package engine

import "dsh/local/screentranslator/internal/config"

// regionLearner 从"画面在变"的帧里学出文字区：打字机输出时，反复变化的那些行就是对话框。
//
// 它不猜边框、不跑 OCR，只用已经在算的 64×48 指纹：
//   - 判稳失败（画面在变）时，把"这一行变了多少格"累加到 hot；
//   - 攒够 minTicks 个样本后，把 hot 里平均变化 ≥ cellHits 的连续行挑出来；
//   - 上下各留 1 行余量，换算成 0～1 的相对矩形。
//
// 学到的区域会比固定值更贴合实际游戏，图标闪烁那种零散变化行不会达标。
type regionLearner struct {
	hot      [sigH]int
	ticks    int
	learned  config.Region
	hasLearn bool
}

const (
	learnMinTicks = 30 // 至少观察 30 个"在变"的帧再下结论
	learnCellHits = 8  // 这一行平均每帧有 8 格在变，才算文字行
	learnMinRows  = 4  // 少于 4 行不认（可能只是图标动画）
)

// observe 用一帧"在变"的画面喂给学习器（a、b 是前后两帧指纹）。
func (l *regionLearner) observe(a, b []byte) {
	if len(a) == 0 || len(a) != len(b) {
		return
	}
	for y := 0; y < sigH; y++ {
		row := 0
		for x := 0; x < sigW; x++ {
			i := y*sigW + x
			d := int(a[i]) - int(b[i])
			if d < 0 {
				d = -d
			}
			if d > 12 {
				row++
			}
		}
		l.hot[y] += row
	}
	l.ticks++
}

// ready 表示样本够了，可以结算一次。
func (l *regionLearner) ready() bool { return l.ticks >= learnMinTicks }

// settle 结算出区域；样本不足或找不到合适的行带时返回 false。
func (l *regionLearner) settle() (config.Region, bool) {
	if !l.ready() {
		return config.Region{}, false
	}
	top, bottom := -1, -1
	for y := 0; y < sigH; y++ {
		if l.hot[y]/l.ticks >= learnCellHits {
			if top < 0 {
				top = y
			}
			bottom = y
		}
	}
	// 重置，下一轮重新观察
	l.hot = [sigH]int{}
	l.ticks = 0

	if top < 0 || bottom-top+1 < learnMinRows {
		return config.Region{}, false
	}
	// 上下各留一行余量，并夹在 0～1 之间
	top--
	bottom++
	if top < 0 {
		top = 0
	}
	if bottom > sigH-1 {
		bottom = sigH - 1
	}
	r := config.Region{
		X0: 0,
		Y0: float64(top) / sigH,
		X1: 1,
		Y1: float64(bottom+1) / sigH,
	}
	if r.Y1-r.Y0 < 0.08 { // 太扁，不可信
		return config.Region{}, false
	}
	l.learned, l.hasLearn = r, true
	return r, true
}

// current 返回已学到的区域（没有则 false）。
func (l *regionLearner) current() (config.Region, bool) { return l.learned, l.hasLearn }

// reset 清空学习结果（切回固定模式时用）。
func (l *regionLearner) reset() {
	l.hot = [sigH]int{}
	l.ticks = 0
	l.learned = config.Region{}
	l.hasLearn = false
}
