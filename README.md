# dsh-pet-screen-translator

给 [dsh-pet](https://github.com/PC2005-cloud/dsh-pet) 加一套「画面英文 → 中文气泡」的能力：右键菜单里多一项 **更多 / 屏幕翻译**，点开即开、再点即关。

## 它做什么

1. 每隔 `intervalMs` 截一次屏（默认主屏全屏，可限定区域）；
2. 用 Windows 自带的 OCR 引擎识别英文（离线，不联网、不产生费用）；
3. 画面文字变了才送翻译，避免重复烧 token；
4. 用**你当前对话的模型**（`agentDefaultModel`）翻译成中文；
5. 借桌宠的 `/broadcast` 接口把译文写进气泡，气泡默认显示 12 秒。

开启期间会占用桌宠（`/lock`），其它插件的碎碎念、余额气泡会让路；关闭时自动释放。用户拖一下宠物也会立刻收回占用，这是 dsh-pet 的既定行为——此时本插件会检测到写气泡失败并自动关闭，不会空转。

### 判重规则

OCR 每拍都会抖一两个字符（`THE` / `'THE` / `TRAT`），所以不是逐字比对：

- 先归一化：小写、去标点、压空白，并把常见 OCR 混淆拉回正常字母（`?`→`h`、`0`→`o`、`1`→`l`、`5`→`s`、`rn`→`m`）；
- 再按词集合算 Jaccard 相似度；短文本（< 60 字符）阈值 **0.45**，长文本 **0.8**；
- 与**最近 3 帧**任意一帧超阈值就跳过，不再调模型。

同样的画面只会翻一次，日志里会留下 `跳过（与最近帧相似）：…`。

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
| `lang` | `en-GB` | OCR 语言；本机可用 `en-GB` 与 `zh-Hans-CN`。 |
| `region` | `null` | `null` = 主屏全屏；给 `{x,y,width,height}` 只识别那块区域（推荐：只框游戏画面，避开宠物自己）。 |
| `prefix` | `译：` | 气泡里的译文前缀。 |
| `system` | 见代码 | 翻译提示词。 |

## 已知限制

- 只翻**英文**：字母占比低于 55% 的画面（中文界面、纯图标）会被跳过；
- OCR 来自 Windows，像素风小字、花体、低对比度背景识别率会下降；把 `region` 框到对话/字幕区能明显改善；
- 全屏独占（exclusive fullscreen）游戏里桌宠气泡可能看不见，把游戏改成「窗口化 / 无边框窗口」即可；
- 每轮翻译都走一次模型调用，`intervalMs` 别设太小，否则既费 token 又跟不上。

## 卸载

```sh
dsh plugin --profile desktop remove dsh-pet-screen-translator
```
