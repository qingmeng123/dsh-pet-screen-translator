// Package config 负责配置的加载与默认值。
//
// 优先级：命令行 > config.json > 内置默认值。
// 读取时会剥掉 UTF-8 BOM —— PowerShell 写的 JSON 常带 BOM，json.Unmarshal 会直接报错。
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// Region 用相对比例描述画面的一块矩形（0～1），用于"只看对话框区域"的指纹。
type Region struct {
	X0 float64 `json:"x0"`
	Y0 float64 `json:"y0"`
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
}

// Normalized 返回可用的矩形：越界或颠倒时退回"整屏"。
func (r Region) Normalized() Region {
	if r.X1 <= r.X0 || r.Y1 <= r.Y0 || r.X0 < 0 || r.Y0 < 0 || r.X1 > 1 || r.Y1 > 1 {
		return Region{X0: 0, Y0: 0, X1: 1, Y1: 1}
	}
	return r
}

// Config 是运行期全部可调项。JSON 标签与 JS 版共用同一份 config.json。
type Config struct {
	WindowClass string `json:"windowClass"`
	WindowTitle string `json:"window"`

	IntervalMs int `json:"intervalMs"`
	// PixelChangeRatio 是判稳阈值：64×48 指纹里变化像素占比 ≥ 它就认为画面变了（默认 0.02）。
	PixelChangeRatio float64 `json:"pixelChangeRatio"`
	SettleMs   int `json:"settleMs"`
	MinChars   int `json:"minChars"`
	MaxChars   int `json:"maxChars"`

	Lang   string `json:"lang"`
	Prefix string `json:"prefix"`

	DurationMs int `json:"duration"`

	TranslateMode      string `json:"translateMode"`
	TranslateTimeoutMs int    `json:"translateTimeoutMs"`

	VisionTimeoutMs int    `json:"visionTimeoutMs"`
	VisionMaxTokens int    `json:"visionMaxTokens"`
	VisionAPIBase   string `json:"visionApiBase"`
	VisionSystem    string `json:"visionSystem"`
	TextSystem      string `json:"system"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
	// DedupeSimilarity 是文本去重阈值：新 OCR 文本与上次相似度 ≥ 它就跳过翻译（默认 0.9）。
	DedupeSimilarity float64 `json:"dedupeSimilarity"`
	// IdleExtendMs 是「长时间没有新译文」时给气泡续的时长（默认 10000 = +10 秒）。
	IdleExtendMs int `json:"idleExtendMs"`
	// TargetMode：fixed = 按 WindowClass/Window 找窗口；auto = 跟随前台窗口（换游戏不用改配置）。
	TargetMode string `json:"targetMode"`
	// MinScreenPercent 是自动模式的最低占屏比例（0.25 = 25%），太小不当游戏窗口。
	MinScreenPercent float64 `json:"minScreenPercent"`
	// OCRWorker 为 true 时用常驻 OCR 进程（只启动一次 PowerShell），显著降低单次识别延迟。
	OCRWorker bool `json:"ocrWorker"`
	// RegionMode：fixed = 用 FingerprintRegion 固定矩形；auto = 在线学习文字区（实验）。
	RegionMode string `json:"regionMode"`
	// SuppressSeconds 是"译文重复"后的抑制秒数：这段时间内同一屏不再重复翻（默认 15）。
	SuppressSeconds int `json:"suppressSeconds"`
	// FingerprintRegion 是判稳指纹的采样区域：默认只看下方对话框，忽略角色动画等无关变化。
	FingerprintRegion Region `json:"fingerprintRegion"`
	// Stream 为 true 时边翻译边更新气泡（首字明显更快）。
	Stream bool `json:"stream"`
	// Speak 为 true 时，完整译文出来后让桌宠用配置的音色念一遍；流式半截永远不念。
	Speak bool `json:"speak"`
	// VoiceSurface 是念给哪一端听：desktop / web。必须与宠物实际 display 一致，
	// 否则未显示的那一端会被 pet-not-displayed 拒绝（连气泡也不更新）。
	VoiceSurface string `json:"voiceSurface"`

	OCRScriptPath string `json:"ocrScriptPath"`
	PowerShell    string `json:"powerShell"`
	OCRTimeoutMs  int    `json:"ocrTimeoutMs"`

	DSHPort     int    `json:"dshPort"`
	PetBasePath string `json:"petBasePath"`

	LogMaxBytes int `json:"logMaxBytes"`
	LogKeep     int `json:"logKeep"`

	// SettleSimilarity 是判稳阈值：与上一帧文本相似度 ≥ 它即视为同一段。
	SettleSimilarity float64 `json:"settleSimilarity"`
}

// Default 返回内置默认值。
func Default() Config {
	return Config{
		IntervalMs:         4000,
		SettleMs:           100,
		PixelChangeRatio:   0.005, // 0.5%：能抓住"只多一行字"（约 1.2%），又放过光标闪烁（约 0.03%）
		DedupeSimilarity:   0.9,
		SuppressSeconds:    15,
		IdleExtendMs:       10000,
		TargetMode:         "auto",
		MinScreenPercent:   0.25,
		OCRWorker:          true,
		RegionMode:         "fixed",
		// 默认只看窗口下方 45%：绝大多数 RPG 对话框都在这一带
		FingerprintRegion: Region{X0: 0, Y0: 0.55, X1: 1, Y1: 1},
		MinChars:           12,
		MaxChars:           600,
		Lang:               "en-GB",
		Prefix:             "译：",
		DurationMs:         12000,
		TranslateMode:      "vision",
		TranslateTimeoutMs: 15000,
		VisionTimeoutMs:    25000,
		VisionMaxTokens:    0, // 0 = 不限制：深度思考会吃掉上限，正文反而为空
		VisionAPIBase:      "https://api.deepseek.com",
		Model:              "deepseek-flash",
		Stream:             true,
		Speak:              true,
		VoiceSurface:       "desktop",
		ReasoningEffort:    "none", // 视觉翻译不需要思考：实测 none 时 completion=6、延迟 ~0.9s（low 会波动到 8s+）
		VisionSystem: "你是游戏画面的实时翻译。把画面里的英文翻译成自然、简短的中文，只输出译文本身：" +
			"不要解释、不要加引号或括号注释、不要重复原文。菜单项、按钮、提醒用语按游戏里常见的中文说法翻。",
		TextSystem: "你是游戏画面的实时翻译。把用户给出的英文画面文字翻译成自然、简短的中文，只输出译文本身：" +
			"不要解释、不要加引号或括号注释、不要重复原文。遇到菜单项、按钮、提醒用语就按游戏里常见的中文说法翻。" +
			"原文里出现明显缺字或乱码（例如 T?E、TRAT）时按上下文补全成最可能的词再翻，不要照抄残缺字母。",
		OCRTimeoutMs:     20000,
		PetBasePath:      "/dsh-pet-7340",
		LogMaxBytes:      1 << 20,
		LogKeep:          3,
		SettleSimilarity: 0.8,
	}
}

// Load 读取配置：默认值打底，再用 path 的 JSON 覆盖；文件不存在不算错。
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			raw = []byte(strings.TrimPrefix(string(raw), "\uFEFF"))
			if err := json.Unmarshal(raw, &cfg); err != nil {
				return cfg, err
			}
		case errors.Is(err, os.ErrNotExist):
			// 用默认值
		default:
			return cfg, err
		}
	}
	cfg.normalize()
	return cfg, nil
}

// normalize 把非法值拉回默认，避免运行期出现 0 间隔死循环之类的问题。
func (c *Config) normalize() {
	d := Default()
	if c.IntervalMs < 500 {
		c.IntervalMs = d.IntervalMs
	}
	if c.SettleMs < 0 {
		c.SettleMs = d.SettleMs
	}
	if c.MinChars < 1 {
		c.MinChars = d.MinChars
	}
	if c.MaxChars < c.MinChars {
		c.MaxChars = d.MaxChars
	}
	if c.Lang == "" {
		c.Lang = d.Lang
	}
	if c.DurationMs <= 0 {
		c.DurationMs = d.DurationMs
	}
	if c.TranslateMode != "vision" && c.TranslateMode != "ocr" {
		c.TranslateMode = d.TranslateMode
	}
	if c.VisionAPIBase == "" {
		c.VisionAPIBase = d.VisionAPIBase
	}
	if c.Model == "" {
		c.Model = d.Model
	}
	if c.PetBasePath == "" {
		c.PetBasePath = d.PetBasePath
	}
	if c.MinScreenPercent <= 0 || c.MinScreenPercent > 1 {
		c.MinScreenPercent = d.MinScreenPercent
	}
	if c.DedupeSimilarity <= 0 || c.DedupeSimilarity > 1 {
		c.DedupeSimilarity = d.DedupeSimilarity
	}
	if c.PixelChangeRatio <= 0 || c.PixelChangeRatio > 1 {
		c.PixelChangeRatio = d.PixelChangeRatio
	}
	if c.SettleSimilarity <= 0 || c.SettleSimilarity > 1 {
		c.SettleSimilarity = d.SettleSimilarity
	}
	if c.VoiceSurface != "desktop" && c.VoiceSurface != "web" {
		c.VoiceSurface = d.VoiceSurface
	}
	if c.LogMaxBytes < 4096 {
		c.LogMaxBytes = d.LogMaxBytes
	}
	if c.LogKeep < 1 {
		c.LogKeep = d.LogKeep
	}
}

// ProjectConfigPath 返回工作区里与本项目共用的 config.json。
func ProjectConfigPath(workspace string) string {
	return filepath.Join(workspace, "dsh-pet-screen-translator", "config.json")
}
