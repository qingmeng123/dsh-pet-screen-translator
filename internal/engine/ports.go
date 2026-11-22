// Package engine 是屏幕翻译的调度核心：抓屏 → 判稳 → 翻译 → 写气泡。
//
// 它只依赖下面这些接口，不碰 Win32、不碰 HTTP、不碰 PowerShell，
// 因此可以用假时钟 + 假实现把「打字机 / 稳定 / 去重 / 降级」全部做成单测。
package engine

import (
	"context"
	"image"
)

// Shot 是一次抓屏结果。
type Shot struct {
	Image  image.Image
	JPEG   []byte
	W, H   int
	Source string // full / window / rect
	// Skipped 非空表示这一拍不该翻译（例如目标窗口不在前台），Engine 会直接跳过。
	Skipped string
}

// Capturer 抓一帧。
type Capturer interface {
	Capture(ctx context.Context) (Shot, error)
}

// OCR 从画面里取文字，只用于判稳/判重。
type OCR interface {
	Text(ctx context.Context, image []byte) (string, error)
}

// Usage 是模型返回的 token 统计。
type Usage struct {
	Prompt, Completion int
}

// Translator 提供两条翻译通道。
type Translator interface {
	Image(ctx context.Context, jpeg []byte) (string, Usage, error)
	Text(ctx context.Context, text string) (string, error)
}

// Streamer 是可选的流式能力：实现了就能边生成边更新气泡。
// onUpdate 收到的是**累计**译文，不是增量。
type Streamer interface {
	StreamImage(ctx context.Context, jpeg []byte, onUpdate func(string)) (string, Usage, error)
}

// Pet 是桌宠的说话通道。
type Pet interface {
	Lock(ctx context.Context) (string, error)
	// Say 只把文本写进气泡，不出声（流式半截、状态提示用）。
	Say(ctx context.Context, lockID, text string) error
	// SaySpoken 写进气泡并让桌宠念出来（完整译文用）；实现方按配置决定是否真的出声。
	SaySpoken(ctx context.Context, lockID, text string) error
	Release(ctx context.Context, lockID string) error
}
