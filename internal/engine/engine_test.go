package engine

import (
	"context"
	"errors"
	"image"
	"image/color"
	"testing"
	"time"

	"dsh/local/screentranslator/internal/config"
)

// ── 假实现 ─────────────────────────────────────────────────────────────

type fakeCapturer struct{ shots []Shot }

func (f *fakeCapturer) Capture(context.Context) (Shot, error) {
	if len(f.shots) == 0 {
		return Shot{}, errors.New("no more shots")
	}
	s := f.shots[0]
	f.shots = f.shots[1:]
	return s, nil
}

// fakeOCR 按图片首字节查表返回文本（模拟同一张图 OCR 结果稳定）。
type fakeOCR struct {
	byFirstByte map[byte]string
	calls       int
}

func (f *fakeOCR) Text(_ context.Context, image []byte) (string, error) {
	f.calls++
	if len(image) == 0 {
		return "", nil
	}
	return f.byFirstByte[image[0]], nil
}

type fakeTranslator struct {
	visionErr   error
	visionText  string
	textText    string
	visionCalls int
	textCalls   int
}

func (f *fakeTranslator) Image(context.Context, []byte) (string, Usage, error) {
	f.visionCalls++
	if f.visionErr != nil {
		return "", Usage{}, f.visionErr
	}
	return f.visionText, Usage{Prompt: 836, Completion: 20}, nil
}

func (f *fakeTranslator) Text(context.Context, string) (string, error) {
	f.textCalls++
	return f.textText, nil
}

type fakePet struct {
	says     []string
	spoken   []string
	locks    int
	released []string
}

func (f *fakePet) Lock(context.Context) (string, error) {
	f.locks++
	return "lk-test", nil
}

func (f *fakePet) Say(_ context.Context, _, text string) error {
	f.says = append(f.says, text)
	return nil
}

// SaySpoken 记录「写气泡 + 念」的终稿；says 也追加，方便沿用旧断言。
func (f *fakePet) SaySpoken(_ context.Context, _, text string) error {
	f.says = append(f.says, text)
	f.spoken = append(f.spoken, text)
	return nil
}

func (f *fakePet) Release(_ context.Context, lockID string) error {
	f.released = append(f.released, lockID)
	return nil
}

// ── 工具 ───────────────────────────────────────────────────────────────

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func newEngine(cap Capturer, ocr OCR, tr Translator, pet Pet, clk *clock) *Engine {
	cfg := config.Default()
	cfg.SettleMs = 2000
	cfg.IntervalMs = 4000
	cfg.TranslateMode = "vision"
	cfg.MinChars = 12
	cfg.PixelChangeRatio = 0.02
	return &Engine{Cfg: cfg, Cap: cap, OCR: ocr, Tr: tr, Pet: pet, Now: clk.now}
}

// shot 造一帧；tag 决定画面颜色（也就决定像素指纹），因此也决定"画面有没有变"。
// 颜色按 30 递进，确保超过 changedRatio 的 12/255 阈值。
func shot(tag byte) Shot {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	v := uint8(int(tag) * 30)
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = v
		img.Pix[i+1] = v
		img.Pix[i+2] = v
		img.Pix[i+3] = 255
	}
	return Shot{Image: img, JPEG: []byte{tag}, W: 16, H: 16, Source: "window"}
}

// ── 用例 ───────────────────────────────────────────────────────────────

// 打字机：画面在变时不翻译，稳定 2 秒后才翻一次；判稳期间不调用 OCR。
func TestSettleWaitsForStableText(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(2), shot(3), shot(3), shot(3)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{
		1: "SELECT THE TORSO",
		2: "SELECT THE TORSO THAT YOU",
		3: "SELECT THE TORSO THAT YOU PREFER",
	}}
	tr := &fakeTranslator{visionText: "选择你喜欢的躯干。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 5; i++ {
		if _, err := e.Step(context.Background()); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		clk.advance(2 * time.Second)
	}

	if tr.visionCalls != 1 {
		t.Fatalf("want exactly 1 translation after settle, got %d", tr.visionCalls)
	}
	if ocr.calls != 1 {
		t.Fatalf("判稳期间不应调用 OCR，want 1 call, got %d", ocr.calls)
	}
	if len(pet.says) != 1 || pet.says[0] != "译：选择你喜欢的躯干。" {
		t.Fatalf("unexpected says: %#v", pet.says)
	}
}

