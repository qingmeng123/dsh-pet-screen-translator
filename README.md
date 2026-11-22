# dsh-pet-screen-translator

给 [dsh-pet](https://github.com/PC2005-cloud/dsh-pet) 加一套「画面英文 → 中文气泡」的能力：右键菜单里多一项 **更多 / 屏幕翻译**，点开即开、再点即关。

## 它做什么

1. 每隔 `intervalMs` 截一次屏（默认主屏全屏，可限定区域）；
2. 用 Windows 自带的 OCR 引擎识别英文（离线，不联网、不产生费用）；
3. 画面文字变了才送翻译，避免重复烧 token；
4. 用**你当前对话的模型**（`agentDefaultModel`）翻译成中文；
5. 借桌宠的 `/broadcast` 接口把译文写进气泡，气泡默认显示 12 秒。

开启期间会占用桌宠（`/lock`），其它插件的碎碎念、余额气泡会让路；关闭时自动释放。用户拖一下宠物也会立刻收回占用，这是 dsh-pet 的既定行为——此时本插件会检测到写气泡失败并自动关闭，不会空转。

### 稳定判定（避免翻半句）

游戏文案是**打字机式连续跳出**的，逐帧翻译会翻到半句（`PLEASE A GIFT. t I KINDNESS…`）。所以每一帧先过一道稳定判定：

- 与上一帧的规范化文本**相似度 ≥ 0.8** 视为“同一段、还在变”，把下一拍缩短到 `settleMs`（默认 300 ms）再看一次——它是**周期**：本拍抓屏+OCR 花掉的时间会从等待里扣掉；
- 只有**连续稳定满 `settleMs`**、且这段文案还没翻过，才真正触发；
- 稳定判定只用**文本**，不用像素：光标闪烁、精灵动画会让像素每帧都变，拿哈希判稳会永远等不到。

实测：一段 82 字符的对话在打字过程中不触发，打完约 2 s 后一次性翻出
`你对你的造物有何感受？（它不会听见。）爱 希望 厌恶 恐惧`。

同一段被 OCR 抖动读成两个版本时，两次译文开头 10 字相同就不重复弹泡（省气泡刷屏）。

### 判重规则

**文本模式**（`translateMode: "ocr"`）：OCR 每拍都会抖一两个字符（`THE` / `'THE` / `TRAT`），所以不逐字比对：

- 先归一化：小写、去标点、压空白，并把常见 OCR 混淆拉回正常字母（`?`→`h`、`0`→`o`、`1`→`l`、`5`→`s`、`rn`→`m`）；
- 再按词集合算 Jaccard 相似度；短文本（< 60 字符）阈值 **0.45**，长文本 **0.8**；
- 与**最近 3 帧**任意一帧超阈值就跳过，不再调模型。

**视觉模式**（`translateMode: "vision"`）改用两条更准的判据：

- 规范化文本**完全相同** → 跳过（画面没变）；
- 或整帧 JPEG 的 **sha256 相同** → 跳过（OCR 抖动但像素没变）；
- 否则才发图。理由：只换一个词（`TORSO`→`LEGS`）时文本相似度高达 0.7，用阈值判重会漏掉真变化。

### 两种引擎

`config.json` 的 `engine` 决定用哪套实现：

| `engine` | 实现 | 说明 |
| --- | --- | --- |
| `go`（推荐） | `bin/translatord.exe` 常驻进程 | 抓屏、判稳、翻译、气泡全在 Go；JS 外壳只负责起停 |
| `js` | 内置 `index.js` | 早期实现，保留作回退；抓屏走 PowerShell |

### Go 引擎

```powershell
# 构建（工具链在 .tools\go，不改 PATH）
$env:GOROOT='D:\deepseek\projects\.tools\go'
$env:GOCACHE='D:\deepseek\projects\.tools\gocache'
& "$env:GOROOT\bin\go.exe" build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o bin/translatord.exe ./cmd/translatord

# 自测
.\bin\translatord.exe version
.\bin\translatord.exe capture --window-class YYGameMakerYY --out shots\t.jpg
.\bin\translatord.exe ocr --image shots\t.jpg
.\bin\translatord.exe run --dsh-port 55503 --workspace D:\deepseek\projects

# 单测
& "$env:GOROOT\bin\go.exe" test ./... -count=1
```

菜单点击由 JS 外壳转成进程起停：

| 操作 | 行为 |
| --- | --- |
| 点一次「屏幕翻译」（开） | `spawn translatord.exe run`，守护进程接管抓屏/翻译/气泡 |
| 再点一次（关） | 外壳 `POST /shutdown` → 守护进程**先释放占用锁**再退出（3 s 内没退出才硬杀） |
| 卸载插件 | 同上，走优雅关闭，不会把宠物锁死后等 TTL |

守护进程启动时会把控制端口打到 stdout（`CONTROL_PORT=…`），外壳据此做优雅关闭。
不用信号的原因：Windows 下 `child.kill()` 是硬终止，Go 的 `os.Interrupt` 收不到，锁会挂到 TTL 过期。

**菜单项自带状态**（点一次开、再点一次关，文字随状态变）：

| 菜单文字 | 含义 |
| --- | --- |
| `屏幕翻译：已关闭` | 进程未运行 |
| `屏幕翻译：启动中` | 已 spawn，等控制端口 |
| `屏幕翻译：运行中` | 进程就绪，等画面变化 |
| `屏幕翻译：识别中` | 文案正在变，等稳定 2 秒 |
| `屏幕翻译：翻译中` | 正在调模型 |
| `屏幕翻译：出错` | 抓屏/OCR/翻译失败 |
| `屏幕翻译：已退出` | 进程异常退出 |

状态来自守护进程的 `GET /status`（外壳每 1.5 s 拉一次），点菜单时按**前缀匹配**，所以文字变了也点得动。

**静态画面优化**：每拍先算 JPEG 的 sha256，与上一帧相同就直接复用上次 OCR 文本，
省掉一次 PowerShell 调用（约 300 ms）。菜单/对话停住不动时，一拍只剩抓屏的 30～45 ms。

**实测**：抓屏 32～46 ms（PowerShell 版 ~490 ms）；OCR 通道 ~310 ms（含 PowerShell 启动，静态画面跳过）；二进制约 6.7 MB。

**日志**：Go 自己的在 `logs\translatord.log`；外壳的在 `logs\dsh-pet-screen-translator-shell.log`。

### 旧 JS 引擎（legacy）

`ocr.ps1` + `index.js` 里的完整实现，`engine: "js"` 时生效。抓屏与 OCR 都在 PowerShell 里完成，
每拍起一个 `powershell.exe`（~490 ms、89 MB 内存起落）。保留原因：Go 引擎出问题时能一键切回。

### 两种翻译通道

| `translateMode` | 输入 | 每帧 prompt tokens（1280×960） | 延迟 | 适用 |
| --- | --- | --- | --- | --- |
| `ocr`（默认） | OCR 文本 | ~136 | 0.8～1.4 s | 普通对话，省钱 |
| `vision` | 窗口截图（JPEG） | ~836 | 1.4～2.6 s | 多选项菜单、像素字体、OCR 易错处 |

视觉模式实测（DELTARUNE 礼物界面）：OCR 把 `MIND` 认成 `MINO`、`BRAVERY`/`VOICE` 漏行；视觉一次读全，
译文「请送给它一份礼物。善良 理智 野心 勇气 声音」。

视觉通道直连 `https://api.deepseek.com`，key 取 DSH 凭据（`DEEPSEEK_API_KEY`），**失败会自动回退 OCR 文本**，
并在日志里记 `视觉翻译失败（…），回退 OCR 文本`。每帧 usage 写入日志：`视觉翻译 prompt=836 completion=221`。

### 抓屏与翻译解耦

抓屏 → OCR 是一条链，翻译在后台跑，**互不阻塞**：

- 慢翻译不会让画面停止识别，新帧只排队等当前翻译结束，更旧的帧直接丢弃；
- 单次翻译超时 `translateTimeoutMs`（默认 15 s），超时判失败并记 `warn`；
- 翻译时对支持 `reasoning effort` 的模型会显式传 `off` —— 实测同一段 193 字符的画面，深度思考关掉后从 **12 秒降到 1 秒**；
- 关闭开关时用 `AbortController` 掐断在途请求，关闭后不会再往气泡里写字。

### 噪声过滤

单标题帧（`DELTARUNE`、`CONTACT ARE`）不值得翻译。现在要求同时满足：

- 长度 ≥ `minChars`（默认 12）；
- 至少 2 个长度 ≥ 2 的英文单词；
- 字母数 ≥ 10 且字母占比 ≥ 55%。

## 语言与运行时

| 部分 | 语言 |
| --- | --- |
| 插件逻辑 `index.js` | **JavaScript**（ESM，Node.js 24，无编译步骤，类型用 JSDoc） |
| 抓屏 + OCR `ocr.ps1` | **PowerShell 5.1**（.NET `System.Drawing` + WinRT `Windows.Media.Ocr`） |

本机没有装 `pwsh`（PowerShell 7），所以脚本按 5.1 语法写，调用时用绝对路径。


## 日志与配置在哪

| 内容 | 路径 |
| --- | --- |
| 运行日志 | `D:\deepseek\projects\logs\dsh-pet-screen-translator.log`（超过 1 MB 轮转，保留 `.1`–`.3` 三份） |
| 用户配置 | `D:\deepseek\projects\dsh-pet-screen-translator\config.json` |
| 源码 | `D:\deepseek\projects\dsh-pet-screen-translator\` |

**注意运行时会读工作区里的绝对路径**，而不是 profile 里的那份副本：

DSH 用 `file:` 依赖把插件**复制**进 `~/.dsh/profiles/desktop/node_modules/dsh-pet-screen-translator`，
运行时 `import.meta.url` 指向副本，顺着它推不出工作区。所以 `index.js` 里把工作区根目录写成常量
（可用环境变量 `DSH_PET_ST_WORKSPACE` 覆盖）；换工作区位置时要改这里。

## 安装

```sh
# 在 DSH 桌面应用自带的 CLI 环境里
dsh plugin --profile desktop add file:D:/deepseek/projects/dsh-pet-screen-translator
```

装完**重启 DSH**。启动后右键桌宠 → 「更多」→「屏幕翻译」。

## 配置（可选）

在插件目录放一份 `config.json`，只写要改的键，改完重启 DSH：

```json
{
  "intervalMs": 4000,
  "duration": 12000,
  "minChars": 12,
  "maxChars": 600,
  "translateTimeoutMs": 15000,
  "lang": "en-GB",
  "prefix": "译：",
  "region": { "x": 0, "y": 0, "width": 1280, "height": 720 }
}
```

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `intervalMs` | 4000 | 截屏周期（ms）；翻译不占用它。 |
| `duration` | 12000 | 气泡显示时长（ms）。 |
| `minChars` | 12 | 少于这么多字符视为“没字”。 |
| `maxChars` | 600 | 送进模型的字符上限，超出截断（多半是 OCR 噪声）。 |
| `translateTimeoutMs` | 15000 | 单次翻译超时；超时放弃该帧并记 `warn`。 |
| `translateMode` | `ocr` | `ocr` = 用 OCR 文本翻译；`vision` = 把窗口截图发模型翻译。 |
| `visionMaxTokens` | 300 | 视觉通道输出上限，防止模型跑题把 completion 撑大。 |
| `lang` | `en-GB` | OCR 语言；本机可用 `en-GB` 与 `zh-Hans-CN`。 |
| `window` | `null` | 按窗口标题（子串、不区分大小写）抓该窗口**客户区**。标题会随游戏场景变化，建议只作兜底。 |
| `windowClass` | `null` | 按窗口**类名**抓客户区（如 `YYGameMakerYY`）。类名比标题稳定，优先用它。 |
| `region` | `null` | 显式矩形 `{x,y,width,height}`，优先级最高。 |
| `prefix` | `译：` | 气泡里的译文前缀。 |
| `system` | 见代码 | 翻译提示词。 |
| `speak` | `true` | 完整译文出来后是否念出来；流式半截永远不念。 |
| `voiceSurface` | `desktop` | 念给哪一端：`desktop` / `web`，必须与宠物 `display` 一致。 |
| `breeze.voiceId` | `voc_puvq4zh92bvs` | Breeze 音色 id（`voc_` 开头）。 |
| `breeze.voiceName` | `灵动顽皮幼女` | 语音下拉里的显示名。 |

**抓取范围优先级**：`region` > `windowClass` > `window` > 整屏。找不到窗口时回退整屏，并在日志里留一条
`窗口「X」没找到（window-not-found），已改用整屏`，同一原因只报一次，不刷屏。

**前台闸门**：`CopyFromScreen` 复制的是「屏幕上那块矩形」，不是窗口自己的像素——目标窗口被别的窗口
盖住时，抓到的是压在上面的内容。所以按窗口抓时，若该窗口不在前台，本拍直接跳过（日志
`跳过：目标窗口不在前台`），避免把 DSH / 编辑器的界面当成游戏文本翻出去。

> 配置文件请存成 **UTF-8 无 BOM**：PowerShell 的 `Set-Content -Encoding UTF8` 会写 BOM，
> `JSON.parse` 遇到 BOM 会直接抛错、整份配置退回默认值（这个坑真实踩过）。代码里已做去 BOM 兜底。

## 语音：Breeze 只念终稿

- 翻译流式时只更新气泡，**不出声**；等完整译文返回后，才让桌宠用 BreezeBlue 音色念一次。
- 音色由翻译插件注册进 dsh-pet：`breeze/voc_puvq4zh92bvs`（Breeze 音色“灵动顽皮幼女”，标签“大肥鱼”）。
- Key 放 `breeze-key.txt`（已 gitignore）或环境变量 `BREEZE_API_KEY`；`config.json` 的 `breeze.apiKey` 也可，但不建议提交。
- 桌宠配置里 `voiceName` 指向该音色；`voiceEnabled` 保持 `false` 时只有翻译终稿会念，桌宠自己的聊天/碎碎念不花钱。想让聊天也念再开 `voiceEnabled`。
- `speak` 关掉就只弹气泡；`voiceSurface` 必须与宠物 `display` 一致（当前 `desktop`）。
- 每次合成都消耗 Breeze credits（汉字 2 units/字，10 units = 1 credit）；同一句有 dsh-pet 缓存，重复不重复扣。

## 已知限制

- 只翻**英文**：字母占比低于 55% 的画面（中文界面、纯图标）会被跳过；
- OCR 来自 Windows，像素风小字、花体、低对比度背景识别率会下降；用 `windowClass` 或 `region` 缩小范围能明显改善；
- `windowClass`/`window` 抓的是**屏幕矩形**：目标窗口不在前台时本拍跳过（见上文「前台闸门」）；
- 全屏独占（exclusive fullscreen）游戏里桌宠气泡可能看不见，把游戏改成「窗口化 / 无边框窗口」即可；
- 每轮翻译都走一次模型调用，`intervalMs` 别设太小，否则既费 token 又跟不上。

## 卸载

```sh
dsh plugin --profile desktop remove dsh-pet-screen-translator
```

> **前台判定**：目标窗口不在前台时跳过本拍，避免抓到遮挡内容。但**桌宠自己的浮窗**（标题含 `dsh-pet`）
> 不算遮挡 —— 它弹气泡时会抢前台，若当成遮挡会永久跳过（这个坑真踩过）。

> **遮挡检测**：目标窗口被别的窗口盖住时，GDI 抓到的是上层窗口的像素，所以会跳过本拍，
> 菜单显示「屏幕翻译：画面被遮挡」。选择方式是客户区 3×3 采样 + `WindowFromPoint`，
> 命中率 < 30% 判为遮挡。被遮挡时无法翻译（`PrintWindow` 抓 GameMaker/DirectX 窗口只会得到黑图）。

> **开关反馈**：点「开」→ 气泡说「屏幕翻译已开启，画面里的英文我会念成中文。」；点「关」→「屏幕翻译已关闭。」。
> 开启即取占用锁（翻译期间桌宠静止），关闭时释放。

### 延迟实测

| 阶段 | 耗时 |
| --- | --- |
| 抓屏 + JPEG | 36 ms |
| OCR（常驻进程） | **23～74 ms**（首帧 74 ms 预热，之后 ~24 ms） |
| 视觉翻译（`reasoningEffort: "none"`） | **0.6～0.9 s** |
| 判稳窗口 | 100 ms（`settleMs`，周期语义） |

从文案打完到气泡出译文约 **1.3 秒**。若把 `reasoningEffort` 置空（服务端默认），
实测波动极大（1.3～8.6 s，一次推理吃掉 2000 tokens），不建议。

> **判稳用像素指纹，不用 OCR**：每帧缩成 64×48 灰度指纹，变化像素占比 < `pixelChangeRatio`（默认 0.5%）
> 就算"画面稳定"，此时才做一次 OCR 判断是不是英文。判稳阶段每拍只花抓屏 36 ms + 指纹 <1 ms，
> 不再每秒起 2～3 个 PowerShell。

### 流式输出

翻译请求走 SSE 流式（`stream: true`），气泡**按 120 ms 节流增量刷新**，首字约 0.47 s 出现，全文约 0.68 s 完成。
流式失败会自动回退非流式；开关是配置里的 `stream`（默认 `true`）。

### 常驻 OCR 进程

翻译开启时随守护进程一起启动一个常驻 PowerShell（`ocr-server.ps1`），只初始化一次 WinRT OCR 引擎；
之后每次识别只是「写一行图片路径 → 读一行 JSON」。关闭翻译时守护进程退出、OCR 子进程的 stdin 断开，
它自己读完最后一行就结束 —— 不会留孤儿进程。

| 方式 | 单次 OCR |
| --- | --- |
| 一次性调用 `ocr-file.ps1` | 300～440 ms（每次都要启动 PowerShell + 初始化） |
| **常驻 `ocr-server.ps1`** | **23～74 ms** |

配置开关：`"ocrWorker": true`（默认）；置 false 退回一次性调用。

### 气泡续期（没有新内容时 +10s）

默认气泡显示 12 秒（`duration`）。如果 12 秒内没有新的译文出现，守护进程会把**同一条气泡再播一次**，
时长变成 `duration + idleExtendMs`（默认 12s + 10s = **22s**），于是你在读长台词时气泡不会自己消失。
配置：`"idleExtendMs": 10000`（设 0 关闭）。
