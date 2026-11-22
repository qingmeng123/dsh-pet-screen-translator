package engine

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"dsh/local/screentranslator/internal/config"
	"dsh/local/screentranslator/internal/logx"
)

// 对外状态值，供控制通道/外壳展示。
const (
	StateIdle        = "idle"
	StateSettling    = "settling"
	StateTranslating = "translating"
	StateError       = "error"
	// StateBlocked 是前缀，实际值形如 blocked-occluded / blocked-not-foreground：
	// 目标窗口被遮挡或不在前台，此时 GDI 抓到的是别的窗口，必须跳过。
	StateBlocked = "blocked-"
)

// Engine 把配置、依赖和运行期状态收在一起。
type Engine struct {
	Cfg config.Config
	Cap Capturer
	OCR OCR
	Tr  Translator
	Pet Pet
	Log *logx.Logger
	Now func() time.Time // 可注入，单测用假时钟

	// 判稳状态（像素指纹）
	candidateSig      []byte
	candidateSince    time.Time
	lastTranslatedSig []byte

	// 译文去重
	lastTranslated string
	// 上次翻译时的 OCR 文本（规范化后）：文字没变但像素变了（例如选项高亮移位）时用它挡掉重复翻译
	lastOcrNorm string
	// 刚翻过一模一样的译文时，短时间内不再重复翻同一屏（OCR 抖动会让文本比对失效）
	suppressUntil time.Time

	// 只为日志去重
	unstable   bool
	lastSkip   string
	lastSource string
	lockID     string

	// 判稳区域：fixed = 用配置里的固定矩形；auto = 用在线学习到的矩形（实验）
	regionMode atomic.Value // "fixed" | "auto"
	learner    regionLearner

	// 对外状态：idle / settling / translating / error / blocked-*
	state atomic.Value
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) logf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Info(format, args...)
	}
}

func (e *Engine) warnf(format string, args ...any) {
	if e.Log != nil {
		e.Log.Warn(format, args...)
	}
}

// setState 记录对外状态。
func (e *Engine) setState(s string) { e.state.Store(s) }

// State 返回当前状态。
func (e *Engine) State() string {
	if v, ok := e.state.Load().(string); ok {
		return v
	}
	return StateIdle
}

// Acquire 取占用锁；失败返回错误。
func (e *Engine) Acquire(ctx context.Context) error {
	if e.lockID != "" {
		return nil
	}
	id, err := e.Pet.Lock(ctx)
	if err != nil {
		return err
	}
	e.lockID = id
	return nil
}

// Speak 往气泡里写一句话：**取锁 → 说 → 立刻放锁**。
// 只在说话这一瞬间占用宠物，两次翻译之间它照常自由活动。
func (e *Engine) Speak(ctx context.Context, text string) error {
	if err := e.Acquire(ctx); err != nil {
		return err
	}
	err := e.Pet.Say(ctx, e.lockID, text)
	e.release()
	return err
}

// release 立刻释放占用锁（幂等）。
func (e *Engine) release() {
	if e.lockID == "" {
		return
	}
	_ = e.Pet.Release(context.Background(), e.lockID)
	e.lockID = ""
}

// Close 释放占用锁（幂等）。
func (e *Engine) Close(ctx context.Context) {
	e.release()
}