// 同一画面连续出现：只翻一次，且 OCR 只跑一次。
func TestSameTextTranslatedOnce(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	text := "PLEASE GIVE IT A GIFT. KINDNESS MIND AMBITION"
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1), shot(1), shot(1)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: text}}
	tr := &fakeTranslator{visionText: "请送给它一份礼物。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 4; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 1 {
		t.Fatalf("want 1 vision call, got %d", tr.visionCalls)
	}
	if ocr.calls != 1 {
		t.Fatalf("want 1 OCR call, got %d", ocr.calls)
	}
}

// 画面真的变了（只换一个词）要重新翻。
func TestTextChangeRetranslates(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1), shot(2), shot(2), shot(2)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{
		1: "SELECT THE TORSO THAT YOU PREFER",
		2: "SELECT THE LEGS THAT YOU PREFER",
	}}
	tr := &fakeTranslator{visionText: "选择你喜欢的部位。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 5; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 2 {
		t.Fatalf("want 2 vision calls (TORSO then LEGS), got %d", tr.visionCalls)
	}
}

// 视觉失败要回退文本通道，而不是丢帧。
func TestVisionFailureFallsBackToText(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	text := "SELECT THE TORSO THAT YOU PREFER"
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: text}}
	tr := &fakeTranslator{visionErr: errors.New("boom"), textText: "选择你偏好的躯干。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 2; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 1 || tr.textCalls != 1 {
		t.Fatalf("want 1 vision + 1 text fallback, got %d/%d", tr.visionCalls, tr.textCalls)
	}
	if len(pet.says) != 1 {
		t.Fatalf("want 1 say, got %d", len(pet.says))
	}
}

// 窗口不在前台：整拍跳过，不 OCR 不翻译。
func TestSkippedShotDoesNothing(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	cap := &fakeCapturer{shots: []Shot{{Skipped: "not-foreground", W: 100, H: 100, Source: "window"}}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{}}
	tr := &fakeTranslator{visionText: "不该出现"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	if _, err := e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tr.visionCalls != 0 || tr.textCalls != 0 {
		t.Fatal("translator should not be called")
	}
	if ocr.calls != 0 {
		t.Fatal("ocr should not be called")
	}
}

// 稳定但没有英文（纯标题/图标帧）：只 OCR 一次，之后不再重复。
func TestNonEnglishFrameOcrOnce(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1), shot(1), shot(1)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: "DELTARUNE"}}
	tr := &fakeTranslator{visionText: "不该出现"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 4; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 0 {
		t.Fatalf("sparse frame should not be translated, got %d", tr.visionCalls)
	}
	if ocr.calls != 1 {
		t.Fatalf("want 1 OCR call for a static non-English frame, got %d", ocr.calls)
	}
}

// 说完立刻放锁：翻译之外的时间桌宠保持自由。
func TestSpeakReleasesLockImmediately(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	text := "PLEASE GIVE IT A GIFT. KINDNESS MIND AMBITION"
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1), shot(1)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: text}}
	tr := &fakeTranslator{visionText: "请送给它一份礼物。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 3; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if pet.locks != 1 {
		t.Fatalf("want 1 lock, got %d", pet.locks)
	}
	if len(pet.released) != 1 || pet.released[0] != "lk-test" {
		t.Fatalf("want immediate release of lk-test, got %#v", pet.released)
	}
	e.Close(context.Background())
	if len(pet.released) != 1 {
		t.Fatalf("close should be a no-op, got %#v", pet.released)
	}
}

// 流式：边收边刷气泡，最后一定以完整译文收尾。
func TestStreamingSaysFinalText(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	text := "SELECT THE TORSO THAT YOU PREFER"
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: text}}
	tr := &fakeStreamer{chunks: []string{"选择", "你喜欢的", "躯干。"}}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 2; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.streamCalls != 1 {
		t.Fatalf("want 1 stream call, got %d", tr.streamCalls)
	}
	if len(pet.says) == 0 {
		t.Fatal("气泡应该被写过")
	}
	if last := pet.says[len(pet.says)-1]; last != "译：选择你喜欢的躯干。" {
		t.Fatalf("最后一条气泡应为完整译文，got %q", last)
	}
	// 核心契约：流式半截只写气泡，只有完整译文才念一次。
	if len(pet.spoken) != 1 || pet.spoken[0] != "译：选择你喜欢的躯干。" {
		t.Fatalf("只应在终稿念一次，got %#v", pet.spoken)
	}
	if pet.locks != 1 || len(pet.released) != 1 {
		t.Fatalf("流式应只取一次锁并释放，locks=%d released=%v", pet.locks, pet.released)
	}
}

