// translatord 是 dsh-pet 屏幕翻译的 Go 守护进程。
//
// 用法：
//
//	translatord version
//	translatord capture --out D:\deepseek\projects\shots\t.jpg [--window-class YYGameMakerYY]
//	translatord run --dsh-port 55503 --workspace D:\deepseek\projects
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"dsh/local/screentranslator/internal/capture"
	"dsh/local/screentranslator/internal/config"
	"dsh/local/screentranslator/internal/engine"
	"dsh/local/screentranslator/internal/logx"
	"dsh/local/screentranslator/internal/ocr"
	"dsh/local/screentranslator/internal/pet"
	"dsh/local/screentranslator/internal/translate"
)

// version 由构建时通过 -ldflags 注入，默认 dev。
var version = "dev"

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "version":
		fmt.Printf("translatord %s\n", version)
	case "capture":
		os.Exit(cmdCapture(args[1:]))
	case "run":
		os.Exit(cmdRun(args[1:]))
	case "ocr":
		os.Exit(cmdOCR(args[1:]))
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: translatord <version|capture|run> [flags]")
}

// cmdCapture 抓一帧并存成 JPEG，用于自测抓屏链路。
func cmdCapture(args []string) int {
	fs := flag.NewFlagSet("capture", flag.ExitOnError)
	out := fs.String("out", "capture.jpg", "输出 JPEG 路径")
	quality := fs.Int("quality", 85, "JPEG 质量 1-100")
	class := fs.String("window-class", "", "窗口类名片段（优先）")
	title := fs.String("window-title", "", "窗口标题片段")
	allowBg := fs.Bool("allow-background", false, "目标窗口不在前台时也抓")
	_ = fs.Parse(args)

	capture.Init()

	var rect capture.RECT
	kind := "full"
	var hwnd uintptr
	if *class != "" || *title != "" {
		h, err := capture.FindWindow(*class, *title)
		if err != nil {
			fmt.Fprintf(os.Stderr, "window not found (class=%q title=%q)\n", *class, *title)
			return 1
		}
		hwnd = h
		r, err := capture.ClientRect(h)
		if err != nil {
			fmt.Fprintf(os.Stderr, "client rect failed: %v\n", err)
			return 1
		}
		if !*allowBg && capture.Foreground() != hwnd {
			fmt.Fprintln(os.Stderr, "target window is not foreground (use --allow-background to override)")
			return 1
		}
		rect, kind = r, "window"
	} else {
		rect = capture.ScreenRect()
	}

	t0 := time.Now()
	img, err := capture.Grab(rect)
	if err != nil {
		fmt.Fprintf(os.Stderr, "grab failed: %v\n", err)
		return 1
	}
	jpegBytes, err := capture.EncodeJPEG(img, *quality)
	if err != nil {
		fmt.Fprintf(os.Stderr, "encode failed: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir failed: %v\n", err)
		return 1
	}
	if err := os.WriteFile(*out, jpegBytes, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write failed: %v\n", err)
		return 1
	}
	printJSON(map[string]any{
		"ok":     true,
		"source": kind,
		"w":      img.Bounds().Dx(),
		"h":      img.Bounds().Dy(),
		"bytes":  len(jpegBytes),
		"ms":     time.Since(t0).Milliseconds(),
		"path":   *out,
	})
	return 0
}