// Step 跑一拍，返回建议的下一拍间隔。
//
// 判稳**不再依赖 OCR**：每拍只算 64×48 灰度指纹，画面变化超过阈值就重置计时；
// 稳定满 settleMs 之后才做一次 OCR（用于英文门槛与日志），然后翻译。
// 这样判稳阶段每拍只有抓屏 36 ms + 指纹 1 ms，不必每 300 ms 起一个 PowerShell。
func (e *Engine) Step(ctx context.Context) (time.Duration, error) {
	started := time.Now()
	interval := time.Duration(e.Cfg.IntervalMs) * time.Millisecond
	settle := time.Duration(e.Cfg.SettleMs) * time.Millisecond

	shot, err := e.Cap.Capture(ctx)
	if err != nil {
		e.warnf("capture failed: %v", err)
		e.setState(StateError)
		return interval, nil
	}

	if src := shot.Source; src != e.lastSource {
		e.lastSource = src
		e.logf("抓取范围：%s %dx%d", src, shot.W, shot.H)
	}
	if shot.Skipped != "" {
		if e.lastSkip != shot.Skipped {
			e.lastSkip = shot.Skipped
			e.logf("跳过：%s（切回游戏窗口即可恢复）", shot.Skipped)
		}
		e.setState(StateBlocked + shot.Skipped)
		return interval, nil
	}
	e.lastSkip = ""

	sig := signature(shot.Image, e.effectiveRegion())
	if len(sig) == 0 {
		e.warnf("抓屏没有图像数据，跳过这一拍")
		e.setState(StateError)
		return interval, nil
	}

	// 这一帧已经翻过了（画面没再明显变化）→ 什么都不做
	if len(e.lastTranslatedSig) > 0 && sameScreen(sig, e.lastTranslatedSig) {
		e.setState(StateIdle)
		return interval, nil
	}

	changed := len(e.candidateSig) == 0 || changedRatio(sig, e.candidateSig) >= e.Cfg.PixelChangeRatio
	if changed {
		// auto 模式下，趁"画面在变"顺手学习文字区
		if e.RegionMode() == "auto" {
			e.learner.observe(e.candidateSig, sig)
			if r, ok := e.learner.settle(); ok {
				e.logf("学到的判稳区域：y %.2f–%.2f", r.Y0, r.Y1)
			}
		}
		e.candidateSig = sig
		e.candidateSince = e.now()
		if !e.unstable {
			e.unstable = true
			e.logf("画面变化中，等 %d ms 稳定后再翻", e.Cfg.SettleMs)
		}
		e.setState(StateSettling)
		return settleRemaining(settle, started), nil
	}
	e.unstable = false

	if e.now().Sub(e.candidateSince) < settle {
		e.setState(StateSettling)
		return settleRemaining(settle, started), nil
	}

	// 画面稳定：这时才做一次 OCR，用来确认「是不是英文」并留日志
	ocrStart := time.Now()
	raw, oerr := e.OCR.Text(ctx, shot.JPEG)
	ocrMs := time.Since(ocrStart).Milliseconds()
	if oerr != nil {
		e.warnf("ocr failed: %v", oerr)
		e.setState(StateError)
		return interval, nil
	}
	text := cleanText(raw)

	// 稳定但没有值得翻译的英文（菜单标题、纯图标界面）→ 记下这一帧，别再重复 OCR
	if len([]rune(text)) < e.Cfg.MinChars || !looksEnglish(text) {
		e.lastTranslatedSig = sig
		e.setState(StateIdle)
		return interval, nil
	}

	// 译文重复后的抑制期：OCR 抖动会让"文本比对"失效，所以用"刚翻过同一屏"兜底。
	norm := normalize(text)
	if e.now().Before(e.suppressUntil) && similarity(norm, e.lastOcrNorm) >= 0.5 {
		e.lastTranslatedSig = sig
		e.setState(StateIdle)
		return interval, nil
	}

	// 文本级去重：像素变了但文字没变（选项高亮移位、背景动画）时不再翻一遍。
	if e.lastOcrNorm != "" && e.lastTranslated != "" {
		if sim := similarity(norm, e.lastOcrNorm); sim >= e.Cfg.DedupeSimilarity {
			e.lastTranslatedSig = sig
			e.logf("与上次翻译的文本相同（相似度 %.2f），跳过翻译", sim)
			e.setState(StateIdle)
			return interval, nil
		}
	}
	e.lastOcrNorm = norm

	e.setState(StateTranslating)
	e.logf("OCR %d 字符（%d ms）：%s", len([]rune(text)), ocrMs, truncate(text, 60))

	zh, usage, said, terr := e.translateAndSay(ctx, shot, text)
	if terr != nil {
		e.warnf("翻译失败：%v", terr)
		e.setState(StateError)
		return interval, nil
	}
	// 无论气泡是否真的更新，这一帧都算处理过了
	e.lastTranslatedSig = sig

	if !said {
		e.logf("译文与上一条相同，跳过重复弹泡：%s", truncate(zh, 20))
		e.setState(StateIdle)
		return interval, nil
	}
	if usage.Prompt > 0 {
		e.logf("视觉翻译 prompt=%d completion=%d（%d 字符）", usage.Prompt, usage.Completion, len([]rune(text)))
	}
	e.logf("已翻译 %d 字符：%s", len([]rune(text)), truncate(zh, 40))
	e.setState(StateIdle)
	return interval, nil
}

// translateAndSay 负责「翻译 + 写气泡」，返回译文、用量、以及这次有没有真的说话（去重用）。
// 视觉通道优先走流式：先出一个字再逐步补全，观众不用等整段。
func (e *Engine) translateAndSay(ctx context.Context, shot Shot, text string) (string, Usage, bool, error) {
	if e.Cfg.Stream && e.Cfg.TranslateMode == "vision" && len(shot.JPEG) > 0 {
		if st, ok := e.Tr.(Streamer); ok {
			zh, usage, err := e.sayStream(ctx, st, shot.JPEG)
			if err == nil {
				return zh, usage, true, nil
			}
			e.warnf("流式翻译失败（%v），回退非流式", err)
		}
	}

	zh, usage, err := e.translate(ctx, shot, text)
	if err != nil {
		return "", usage, false, err
	}
	if e.lastTranslated != "" && prefix(zh, 10) == prefix(e.lastTranslated, 10) {
		e.lastTranslated = zh
		return zh, usage, false, nil
	}
	prev := e.lastTranslated
	e.lastTranslated = zh
	if err := e.Speak(ctx, e.Cfg.Prefix+truncate(zh, 300)); err != nil {
		return zh, usage, false, fmt.Errorf("写气泡失败: %w", err)
	}
	e.noteTranslation(zh, prev)
	return zh, usage, true, nil
}

