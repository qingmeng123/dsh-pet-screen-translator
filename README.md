# dsh-pet 屏幕翻译

给 [dsh-pet](https://github.com/PC2005-cloud/dsh-pet) 加一双眼睛：**游戏里的英文对白，实时翻成中文写进气泡**。

它是 dsh-pet 的一个本地插件，由 **Go 守护进程**抓屏 + 调模型 + 写气泡，JS 只负责起停，因此常驻内存只有 10 MB 出头，
抓一帧 30～45 ms，从画面稳定到气泡出字约 **1 秒**（流式首字 ~0.5 s）。

```
游戏画面 ──抓屏 36ms──▶ 像素判稳 100ms ──▶ OCR 24ms ──▶ 视觉模型 ~0.9s ──▶ 桌宠气泡（流式逐字）
```

---

## 功能特性

| 特性 | 说明 |
| --- | --- |
| **视觉翻译** | 直接把窗口截图交给 `deepseek-flash`，不依赖 OCR 质量；像素字体、多选项菜单也能读 |
| **流式输出** | SSE 逐字回填气泡，首字约 0.5 s |
| **打字机友好** | 用 64×48 像素指纹判稳，文案还在跳时不翻，避免把半句话拿去翻译 |
| **不重复翻** | 三层去重：像素同屏跳过 → OCR 文本相似度 ≥0.9 跳过 → 译文重复后 15 s 抑制 |
| **不瞎翻** | 窗口被遮挡 / 不在前台 / 已最小化时明确跳过，并在菜单里说明原因 |
| **气泡续期** | 12 秒没有新内容，自动把气泡续到 22 秒，长台词不会自己消失 |
| **省资源** | 常驻 OCR 进程（单次 24 ms）、静态画面只算指纹、思考 token 全关 |
| **状态可见** | 菜单里直接显示：运行中 / 识别中 / 翻译中 / 画面被遮挡 / 游戏已最小化 |
| **随开随关** | 点一次起进程，再点一次全停（Go 与 OCR 子进程一起退出，不留孤儿） |

## 系统要求

| 项目 | 要求 |
| --- | --- |
| 系统 | Windows 10 1809+ / Windows 11（依赖 WinRT OCR） |
| 宿主 | DSH 桌面版（带 `dsh-pet` 插件） |
| 运行时 | 无需安装；构建时需要 **Go 1.24+** |
| 其他 | PowerShell 5.1（系统自带）、DeepSeek API Key |

---

## 下载与安装

### 1. 取代码

```powershell
git clone https://github.com/qingmeng123/dsh-pet-screen-translator.git
cd dsh-pet-screen-translator
```

> 想放到别的位置也可以，下文把 `<项目目录>` 换成实际路径。

### 2. 构建 Go 守护进程

安装 [Go 1.24+](https://go.dev/dl/)，然后：

```powershell
# 在 <项目目录> 下
go build -trimpath -ldflags "-s -w" -o bin/translatord.exe ./cmd/translatord

# 自检：应输出版本号
.\bin\translatord.exe version
```

`bin\` 不进版本库（`.gitignore` 里已排除），需要本地构建一次。

### 3. 注册到 DSH

任选一种：

**A. 应用内添加（推荐）**

DSH 桌面版 → 设置 → 插件 → 添加本地插件 → 选择 `<项目目录>`。

**B. 命令行**

```powershell
dsh plugin --profile desktop add file:<项目目录>
```

> `dsh` 是 DSH 自带的 CLI（Windows 上通常在 `D:\harness\resources\runtime\cli\bin\dsh.cmd`）。

### 4. 配置 API Key

程序优先读环境变量，其次读 DSH 凭据文件：

```powershell
# 方式一：环境变量（临时）
$env:DEEPSEEK_API_KEY = "sk-..."

# 方式二：DSH 凭据文件（推荐，DSH 里已登录就直接可用）
# %USERPROFILE%\.dsh\.credentials.yaml
#   DEEPSEEK_API_KEY: sk-...
```

### 5. 指定工作区（可选）

外壳默认认为工作区是 `D:\deepseek\projects`。**如果你的项目不在这里**，二选一：

```powershell
# 推荐：设环境变量
$env:DSH_PET_ST_WORKSPACE = "E:\games\dsh-workspace"
```

或修改 `go-engine.js` 顶部的 `WORKSPACE` 常量。

### 6. 启动

打开游戏窗口 → 右键桌宠 → **更多 → 屏幕翻译 → 开关**。

菜单文字会随状态变化：

| 菜单文字 | 含义 |
| --- | --- |
| `开关：已关闭` | 进程未运行 |
| `开关：启动中` | 正在拉起守护进程 |
| `开关：运行中` | 就绪，等画面变化 |
| `开关：识别中` | 画面还在变，等稳定 100 ms |
| `开关：翻译中` | 正在调模型 |
| `开关：画面被遮挡` | 游戏被别的窗口盖住，切回游戏即可 |
| `开关：游戏已最小化` | 从任务栏恢复游戏即可 |
| `开关：出错` | 抓屏 / OCR / 翻译失败，理由见日志 |

开关与判稳区域都收在同一个子菜单里：

```
更多
├─ 屏幕翻译
│   ├─ 开关：运行中
│   └─ 判稳区域：固定
└─ 吃白饭
```

---

## 配置

配置文件：`config.json`（项目目录下），改完点一次「开关」重启守护进程即生效。

```json
{
  "engine": "go",
  "translateMode": "vision",

  "windowClass": "YYGameMakerYY",
  "window": "DELTARUNE",

  "settleMs": 100,
  "pixelChangeRatio": 0.005,
  "fingerprintRegion": { "x0": 0, "y0": 0.10, "x1": 1, "y1": 1 },

  "intervalMs": 4000,
  "minChars": 12,
  "dedupeSimilarity": 0.9,
  "suppressSeconds": 15,

  "duration": 12000,
  "idleExtendMs": 10000,
  "prefix": "译：",

  "stream": true,
  "reasoningEffort": "none",
  "visionMaxTokens": 0,
  "ocrWorker": true
}
```

### 常用项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `windowClass` / `window` | `YYGameMakerYY` / `DELTARUNE` | 目标窗口，类名优先、支持片段匹配。**换游戏改这里** |
| `settleMs` | 100 | 判稳窗口（周期语义：扣掉本拍耗时后补足） |
| `pixelChangeRatio` | 0.005 | 指纹里多少比例像素变化算「画面变了」；太小会被动画误触发，太大漏翻 |
| `fingerprintRegion` | 下方 90% | 只对这块区域做指纹。对话在顶部就改 `y0/y1` |
| `intervalMs` | 4000 | 画面静止时的轮询间隔 |
| `minChars` | 12 | 少于这么多字符不翻（过滤标题、单字 UI） |
| `dedupeSimilarity` | 0.9 | OCR 文本相似度达到它就不再翻（同一屏防抖） |
| `suppressSeconds` | 15 | 译文重复后，这么多秒内同一屏不再翻 |
| `duration` | 12000 | 气泡基础显示时长（毫秒） |
| `idleExtendMs` | 10000 | 没有新内容时续期的时长，设 0 关闭 |
| `stream` | true | 流式输出 |
| `reasoningEffort` | `none` | 关掉思考 token；设 `low`/`high` 会明显变慢 |
| `ocrWorker` | true | 常驻 OCR 进程；false 退回一次性调用（每次多 ~400 ms） |
| `prefix` | `译：` | 气泡前缀 |

### 对话区域不在下方？

`fingerprintRegion` 用相对比例表示：`x0,y0,x1,y1 ∈ [0,1]`。例如对话框在中间横带：

```json
"fingerprintRegion": { "x0": 0, "y0": 0.35, "x1": 1, "y1": 0.75 }
```

---

## 工作原理

```
┌──────────────────────────── 每 100ms / 4s 一拍 ────────────────────────────┐
│ 1. 抓屏（GDI BitBlt，36 ms）                                               │
│ 2. 遮挡采样：9 点命中率 <88% → 跳过并说明原因                                │
│ 3. 像素判定：64×48 灰度指纹                                                │
│      · 与「已翻帧」几乎相同 → 什么都不做                                    │
│      · 与候选帧差异 ≥0.5%   → 画面在变，重置 100ms 计时                      │
│      · 稳定满 100ms        → 进入 OCR                                      │
│ 4. OCR（常驻进程，24 ms）→ 过滤非英文 / 与上次文本相似                       │
│ 5. 视觉模型（流式，首字 ~0.5s）→ 边收边写气泡                                │
│ 6. 12s 无新内容 → 气泡续到 22s                                             │
└───────────────────────────────────────────────────────────────────────────┘
```

只有第 5 步会请求模型；其余全部本地完成。

### 两种引擎

| `engine` | 实现 | 说明 |
| --- | --- | --- |
| `go`（默认） | `bin/translatord.exe` 常驻进程 | 抓屏/判稳/翻译/气泡都在 Go 里，JS 外壳只管起停 |
| `js` | `index.js` 内置实现 | 早期版本，抓屏走 PowerShell，保留作回退 |

### Go 进程结构

```
cmd/translatord        入口：version / capture / ocr / run 子命令 + 控制通道
internal/capture       BitBlt 抓屏、JPEG、遮挡采样
internal/engine        判稳 / 去重 / 续期状态机（15 个单测，可注入假时钟）
internal/ocr           常驻 OCR 工人（stdin/stdout 行协议）与一次性回退
internal/translate     DeepSeek 客户端（流式 + 非流式、reasoning_effort 兼容）
internal/pet           桌宠 HTTP 客户端（锁 / 气泡 / 菜单）
internal/config        配置默认值与归一化
internal/logx          单行日志 + 轮转
```

---

## 日志与排错

| 文件 | 内容 |
| --- | --- |
| `logs\translatord.log` | 守护进程日志（每行带毫秒时间戳，1 MB 轮转，保留 3 份） |
| `logs\dsh-pet-screen-translator-shell.log` | JS 外壳日志（起停、崩溃、自动重启） |

**常见现象**

| 现象 | 排查 |
| --- | --- |
| 菜单显示「画面被遮挡」 | 游戏被其他窗口盖住，或桌宠自己的浮窗抢了前台（已豁免）；切回游戏即可 |
| 菜单显示「游戏已最小化」 | 从任务栏恢复游戏窗口 |
| 一直「识别中」 | 画面一直有动画（角色/粒子），或 `pixelChangeRatio` 太小；把 `fingerprintRegion` 收窄到对话框 |
| 同一句翻两次 | `dedupeSimilarity` 调低（0.9 → 0.85），或减少 `fingerprintRegion` 里的动画区域 |
| 文案变了却不翻 | `pixelChangeRatio` 太大（单行变化可能只有 1% 左右），0.005 比较稳 |
| 翻译很慢 | 检查 `reasoningEffort` 是否为 `none`、`ocrWorker` 是否为 true |
| 气泡一闪就没了 | 调大 `duration` / `idleExtendMs` |

**查看实时日志**

```powershell
Get-Content .\logs\translatord.log -Tail 20 -Wait
```

---

## 性能实测（1280×960 窗口）

| 阶段 | 耗时 |
| --- | --- |
| 抓屏 + JPEG | 30～45 ms |
| 判稳指纹 | <1 ms |
| OCR（常驻进程） | 23～74 ms（首帧预热 74 ms） |
| 视觉翻译（首字） | ~474 ms |
| 视觉翻译（全文） | ~677 ms |
| **画面稳定 → 气泡出字** | **≈ 1.0 s** |

每帧成本：约 810 prompt tokens（视觉通道）、completion 5～20 tokens（关掉思考后）。

---

## 开发

```powershell
# 单元测试（15 个引擎用例 + 2 个抓屏用例，共 17 个）
go test ./... -count=1

# 静态检查
go vet ./...

# 构建
go build -trimpath -ldflags "-s -w -X main.version=dev" -o bin/translatord.exe ./cmd/translatord

# 自测命令
.\bin\translatord.exe version
.\bin\translatord.exe capture --window-class YYGameMakerYY --out shots\t.jpg
.\bin\translatord.exe ocr --image shots\t.jpg
.\bin\translatord.exe run --dsh-port <DSH 端口> --workspace <工作区>

# 外壳加载冒烟测试（改完 go-engine.js 先跑这个，避免把宿主带崩）
node smoke.mjs
```

## 卸载

```powershell
dsh plugin --profile desktop remove dsh-pet-screen-translator
```

删除项目目录即可；没有写注册表、没有改 PATH、没有常驻服务。

## 已知限制

- 只支持**一个**目标窗口（`windowClass`），不做多窗口同时翻译。
- 游戏被完全遮挡时无法翻译：GDI 只能抓屏幕像素，DirectX 窗口（如 DELTARUNE）连 `PrintWindow` 都只给黑图。
- 需要网络访问 `api.deepseek.com`；国内网络需要代理。
- 只做英译中，OCR 语言在 `ocr-server.ps1` 的 `-Lang` 里可改（默认 `en-GB`）。

## 许可

MIT。`dsh-pet` 的菜单与气泡接口属于 [dsh-pet](https://github.com/PC2005-cloud/dsh-pet) 项目。
