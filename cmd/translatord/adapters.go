package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"dsh/local/screentranslator/internal/config"
	"dsh/local/screentranslator/internal/engine"
	"dsh/local/screentranslator/internal/pet"
	"dsh/local/screentranslator/internal/translate"
)

// trAdapter 把 translate.Client 适配成 engine.Translator。
type trAdapter struct {
	c   *translate.Client
	cfg config.Config
}

func (a trAdapter) Image(ctx context.Context, jpeg []byte) (string, engine.Usage, error) {
	res, err := a.c.TranslateImage(ctx, jpeg, a.cfg.VisionSystem, "把这张游戏画面里的英文翻译成中文，只输出译文。")
	if err != nil {
		return "", engine.Usage{}, err
	}
	return res.Text, engine.Usage{Prompt: res.Usage.Prompt, Completion: res.Usage.Completion}, nil
}

// StreamImage 让 trAdapter 满足 engine.Streamer：边收边回调累计译文。
func (a trAdapter) StreamImage(ctx context.Context, jpeg []byte, onUpdate func(string)) (string, engine.Usage, error) {
	res, err := a.c.StreamImage(ctx, jpeg, a.cfg.VisionSystem, "把这张游戏画面里的英文翻译成中文，只输出译文。", onUpdate)
	if err != nil {
		return "", engine.Usage{}, err
	}
	return res.Text, engine.Usage{Prompt: res.Usage.Prompt, Completion: res.Usage.Completion}, nil
}

func (a trAdapter) Text(ctx context.Context, text string) (string, error) {
	res, err := a.c.TranslateText(ctx, text, a.cfg.TextSystem)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// petAdapter 把 pet.Client 适配成 engine.Pet。
type petAdapter struct {
	c        *pet.Client
	petID    string
	duration time.Duration
	// speak 为 false 时 SaySpoken 退化成只写气泡。
	speak bool
	// surface 是播报端：desktop / web；必须与宠物 display 一致。
	surface string
}

func (p petAdapter) Lock(ctx context.Context) (string, error) {
	return p.c.Lock(ctx, p.petID, 15*time.Minute)
}

func (p petAdapter) Say(ctx context.Context, lockID, text string) error {
	return p.c.Broadcast(ctx, p.petID, lockID, text, p.duration, false, "any")
}

// SaySpoken 只给完整译文用：写气泡并要求对应端点念出来。
func (p petAdapter) SaySpoken(ctx context.Context, lockID, text string) error {
	if !p.speak {
		return p.Say(ctx, lockID, text)
	}
	return p.c.Broadcast(ctx, p.petID, lockID, text, p.duration, true, p.surface)
}

// SayFor 让 petAdapter 满足 engine.TimedSpeaker：指定这条气泡显示多久。
func (p petAdapter) SayFor(ctx context.Context, lockID, text string, dur time.Duration, spoken bool) error {
	if dur <= 0 {
		dur = p.duration
	}
	if !spoken || !p.speak {
		return p.c.Broadcast(ctx, p.petID, lockID, text, dur, false, "any")
	}
	return p.c.Broadcast(ctx, p.petID, lockID, text, dur, true, p.surface)
}

func (p petAdapter) Release(ctx context.Context, lockID string) error {
	return p.c.Release(ctx, p.petID, lockID)
}

var keyRe = regexp.MustCompile(`DEEPSEEK_API_KEY:\s*(\S+)`)

// loadAPIKey 取 DeepSeek key：环境变量优先，其次 DSH 凭据文件。key 不落日志。
func loadAPIKey(home string) (string, error) {
	if v := os.Getenv("DEEPSEEK_API_KEY"); v != "" {
		return v, nil
	}
	path := filepath.Join(home, ".dsh", ".credentials.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("no DEEPSEEK_API_KEY (env or %s): %w", path, err)
	}
	m := keyRe.FindSubmatch(raw)
	if len(m) < 2 {
		return "", fmt.Errorf("DEEPSEEK_API_KEY not found in %s", path)
	}
	return string(m[1]), nil
}