// 流式失败要回退非流式视觉，而不是丢帧。
func TestStreamingFailureFallsBack(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	text := "SELECT THE TORSO THAT YOU PREFER"
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: text}}
	tr := &fakeStreamer{streamErr: errors.New("boom"), fallbackText: "选择你偏好的躯干。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 2; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.streamCalls != 1 || tr.imageCalls != 1 {
		t.Fatalf("want 1 stream + 1 fallback image call, got %d/%d", tr.streamCalls, tr.imageCalls)
	}
	if len(pet.says) != 1 || pet.says[0] != "译：选择你偏好的躯干。" {
		t.Fatalf("unexpected says: %#v", pet.says)
	}
}

// fakeStreamer 同时实现 Translator 与 Streamer：流式路径优先。
type fakeStreamer struct {
	chunks       []string
	streamErr    error
	fallbackText string
	streamCalls  int
	imageCalls   int
}

func (f *fakeStreamer) Image(context.Context, []byte) (string, Usage, error) {
	f.imageCalls++
	return f.fallbackText, Usage{Prompt: 780, Completion: 5}, nil
}

func (f *fakeStreamer) Text(context.Context, string) (string, error) { return f.fallbackText, nil }

func (f *fakeStreamer) StreamImage(_ context.Context, _ []byte, onUpdate func(string)) (string, Usage, error) {
	f.streamCalls++
	if f.streamErr != nil {
		return "", Usage{}, f.streamErr
	}
	full := ""
	for _, c := range f.chunks {
		full += c
		onUpdate(full)
	}
	return full, Usage{Prompt: 780, Completion: 12}, nil
}

// 像素变了但文字没变（选项高亮移位）：靠 OCR 文本相似度挡掉，不重复翻译。
func TestSameTextDifferentPixelsSkipped(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	text := "SELECT THE TORSO THAT YOU PREFER"
	// tag 1 与 tag 2 像素差 30/255，超过 2% 变化阈值；但 OCR 文本相同
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1), shot(2), shot(2), shot(2)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: text, 2: text}}
	tr := &fakeTranslator{visionText: "选择你喜欢的躯干。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 5; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 1 {
		t.Fatalf("同一段文字不应重复翻译，want 1 call, got %d", tr.visionCalls)
	}
	if len(pet.says) != 1 {
		t.Fatalf("want 1 say, got %d", len(pet.says))
	}
}

// 文本确实变了（TORSO→LEGS）即使相似度 0.71 也要翻，不能被去重挡住。
func TestDedupeDoesNotBlockRealChange(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	cap := &fakeCapturer{shots: []Shot{shot(1), shot(1), shot(2), shot(2)}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{
		1: "SELECT THE TORSO THAT YOU PREFER",
		2: "SELECT THE LEGS THAT YOU PREFER",
	}}
	tr := &fakeTranslator{visionText: "选择你喜欢的部位。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 4; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 2 {
		t.Fatalf("文本变化应重新翻译，want 2 calls, got %d", tr.visionCalls)
	}
}