// sayStream 边收边写气泡：取一次锁，按 ~120 ms 的节奏把累计译文刷进气泡，结束后再补一次终稿。
func (e *Engine) sayStream(ctx context.Context, st Streamer, jpeg []byte) (string, Usage, error) {
	if err := e.Acquire(ctx); err != nil {
		return "", Usage{}, err
	}
	defer e.release()

	var (
		lastSent time.Time
		lastText string
		started  = time.Now()
		first    = true
	)
	push := func(full string, force bool) {
		full = strings.TrimSpace(full)
		if full == "" || full == lastText {
			return
		}
		if !force && time.Since(lastSent) < 120*time.Millisecond {
			return // 刷太快没意义，还会把桌宠接口打满
		}
		lastSent = time.Now()
		lastText = full
		if first {
			first = false
			e.logf("流式首字 %d ms：%s", time.Since(started).Milliseconds(), truncate(full, 20))
		}
		if err := e.Pet.Say(ctx, e.lockID, e.Cfg.Prefix+truncate(full, 300)); err != nil {
			e.warnf("流式写气泡失败：%v", err)
		}
	}

	prev := e.lastTranslated
	zh, usage, err := st.StreamImage(ctx, jpeg, func(full string) { push(full, false) })
	if err != nil {
		if lastText != "" {
			push(lastText, true) // 半截也算，别让气泡空着
		}
		return lastText, usage, err
	}
	zh = strings.TrimSpace(zh)
	if zh != "" {
		push(zh, true)
		if dup := e.noteTranslation(zh, prev); dup {
			e.logf("译文与上一条几乎相同，%d 秒内不再翻这一屏", int(e.Cfg.SuppressSeconds))
		}
		e.lastTranslated = zh
	}
	return zh, usage, nil
}

// translate 按配置选通道；视觉失败自动回退文本。
func (e *Engine) translate(ctx context.Context, shot Shot, text string) (string, Usage, error) {
	if e.Cfg.TranslateMode == "vision" && len(shot.JPEG) > 0 {
		zh, usage, err := e.Tr.Image(ctx, shot.JPEG)
		if err == nil {
			return zh, usage, nil
		}
		e.warnf("视觉翻译失败（%v），回退 OCR 文本", err)
	}
	zh, err := e.Tr.Text(ctx, text)
	return zh, Usage{}, err
}

// Run 是常驻循环，直到 ctx 取消。首拍立即执行，让「开启」尽快有反应。
func (e *Engine) Run(ctx context.Context) {
	for {
		delay, err := e.Step(ctx)
		if err != nil {
			e.warnf("step error: %v", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// settleRemaining 把「判稳间隔」解释为**周期**：本拍已经花了多少（抓屏+指纹），就只补剩下的。
func settleRemaining(settle time.Duration, started time.Time) time.Duration {
	left := settle - time.Since(started)
	if left < 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	return left
}

// noteTranslation 记录一条译文；如果与上一条几乎相同（OCR 抖动产生的重复屏），
// 就开启一段抑制期，避免同一屏被反复翻译。返回是否判定为重复。
func (e *Engine) noteTranslation(zh, prev string) bool {
	if prev == "" || charSimilarity(zh, prev) < 0.85 {
		return false
	}
	// 译文几乎一样 → 判定为"同一屏又翻了一遍"，开一段抑制期。
	secs := e.Cfg.SuppressSeconds
	if secs <= 0 {
		secs = 15
	}
	e.suppressUntil = e.now().Add(time.Duration(secs) * time.Second)
	return true
}

// RegionMode 返回当前判稳区域模式。
func (e *Engine) RegionMode() string {
	if v, ok := e.regionMode.Load().(string); ok {
		return v
	}
	return "fixed"
}

// SetRegionMode 切换 fixed / auto；切回 fixed 会清空学习结果。
func (e *Engine) SetRegionMode(mode string) {
	if mode != "auto" {
		mode = "fixed"
	}
	e.regionMode.Store(mode)
	if mode == "fixed" {
		e.learner.reset()
	}
}

// effectiveRegion 返回这一拍该用的指纹区域。
func (e *Engine) effectiveRegion() config.Region {
	if e.RegionMode() == "auto" {
		if r, ok := e.learner.current(); ok {
			return r
		}
	}
	return e.Cfg.FingerprintRegion
}
