// Package ocr 把「CPU 侧的 OCR」藏在 Provider 接口后面。
//
// 当前实现调用 ocr-file.ps1（Windows.Media.Ocr）；将来换成 WinRT 原生绑定或
// Tesseract 时，只需要新增一个实现，engine 不用改。
package ocr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// ErrUnavailable 表示 OCR 引擎不可用（脚本缺失、PowerShell 不存在等）。
var ErrUnavailable = errors.New("ocr unavailable")

// Provider 是 OCR 能力的最小契约。
type Provider interface {
	// Text 识别一张图片（JPEG/PNG 都可以）。
	Text(ctx context.Context, image []byte) (string, error)
}

// PowerShell 用 ocr-file.ps1 实现 Provider。
type PowerShell struct {
	// Exe 是 powershell.exe 绝对路径。
	Exe string
	// Script 是 ocr-file.ps1 绝对路径。
	Script string
	// Lang 是识别语言，如 en-GB。
	Lang string
	// Timeout 是单次识别超时。
	Timeout time.Duration
	// TempDir 是临时图落盘目录；空则用 os.TempDir()。
	// 插件拉起守护进程时 os.TempDir() 可能不可写（沙箱），所以由调用方指定工作区目录。
	TempDir string

	seq atomic.Int64
}

// Text 把图片写到临时文件，交给 PowerShell 识别，读完即删。
func (p *PowerShell) Text(ctx context.Context, image []byte) (string, error) {
	if p.Exe == "" || p.Script == "" {
		return "", ErrUnavailable
	}
	if _, err := os.Stat(p.Script); err != nil {
		return "", fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	dir := p.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, fmt.Sprintf("dsh-ocr-%d-%d.jpg", os.Getpid(), p.seq.Add(1)))
	if err := os.WriteFile(tmp, image, 0o644); err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, p.Exe, "-NoProfile", "-ExecutionPolicy", "Bypass",
		"-File", p.Script, "-Path", tmp, "-Lang", p.Lang)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("powershell ocr: %w", err)
	}

	// 取最后一行 JSON（脚本可能还会打印别的）
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	var payload struct {
		OK    bool   `json:"ok"`
		Text  string `json:"text"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(last), &payload); err != nil {
		return "", fmt.Errorf("bad ocr output: %s", truncate(last, 120))
	}
	if !payload.OK {
		return "", fmt.Errorf("ocr failed: %s", payload.Error)
	}
	return payload.Text, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