// 指纹只看对话框区域：上半屏动画不该触发，下方文字变化必须能检测到。
func TestSignatureRegionIsolation(t *testing.T) {
	region := config.Region{X0: 0, Y0: 0.55, X1: 1, Y1: 1}
	mk := func() *image.RGBA {
		img := image.NewRGBA(image.Rect(0, 0, 200, 200))
		for i := 0; i < len(img.Pix); i += 4 {
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 20, 20, 20, 255
		}
		return img
	}
	fill := func(img *image.RGBA, top, bottom int, v uint8) {
		b := img.Bounds()
		for y := top; y < bottom; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				img.Set(x, y, color.RGBA{v, v, v, 255})
			}
		}
	}

	base := mk()

	// 上半屏大面积变化（角色动画）：指纹应几乎不变
	upper := mk()
	fill(upper, 0, 80, 240)
	if got := changedRatio(signature(base, region), signature(upper, region)); got > 0.02 {
		t.Fatalf("上半屏变化不应影响指纹，got %.3f", got)
	}

	// 下方对话框变化：必须检测到
	lower := mk()
	fill(lower, 150, 190, 240)
	if got := changedRatio(signature(base, region), signature(lower, region)); got < 0.05 {
		t.Fatalf("对话框区域变化必须被检测到，got %.3f", got)
	}
}
func picShot(tag byte, textVersion int, iconOn bool) Shot {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 12, 12, 12, 255
	}
	// 文本条带：位于下方区域（region 0.55–1.0），内容由 textVersion 决定
	for y := 38; y < 46; y++ {
		for x := 4; x < 56; x++ {
			if (x+textVersion*7)%9 < 5 { // 不同版本 → 花纹不同
				img.Set(x, y, color.RGBA{235, 235, 235, 255})
			}
		}
	}
	// 小图标闪烁：只影响 6×6 的一小块（每行 6 格 < 10，不该判成新画面）
	if iconOn {
		for y := 52; y < 58; y++ {
			for x := 2; x < 8; x++ {
				img.Set(x, y, color.RGBA{250, 250, 250, 255})
			}
		}
	}
	return Shot{Image: img, JPEG: []byte{tag}, W: 64, H: 64, Source: "window"}
}

// 同一段文字 + 图标闪烁：不该重复翻译。
func TestAnimatedIconDoesNotRetranslate(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	text := "PLEASE GIVE IT A GIFT. KINDNESS MIND AMBITION"
	cap := &fakeCapturer{shots: []Shot{
		picShot(1, 0, false), picShot(1, 0, false),
		picShot(2, 0, true), picShot(2, 0, true), picShot(2, 0, true),
	}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{1: text, 2: text}}
	tr := &fakeTranslator{visionText: "请送给它一份礼物。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 5; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 1 {
		t.Fatalf("图标闪烁不该触发重新翻译，want 1 call, got %d", tr.visionCalls)
	}
}

// 文本条带真的换了：必须重新翻译。
func TestTextBandChangeRetranslates(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	cap := &fakeCapturer{shots: []Shot{
		picShot(1, 0, false), picShot(1, 0, false),
		picShot(2, 1, false), picShot(2, 1, false),
	}}
	ocr := &fakeOCR{byFirstByte: map[byte]string{
		1: "PLEASE GIVE IT A GIFT. KINDNESS MIND AMBITION",
		2: "PLEASE GIVE IT A GIFT. BRAVERY VOICE JUSTICE",
	}}
	tr := &fakeTranslator{visionText: "请送给它一份礼物。"}
	pet := &fakePet{}
	e := newEngine(cap, ocr, tr, pet, clk)

	for i := 0; i < 4; i++ {
		_, _ = e.Step(context.Background())
		clk.advance(2 * time.Second)
	}
	if tr.visionCalls != 2 {
		t.Fatalf("文本条带变化应重新翻译，want 2 calls, got %d", tr.visionCalls)
	}
}

// 学习器：上方图标闪烁 + 下方持续打字 → 学到的区域应落在下方，不覆盖上方。
func TestRegionLearnerPicksTextBand(t *testing.T) {
	var l regionLearner
	// 构造 30 个"在变"的样本：上方 0～30% 只有零星变化，下方 55%～75% 每行大量变化
	for i := 0; i < learnMinTicks; i++ {
		a := make([]byte, sigW*sigH)
		b := make([]byte, sigW*sigH)
		for y := 0; y < sigH; y++ {
			frac := float64(y) / sigH
			for x := 0; x < sigW; x++ {
				i0 := y*sigW + x
				a[i0] = 10
				switch {
				case frac >= 0.55 && frac <= 0.75:
					b[i0] = 200 // 文字行：整行变
				case frac < 0.30 && x%16 == 0:
					b[i0] = 200 // 图标闪烁：每行只变 4 格
				default:
					b[i0] = 10
				}
			}
		}
		l.observe(a, b)
	}

	r, ok := l.settle()
	if !ok {
		t.Fatal("应该能学到区域")
	}
	if r.Y0 < 0.45 || r.Y1 > 0.85 {
		t.Fatalf("学到的区域应落在下方文字带，got y %.2f–%.2f", r.Y0, r.Y1)
	}
	if _, has := l.current(); !has {
		t.Fatal("学完后 current 应返回区域")
	}
}