// cmdRun 是守护进程主循环。
func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	workspace := fs.String("workspace", `D:\deepseek\projects`, "工作区根目录")
	cfgPath := fs.String("config", "", "config.json 路径（默认取工作区里的）")
	dshPort := fs.Int("dsh-port", 0, "DSH Web 端口（必填）")
	petID := fs.String("pet", "main", "目标宠物 id")
	_ = fs.Parse(args)
	if *dshPort <= 0 {
		fmt.Fprintln(os.Stderr, "--dsh-port is required")
		return 2
	}

	path := *cfgPath
	if path == "" {
		path = config.ProjectConfigPath(*workspace)
	}
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		return 1
	}
	cfg.DSHPort = *dshPort

	home, _ := os.UserHomeDir()
	key, err := loadAPIKey(home)
	if err != nil && cfg.TranslateMode == "vision" {
		fmt.Fprintf(os.Stderr, "vision 模式需要 API key: %v\n", err)
		return 1
	}

	logger, err := logx.New(filepath.Join(*workspace, "logs", "translatord.log"), int64(cfg.LogMaxBytes), cfg.LogKeep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open log: %v\n", err)
		return 1
	}
	defer logger.Close()

	projDir := filepath.Join(*workspace, "dsh-pet-screen-translator")
	tmpDir := filepath.Join(projDir, ".tmp")
	ocrExe := `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	ocrTimeout := time.Duration(cfg.OCRTimeoutMs) * time.Millisecond

	// OCR：优先常驻进程（只启一次 PowerShell + WinRT），失败则退回一次性调用。
	var ocrProvider engine.OCR
	var ocrWorker *ocr.Worker
	if cfg.OCRWorker {
		w, err := ocr.StartWorker(ocrExe, filepath.Join(projDir, "ocr-server.ps1"), cfg.Lang, tmpDir, ocrTimeout)
		if err != nil {
			logger.Warn("常驻 OCR 启动失败，退回一次性调用：%v", err)
		} else {
			ocrWorker = w
			ocrProvider = w
			logger.Info("常驻 OCR 已就绪（单次识别 ~10ms，不再是 ~440ms）")
		}
	}
	if ocrProvider == nil {
		ocrProvider = &ocr.PowerShell{
			Exe:     ocrExe,
			Script:  filepath.Join(projDir, "ocr-file.ps1"),
			Lang:    cfg.Lang,
			Timeout: ocrTimeout,
			TempDir: tmpDir,
		}
	}
	if ocrWorker != nil {
		// 守护进程退出时一并结束 OCR 子进程
		defer ocrWorker.Close()
	}

	tr := translate.New(cfg.VisionAPIBase, key, cfg.Model, cfg.VisionMaxTokens,
		time.Duration(cfg.VisionTimeoutMs)*time.Millisecond)
	tr.ReasoningEffort = cfg.ReasoningEffort

	eng := &engine.Engine{
		Cfg: cfg,
		Cap: &capture.Shooter{
			Class:                  cfg.WindowClass,
			Title:                  cfg.WindowTitle,
			Quality:                85,
			IgnoreForegroundTitles: []string{"dsh-pet"},
		},
		OCR: ocrProvider,
		Tr:  trAdapter{c: tr, cfg: cfg},
		Pet: petAdapter{
			c:        pet.New(cfg.DSHPort, cfg.PetBasePath),
			petID:    *petID,
			duration: time.Duration(cfg.DurationMs) * time.Millisecond,
			speak:    cfg.Speak,
			surface:  cfg.VoiceSurface,
		},
		Log: logger,
	}

	eng.SetRegionMode(cfg.RegionMode)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 控制通道：外壳用它做「优雅关闭」——先释放占用锁再退出。
	// 监听 127.0.0.1 的随机端口，把端口打到 stdout（外壳解析 CONTROL_PORT=…）。
	// 之所以不用信号：Windows 上 child.kill() 是硬终止，Go 收不到，锁会挂到 TTL 过期。
	control := startControlServer(stop, logger, eng)
	defer control.Close()

	// 启动时把关键参数打出来，方便确认"跑的是不是最新配置"
	logger.Info("参数：settle=%dms interval=%dms pixelRatio=%.3f dedupe=%.2f region=[%.2f,%.2f,%.2f,%.2f] stream=%v speak=%v surface=%s reasoning=%s model=%s",
		cfg.SettleMs, cfg.IntervalMs, cfg.PixelChangeRatio, cfg.DedupeSimilarity,
		cfg.FingerprintRegion.X0, cfg.FingerprintRegion.Y0, cfg.FingerprintRegion.X1, cfg.FingerprintRegion.Y1,
		cfg.Stream, cfg.Speak, cfg.VoiceSurface, cfg.ReasoningEffort, cfg.Model)
	logger.Info("translatord 启动：mode=%s windowClass=%s window=%s port=%d control=%d",
		cfg.TranslateMode, cfg.WindowClass, cfg.WindowTitle, cfg.DSHPort, control.Port())
	fmt.Printf("CONTROL_PORT=%d\n", control.Port())

	// 开关提示语：之前 JS 版有"已开启/已关闭"的气泡，Go 版一度丢了，这里补回来。
	if err := eng.Speak(ctx, "屏幕翻译已开启，画面里的英文我会念成中文。"); err != nil {
		logger.Warn("开启提示写入气泡失败：%v", err)
	}

	eng.Run(ctx)

	if err := eng.Speak(context.Background(), "屏幕翻译已关闭。"); err != nil {
		logger.Warn("关闭提示写入气泡失败：%v", err)
	}
	eng.Close(context.Background())
	logger.Info("translatord 退出：占用锁已释放")
	return 0
}

// cmdOCR 用现有图片走一遍 OCR 适配器，自测 PowerShell 通道。
func cmdOCR(args []string) int {
	fs := flag.NewFlagSet("ocr", flag.ExitOnError)
	imagePath := fs.String("image", "", "图片路径（jpg/png）")
	workspace := fs.String("workspace", `D:\deepseek\projects`, "工作区根目录")
	lang := fs.String("lang", "en-GB", "识别语言")
	_ = fs.Parse(args)
	if *imagePath == "" {
		fmt.Fprintln(os.Stderr, "--image is required")
		return 2
	}
	img, err := os.ReadFile(*imagePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read image: %v\n", err)
		return 1
	}
	p := &ocr.PowerShell{
		Exe:     `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		Script:  filepath.Join(*workspace, "dsh-pet-screen-translator", "ocr-file.ps1"),
		Lang:    *lang,
		Timeout: 20 * time.Second,
		TempDir: filepath.Join(*workspace, "dsh-pet-screen-translator", ".tmp"),
	}
	start := time.Now()
	text, err := p.Text(context.Background(), img)
	if err != nil {
		printJSON(map[string]any{"ok": false, "error": err.Error()})
		return 1
	}
	printJSON(map[string]any{"ok": true, "ms": time.Since(start).Milliseconds(), "text": text})
	return 0
}

func printJSON(v any) {
	raw, _ := json.Marshal(v)
	fmt.Println(string(raw))
}
