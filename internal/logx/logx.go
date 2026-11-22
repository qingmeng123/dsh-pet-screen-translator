// Package logx 提供单行、可轮转的文件日志。
//
// 与 JS 版保持同样的落盘约定：工作区 logs/<name>.log，超过上限切分，保留 N 份。
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Logger 是并发安全的文件日志器；零值不可用，用 New 构造。
type Logger struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	f        *os.File
}

// New 打开（或创建）日志文件。目录不存在会自动创建。
func New(path string, maxBytes int64, keep int) (*Logger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Logger{path: path, maxBytes: maxBytes, keep: keep, f: f}, nil
}

// Info 写一条 info 日志。
func (l *Logger) Info(format string, args ...any) { l.write("info", format, args...) }

// Warn 写一条 warn 日志。
func (l *Logger) Warn(format string, args ...any) { l.write("warn", format, args...) }

func (l *Logger) write(level, format string, args ...any) {
	line := fmt.Sprintf("%s [%s] %s\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), level, fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.WriteString(line); err != nil {
		return
	}
	if st, err := l.f.Stat(); err == nil && st.Size() >= l.maxBytes {
		l.rotateLocked()
	}
}

// rotateLocked 调用方必须已持锁：x.log → x.log.1 → … → x.log.keep。
func (l *Logger) rotateLocked() {
	_ = l.f.Close()
	for i := l.keep; i >= 1; i-- {
		from := l.path
		if i > 1 {
			from = fmt.Sprintf("%s.%d", l.path, i-1)
		}
		_ = os.Rename(from, fmt.Sprintf("%s.%d", l.path, i))
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err == nil {
		l.f = f
	}
}

// Close 关闭文件句柄。
func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
