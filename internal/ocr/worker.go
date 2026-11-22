package ocr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Worker 是常驻的 OCR 子进程：PowerShell 只启动一次、WinRT 引擎只初始化一次，
// 之后每次识别只是「写一行路径 → 读一行 JSON」。
//
// 为什么值得：一次性调用要 300～440 ms，其中约 250 ms 是 powershell.exe 启动、
// 约 50 ms 是 WinRT 初始化。常驻之后首次 60 ms、后续 4～10 ms。
//
// 进程随守护进程生灭：关闭时发 {"quit":true} 并关 stdin；即使守护进程被强杀，
// 子进程的 stdin 也会断，ReadLine 返回 null 后自行退出，不留孤儿。
type Worker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	tmpDir string
	seq    atomic.Int64
	mu     sync.Mutex
	closed bool
}

type workerRequest struct {
	Path string `json:"path"`
}

type workerResponse struct {
	Ready bool   `json:"ready"`
	OK    bool   `json:"ok"`
	Text  string `json:"text"`
	Error string `json:"error"`
	Ms    int    `json:"ms"`
}

// StartWorker 启动常驻 OCR 进程，并等待它回报 ready。
func StartWorker(exe, script, lang, tmpDir string, timeout time.Duration) (*Worker, error) {
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	cmd := exec.Command(exe, "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Lang", lang)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ocr worker: %w", err)
	}

	w := &Worker{cmd: cmd, stdin: stdin, reader: bufio.NewReaderSize(stdout, 1<<20), tmpDir: tmpDir}

	// 等 ready 行，避免第一次识别撞到初始化竞态
	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := w.reader.ReadString('\n')
		ch <- readResult{line, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("ocr worker ready: %w", r.err)
		}
		var resp workerResponse
		if err := json.Unmarshal([]byte(r.line), &resp); err != nil || !resp.Ready {
			_ = cmd.Process.Kill()
			return nil, fmt.Errorf("ocr worker not ready: %q", truncate(r.line, 120))
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("ocr worker ready timeout after %s", timeout)
	}
	return w, nil
}

// Text 识别一张 JPEG/PNG 的字节内容，返回文本。
func (w *Worker) Text(ctx context.Context, image []byte) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return "", ErrUnavailable
	}

	dir := w.tmpDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("dsh-ocr-w%d-%d.jpg", os.Getpid(), w.seq.Add(1)))
	if err := os.WriteFile(path, image, 0o644); err != nil {
		return "", err
	}
	defer os.Remove(path)

	req, _ := json.Marshal(workerRequest{Path: path})
	if _, err := w.stdin.Write(append(req, '\n')); err != nil {
		w.closed = true
		return "", fmt.Errorf("ocr worker write: %w", err)
	}

	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := w.reader.ReadString('\n')
		ch <- readResult{line, err}
	}()
	select {
	case <-ctx.Done():
		// 子进程还卡在这次识别里，标记失效，让上层回退一次性调用
		w.closed = true
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			w.closed = true
			return "", fmt.Errorf("ocr worker read: %w", r.err)
		}
		var resp workerResponse
		if err := json.Unmarshal([]byte(r.line), &resp); err != nil {
			return "", fmt.Errorf("ocr worker bad json: %q", truncate(r.line, 120))
		}
		if !resp.OK {
			return "", fmt.Errorf("ocr worker: %s", resp.Error)
		}
		return resp.Text, nil
	}
}

// Close 让子进程自行退出；超时再强杀。
func (w *Worker) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	_, _ = w.stdin.Write([]byte("{\"quit\":true}\n"))
	_ = w.stdin.Close()
	w.mu.Unlock()

	done := make(chan struct{})
	go func() {
		_ = w.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = w.cmd.Process.Kill()
	}
}
