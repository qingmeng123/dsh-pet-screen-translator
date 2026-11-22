/**
 * dsh-pet-screen-translator —— 给 dsh-pet 加一只“会翻译的眼睛”。
 *
 * 形态：一个独立的 DSH 宿主插件，和 dsh-pet 同进程运行，只通过 dsh-pet 的公开 HTTP 接口交互
 * （`/menu/item` 注册菜单项、`/menu/events` 收点击、`/lock` + `/broadcast` 让它说话）。
 * 不 import dsh-pet 的任何内部模块，所以 dsh-pet 升级不会把这里弄坏。
 *
 * 流程：右键菜单「屏幕翻译」→ 取占用锁 → 定时截屏 + Windows OCR → 有新的英文就给
 * 当前对话模型翻译 → 把译文写进桌宠气泡。再点一次菜单关闭。
 *
 * 配置：同目录 config.json，缺省值见 DEFAULTS。改完重启 DSH 生效。
 *
 * @module dsh-pet-screen-translator
 */

import { spawn } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { access, appendFile, mkdir, readFile, realpath, rename, stat, unlink } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { installBreezeVoice } from './breeze-voice.js';

/** 插件行 id（与 cordis.patch.yml 一致）。 */
export const name = 'screen-translator';

/** 需要注入的宿主服务：webServer 取端口，agentDefaultModel + llm 用来翻译，petApi7340 注册 Breeze 音色。 */
export const inject = ['webServer', 'agentDefaultModel', 'llm', 'credentials', 'petApi7340'];

const ROOT = dirname(fileURLToPath(import.meta.url));

/** 菜单项归属：第三方菜单项按 owner 管理，重复注册是覆盖语义。 */
const OWNER = 'screen-translator';
/** 菜单文字：右键菜单「更多 / 屏幕翻译」。 */
const LABEL = '屏幕翻译';
/** 默认操作的宠物 id。 */
const PET = 'main';
/** 占用锁时长（ms）：30 分钟上限内取 15 分钟，够一场长对话，过期也不会把宠物永久锁死。 */
const LOCK_TTL = 15 * 60 * 1000;
/** 续期周期（ms）。 */
const LOCK_RENEW = 5 * 60 * 1000;

/** 缺省配置；config.json 里的同名键覆盖它。 */
const DEFAULTS = {
  /** 截屏 - OCR 的周期（ms）。翻译不再占用这个周期，慢请求不会拖住抓屏。 */
  intervalMs: 4000,
  /** 气泡显示时长（ms）。 */
  duration: 12000,
  /** OCR 后少于这么多字符就当作“没字”，不翻译。 */
  minChars: 12,
  /** 单次送进模型的字符上限（画面文字通常很短，超了多半是 OCR 噪声）。 */
  maxChars: 600,
  /** 单次翻译的超时（ms）：超了就放弃这一帧。翻译已与抓屏解耦，这里放宽一点以免误杀慢响应。 */
  translateTimeoutMs: 15000,
  /** 截屏区域：{x,y,width,height}；null = 不按矩形抓。优先级最高。 */
  region: null,
  /** 按窗口标题（不区分大小写、子串匹配）抓该窗口的客户区，排除标题栏与旁边窗口；null = 不用。 */
  window: null,
  /** 按窗口类名（不区分大小写、子串匹配）抓客户区。类名比标题稳定 —— 游戏切场景常改标题。 */
  windowClass: null,
  /** OCR 语言标识。 */
  lang: 'en-GB',
  /** 送进模型的提示词。 */
  system:
    '你是游戏画面的实时翻译。把用户给出的英文画面文字翻译成自然、简短的中文，只输出译文本身：' +
    '不要解释、不要加引号或括号注释、不要重复原文。遇到菜单项、按钮、提醒用语就按游戏里常见的中文说法翻。' +
    '原文里出现明显缺字或乱码（例如 T?E、TRAT）时按上下文补全成最可能的词再翻，不要照抄残缺字母。',
  /** 翻译通道：ocr = 用 OCR 文本（便宜）；vision = 把窗口截图发模型（准，但贵 6～7 倍 token）。 */
  translateMode: 'ocr',
  /** 视觉通道的接口地址；只支持 DeepSeek 官方接口（同名模型）。 */
  visionApiBase: 'https://api.deepseek.com',
  /** 视觉通道单次输出上限：译文很短，限住它避免模型跑题把 completion 撑到上千 token。 */
  visionMaxTokens: 300,
  /** 文案稳定判定：同一帧连续保持不变这么久（ms）才触发翻译，避开打字机/动画中途。 */
  settleMs: 2000,
  /** 视觉通道的超时（ms）。 */
  visionTimeoutMs: 25000,
  /** 视觉通道的提示词。 */
  visionSystem:
    '你是游戏画面的实时翻译。把画面里的英文翻译成自然、简短的中文，只输出译文本身：' +
    '不要解释、不要加引号或括号注释、不要重复原文。菜单项、按钮、提醒用语按游戏里常见的中文说法翻。',
  /** 展示译文时的前缀。 */
  prefix: '译：',
};

/** 读取配置：优先工作区里的 `dsh-pet-screen-translator/config.json`，其次插件目录内的副本。 */
async function loadConfig() {
  const candidates = [join(PROJECT_DIR, 'config.json'), join(ROOT, 'config.json')];
  for (const candidate of candidates) {
    try {
      // 去掉 BOM：PowerShell 的 Set-Content -Encoding UTF8 会写 BOM，JSON.parse 会直接报错
      const raw = (await readFile(candidate, 'utf8')).replace(/^\uFEFF/, '');
      const user = JSON.parse(raw);
      return { ...DEFAULTS, ...(user && typeof user === 'object' ? user : {}) };
    } catch {
      /* 试下一个 */
    }
  }
  return { ...DEFAULTS };
}

/** 规范化 OCR 文本：只留下有意义的内容，便于比较与翻译。 */
function cleanText(raw) {
  return String(raw ?? '')
    .split(/\r?\n/)
    .map((line) => line.replace(/\s+/g, ' ').trim())
    .filter((line) => line.length >= 2)
    .join(' ')
    .trim();
}

/**
 * 英文字符占比与信息量是否够（避免把中文界面、单标题帧送去翻译）。
 * 要求：字母 ≥ 10、至少 2 个长度 ≥ 2 的单词、字母占比 ≥ 55%。
 */
function looksEnglish(text) {
  const latin = (text.match(/[A-Za-z]/g) ?? []).length;
  const cjk = (text.match(/[\u4e00-\u9fff]/g) ?? []).length;
  const words = (text.match(/[A-Za-z]{2,}/g) ?? []).length;
  if (latin < 10 || words < 2) return false;
  return latin / Math.max(latin + cjk, 1) >= 0.55;
}

/** 词集合的 Jaccard 相似度：用来判断“画面没变”，跳过重复翻译。 */
function similarity(a, b) {
  const sa = new Set(a.toLowerCase().match(/[a-z0-9']+/g) ?? []);
  const sb = new Set(b.toLowerCase().match(/[a-z0-9']+/g) ?? []);
  if (sa.size === 0 || sb.size === 0) return 0;
  let inter = 0;
  for (const w of sa) if (sb.has(w)) inter++;
  return inter / (sa.size + sb.size - inter);
}

/** 是否支持 reasoning effort 的缓存：key = `provider/model`。 */
const reasoningCache = new Map();

/**
 * 当前模型是否声明支持 reasoning effort（含 `off`）。
 *
 * 只有确实声明支持时才传 `reasoningEffort: 'off'`：dsh-llm 对「无 reasoning 元数据 + 显式传 effort」
 * 直接判错，请求发不出去，表现为空流（日志里的「模型未返回文本 blocks=none」）。
 * @param {any} ctx - 宿主上下文。
 * @param {{ provider: string, model: string }} selection - 当前模型。
 * @returns {Promise<boolean>} 支持与否；查询失败一律保守返回 false。
 */
async function supportsReasoningOff(ctx, selection) {
  const key = `${selection.provider}/${selection.model}`;
  if (reasoningCache.has(key)) return reasoningCache.get(key);
  let supported = false;
  try {
    const llm = ctx.llm;
    if (typeof llm?.resolveModelInfo === 'function') {
      const info = await llm.resolveModelInfo(selection.provider, selection.model);
      supported = info?.reasoning?.efforts?.some((e) => e.id === 'off') ?? false;
    }
  } catch {
    supported = false;
  }
  reasoningCache.set(key, supported);
  return supported;
}

/**
 * 用当前对话模型把英文翻成中文。
 * @param {any} ctx - 宿主上下文。
 * @param {string} text - OCR 出来的英文。
 * @param {string} system - 提示词。
 * @param {AbortSignal | undefined} outer - 调用方的中止信号（关闭插件时用它掐断在途请求）。
 * @param {number} timeoutMs - 单次超时，超时按失败处理。
 * @returns {Promise<{ ok: true, text: string } | { ok: false, error: string }>} 结果；失败带原因。
 */
async function translate(ctx, text, system, outer, timeoutMs) {
  const selection = ctx.agentDefaultModel?.currentSelection?.();
  if (!selection?.provider || !selection?.model) return { ok: false, error: '当前对话未配置模型' };
  const llm = ctx.llm;
  if (!llm || typeof llm.stream !== 'function') return { ok: false, error: 'LLM 服务不可用' };

  let assembler;
  let messages = [{ role: 'user', content: [{ type: 'text', text }] }];
  try {
    const mod = await import('@deepseek-ai/dsh-llm');
    if (mod?.BlockAssembler && mod?.createUserMessage) {
      assembler = new mod.BlockAssembler();
      messages = [
        mod.createUserMessage({
          content: [{ type: 'text', text }],
          source: { kind: 'user' },
        }),
      ];
    }
  } catch {
    /* 解析不到 dsh-llm 就用上面的降级拼装 */
  }

  // 超时与外部中止合并：任一触发都算失败。AbortSignal.any 在 Node 20+ 可用，兜底用超时。
  const timeout = AbortSignal.timeout(Number.isFinite(timeoutMs) ? timeoutMs : 15_000);
  const signal = outer && typeof AbortSignal.any === 'function' ? AbortSignal.any([outer, timeout]) : timeout;

  const options = {
    provider: selection.provider,
    model: selection.model,
    sessionId: 'dsh-pet-screen-translator',
    messages,
    system,
    temperature: 0.2,
    // 翻译不需要深度思考：开着只会更慢，还可能只产出推理块、正文为空
    ...((await supportsReasoningOff(ctx, selection)) ? { reasoningEffort: 'off' } : {}),
    signal,
  };

  let out = '';
  try {
    for await (const chunk of llm.stream(options)) {
      if (assembler) {
        assembler.push(chunk);
        continue;
      }
      if (typeof chunk === 'string') out += chunk;
      else if (chunk && typeof chunk.text === 'string') out += chunk.text;
      else if (chunk && typeof chunk.delta === 'string') out += chunk.delta;
    }
  } catch (e) {
    const msg = e instanceof Error ? `${e.name}: ${e.message}` : String(e);
    return { ok: false, error: signal.aborted ? `超时或已中止（${msg}）` : msg };
  }

  if (assembler) {
    out = assembler
      .blocks()
      .filter((b) => b.type === 'text')
      .map((b) => ('text' in b ? b.text : ''))
      .join('');
  }
  out = out.trim();
  if (out === '') {
    // 把块类型和流终止原因带出来：空正文通常是「只有推理块」或适配器把错误折进了 finish
    const blocks = assembler ? assembler.blocks().map((b) => b.type).join(',') || 'none' : 'no-assembler';
    const finish = assembler?.finish;
    const kind = finish && typeof finish === 'object' ? (finish.kind ?? '?') : '?';
    const failure = finish && typeof finish === 'object' && finish.failure ? JSON.stringify(finish.failure).slice(0, 120) : '';
    return { ok: false, error: `模型未返回文本（blocks=${blocks} finish=${kind}${failure ? ' ' + failure : ''}）` };
  }
  return { ok: true, text: out };
}

/**
 * 诊断日志：落在**当前工作区**的 `logs/` 下（插件目录在 `<workspace>/dsh-pet-screen-translator`，
 * 这里取它的上一级），不再写用户目录。超过 1 MB 自动轮转，只留最近 3 份。
 * @param {'info'|'warn'} level - 级别。
 * @param {string} msg - 内容。
 */
async function writeLog(level, msg) {
  try {
    const file = join(LOG_DIR, LOG_NAME);
    await mkdir(LOG_DIR, { recursive: true });
    await appendFile(file, `${new Date().toISOString()} [${level}] ${msg}\n`, 'utf8');
    await rotateIfNeeded(file);
  } catch {
    /* 日志失败不影响主流程 */
  }
}

/**
 * 按大小轮转日志：`x.log` → `x.log.1` → `x.log.2` → `x.log.3`。
 * @param {string} file - 日志文件路径。
 */
async function rotateIfNeeded(file) {
  const info = await stat(file);
  if (info.size < LOG_MAX_BYTES) return;
  for (let i = LOG_KEEP; i >= 1; i--) {
    const from = i === 1 ? file : `${file}.${i - 1}`;
    await rename(from, `${file}.${i}`).catch(() => {});
  }
}

/**
 * 工作区根目录。
 *
 * 为什么写死：DSH 以 `file:` 依赖把插件**复制**进
 * `~/.dsh/profiles/desktop/node_modules/dsh-pet-screen-translator`，运行时 `import.meta.url`
 * 指向的是那份副本，顺着它推不出工作区。所以日志与配置一律按这个绝对路径走，
 * 需要换位置时改这里，或设环境变量 `DSH_PET_ST_WORKSPACE`。
 */
const WORKSPACE_ROOT = process.env.DSH_PET_ST_WORKSPACE ?? 'D:\\deepseek\\projects';
/** 工作区内本项目的目录（源码、config.json 所在）。 */
const PROJECT_DIR = join(WORKSPACE_ROOT, 'dsh-pet-screen-translator');
/** 日志目录：工作区 `logs/`。 */
const LOG_DIR = join(WORKSPACE_ROOT, 'logs');
/** 日志文件名。 */
const LOG_NAME = 'dsh-pet-screen-translator.log';
/** 单个日志文件上限（1 MB）。 */
const LOG_MAX_BYTES = 1024 * 1024;
/** 轮转保留份数。 */
const LOG_KEEP = 3;

/**
 * 规范化文本：小写、去标点、压空白，并把 OCR 常见混淆字符拉回正常字母。
 * 只用于判重，不用于翻译原文。
 *
 * 游戏字体最容易认错的是：`H`→`?`、`0`↔`o`、`1`↔`l`、`5`→`s`、`rn`→`m`。
 * 不做这一步时，同一句话的两次识别会差出半个词表，判重直接失效。
 */
function normalize(text) {
  return String(text ?? '')
    .toLowerCase()
    .replace(/\?/g, 'h')
    .replace(/rn/g, 'm')
    .replace(/[0]/g, 'o')
    .replace(/[1]/g, 'l')
    .replace(/[5]/g, 's')
    .replace(/[^a-z0-9\u4e00-\u9fff]+/g, ' ')
    .replace(/\s+/g, ' ')
    .trim();
}

/**
 * 同步读 config.json 的 engine 字段，决定走 Go 守护进程还是内置 JS 实现。
 * 同步读是为了在 apply() 里立刻分支（cordis 的 apply 要求同步完成装配）。
 * @returns {'go' | 'js'}
 */
function readEngine() {
  try {
    const raw = readFileSync(join(PROJECT_DIR, 'config.json'), 'utf8').replace(/^\uFEFF/, '');
    const cfg = JSON.parse(raw);
    return cfg.engine === 'go' ? 'go' : 'js';
  } catch {
    return 'js';
  }
}

/**
 * 插件主体。
 * @param {any} ctx - DSH 宿主上下文。
 */
export function apply(ctx) {
  // Breeze 音色：与引擎无关，先挂上；翻译引擎只在完整译文出来后带 speak 广播。
  installBreezeVoice(ctx);
  // 引擎选择：config.json 的 engine = "go" 时交给 go-engine.js（只负责起停 translatord.exe）；
  // 其余情况走下面这套 JS 实现（legacy，保留作回退）。
  if (readEngine() === 'go') {
    void import('./go-engine.js').then((m) => m.apply(ctx)).catch((e) => {
      ctx.logger?.warn?.(`[screen-translator] go 引擎加载失败，回退 js：${e instanceof Error ? e.message : String(e)}`);
    });
    return;
  }

  /** 日志一律压成单行：译文里带换行会把「一行一条」的日志结构冲垮。 */
  const flat = (msg) => String(msg).replace(/\s+/g, ' ').trim();
  const log = (msg) => {
    ctx.logger?.info?.(`[screen-translator] ${flat(msg)}`);
    void writeLog('info', flat(msg));
  };
  const warn = (msg) => {
    ctx.logger?.warn?.(`[screen-translator] ${flat(msg)}`);
    void writeLog('warn', flat(msg));
  };

  /** 状态：全是内存态，DSH 重启即清零（与占用锁同生命周期）。 */
  const state = {
    on: false,
    lockId: null,
    /** 最近 3 帧的归一化文本：OCR 抖动（'THE / THE / TRAT）不会骗过判重。 */
    recent: [],
    lastTranslated: '',
    timer: null,
    renewTimer: null,
    polling: null,
    alive: true,
    /** 待翻译的文本（只保留最新一帧，翻译不阻塞抓屏）。 */
    pendingJob: null,
    /** 是否有翻译在途。 */
    translating: false,
    /** 在途翻译的中止器：关闭时用它掐掉请求。 */
    abort: null,
    /** 上一次的抓取范围（source WxH），变化时才记日志。 */
    lastSource: '',
    /** 上一次的回退原因，避免每拍重复告警。 */
    lastFallback: '',
    /** 上一次的跳过原因（目标窗口不在前台等），同样只记一次。 */
    lastSkip: '',
    /** 稳定判定：候选帧的规范化文本与首次出现时间。 */
    candidateNorm: '',
    candidateSince: 0,
    /** 已经翻过的那段文本，避免同一段反复触发。 */
    translatedNorm: '',
    /** 是否处于「文案还在变」的状态，只为日志去重。 */
    unstable: false,
    config: { ...DEFAULTS },
  };

  /** dsh-pet 的基址：同一台机器、同一个 DSH Web 端口。 */
  const base = () => `http://127.0.0.1:${ctx.webServer?.port ?? 0}/dsh-pet-7340`;

  /** 发一个 JSON 请求，失败返回 null（不抛，循环要活得久）。 */
  async function call(method, path, body) {
    try {
      const res = await fetch(base() + path, {
        method,
        headers: body === undefined ? undefined : { 'content-type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(15_000),
      });
      const text = await res.text();
      try {
        return JSON.parse(text);
      } catch {
        return { ok: res.ok, raw: text };
      }
    } catch {
      return null;
    }
  }

  /** 往气泡里写一句话（需锁；锁丢了就重新取一次再试）。 */
  async function say(text) {
    if (!state.lockId) return false;
    let r = await call(
      'POST',
      `/broadcast?pet=${PET}&surface=any&lock=${encodeURIComponent(state.lockId)}`,
      { text, duration: state.config.duration, anim: false },
    );
    if (r && r.ok === false && (r.reason === 'lock-lost' || r.reason === 'lock-required')) {
      state.lockId = null;
      if (await acquireLock()) {
        r = await call(
          'POST',
          `/broadcast?pet=${PET}&surface=any&lock=${encodeURIComponent(state.lockId)}`,
          { text, duration: state.config.duration, anim: false },
        );
      }
    }
    return Boolean(r && r.ok);
  }

  /** 取占用锁（失败只记日志，下一轮再试）。 */
  async function acquireLock() {
    const r = await call('POST', `/lock?pet=${PET}&surface=any`, { ttl: LOCK_TTL });
    if (r && r.ok && r.lockId) {
      state.lockId = r.lockId;
      return true;
    }
    warn(`取占用锁失败：${r?.reason ?? 'no-response'}`);
    return false;
  }

  /** 释放占用锁（幂等：没有锁也算成功）。 */
  async function releaseLock() {
    const id = state.lockId;
    state.lockId = null;
    if (!id) return;
    await call('POST', `/lock/release?pet=${PET}`, { lockId: id });
  }

  /** 续期：锁到期前把它续上，避免长会话中途丢锁。 */
  function startRenew() {
    stopRenew();
    state.renewTimer = setInterval(() => {
      if (!state.on) return;
      call('POST', `/lock?pet=${PET}&surface=any`, { lockId: state.lockId, ttl: LOCK_TTL }).then((r) => {
        if (r && r.ok && r.lockId) state.lockId = r.lockId;
        else warn(`续期失败：${r?.reason ?? 'no-response'}`);
      });
    }, LOCK_RENEW);
  }

  function stopRenew() {
    if (state.renewTimer) clearInterval(state.renewTimer);
    state.renewTimer = null;
  }

  /** 可用的 PowerShell 解释器：DSH 宿主进程通常没有 pwsh 的 PATH，按候选路径找一遍。 */
  const SHELLS = [
    'C:\\Program Files\\PowerShell\\7\\pwsh.exe',
    'C:\\Program Files\\PowerShell\\7-preview\\pwsh.exe',
    'C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe',
  ];
  let shellPath = null;

  /** 解析并缓存 PowerShell 可执行文件路径。 */
  async function resolveShell() {
    if (shellPath) return shellPath;
    for (const candidate of SHELLS) {
      try {
        await access(candidate);
        shellPath = candidate;
        return shellPath;
      } catch {
        /* 下一个 */
      }
    }
    shellPath = 'powershell.exe';
    return shellPath;
  }

  /**
   * 调一次 OCR 脚本，拿回 { ok, text, ms, source, fallback }。
   * 抓取范围优先级：显式 region 矩形 > 按窗口标题抓客户区 > 整屏。
   */
  async function runOcr(jpegPath) {
    const { region, lang, window: windowTitle, windowClass } = state.config;
    const shell = await resolveShell();
    const args = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(ROOT, 'ocr.ps1'), '-Lang', lang];
    if (region && Number.isFinite(region.x)) {
      args.push('-X', String(region.x), '-Y', String(region.y), '-W', String(region.width), '-H', String(region.height));
    } else {
      // 类名优先（不随标题变化），标题作为兜底
      if (typeof windowClass === 'string' && windowClass.trim() !== '') {
        args.push('-WindowClass', windowClass.trim());
      }
      if (typeof windowTitle === 'string' && windowTitle.trim() !== '') {
        args.push('-Window', windowTitle.trim());
      }
      if (jpegPath) args.push('-Jpeg', jpegPath);
    }
    return new Promise((resolve) => {
      const child = spawn(shell, args, { windowsHide: true });
      let out = '';
      let err = '';
      const timer = setTimeout(() => {
        child.kill();
        resolve({ ok: false, error: 'ocr-timeout' });
      }, 20_000);
      child.stdout.on('data', (d) => (out += d));
      child.stderr.on('data', (d) => (err += d));
      child.on('error', () => {
        clearTimeout(timer);
        resolve({ ok: false, error: 'spawn-failed' });
      });
      child.on('close', () => {
        clearTimeout(timer);
        try {
          resolve(JSON.parse(out.trim().split(/\r?\n/).pop()));
        } catch {
          resolve({ ok: false, error: err.trim() || 'bad-json' });
        }
      });
    });
  }

  /**
   * 取 DeepSeek 官方 key：优先走 DSH 凭据服务，拿不到再读 `~/.dsh/.credentials.yaml`。
   * key 只在内存里用，任何日志都不打印。
   * @returns {Promise<string | null>}
   */
  async function resolveApiKey() {
    try {
      const mod = await import('@deepseek-ai/dsh-credentials');
      const ref = mod?.credentialRef?.('DEEPSEEK_API_KEY');
      const got = await ctx.credentials?.resolve?.(ref);
      if (got && typeof got.value === 'string' && got.value !== '') return got.value;
    } catch {
      /* 退回文件 */
    }
    try {
      const home = process.env.USERPROFILE ?? process.env.HOME ?? '';
      const yaml = await readFile(join(home, '.dsh', '.credentials.yaml'), 'utf8');
      const m = yaml.match(/DEEPSEEK_API_KEY:\s*(\S+)/);
      if (m) return m[1];
    } catch {
      /* 没有就没有 */
    }
    return null;
  }

  /**
   * 视觉通道：把窗口截图发给模型翻译。
   * @param {string} jpegPath - 本帧 JPEG。
   * @param {AbortSignal} outer - 关闭时中止。
   * @returns {Promise<{ok:true,text:string,usage:{prompt:number,completion:number}} | {ok:false,error:string}>}
   */
  async function translateVision(jpegPath, outer) {
    const selection = ctx.agentDefaultModel?.currentSelection?.();
    if (!selection?.model) return { ok: false, error: '当前对话未配置模型' };
    const key = await resolveApiKey();
    if (!key) return { ok: false, error: '没有可用的 DEEPSEEK_API_KEY' };

    let b64;
    try {
      b64 = (await readFile(jpegPath)).toString('base64');
    } catch (e) {
      return { ok: false, error: `读图失败：${e instanceof Error ? e.message : String(e)}` };
    }

    const timeout = AbortSignal.timeout(Number.isFinite(state.config.visionTimeoutMs) ? state.config.visionTimeoutMs : 25_000);
    const signal = outer && typeof AbortSignal.any === 'function' ? AbortSignal.any([outer, timeout]) : timeout;

    let res;
    try {
      res = await fetch(`${String(state.config.visionApiBase).replace(/\/+$/, '')}/chat/completions`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${key}`, 'Content-Type': 'application/json' },
        signal,
        body: JSON.stringify({
          model: selection.model,
          temperature: 0.2,
          max_tokens: Number.isFinite(state.config.visionMaxTokens) ? state.config.visionMaxTokens : 300,
          messages: [
            { role: 'system', content: state.config.visionSystem },
            {
              role: 'user',
              content: [
                { type: 'text', text: '把这张游戏画面里的英文翻译成中文，只输出译文。' },
                { type: 'image_url', image_url: { url: `data:image/jpeg;base64,${b64}` } },
              ],
            },
          ],
        }),
      });
    } catch (e) {
      return { ok: false, error: `请求失败：${e instanceof Error ? e.message : String(e)}` };
    }

    const body = await res.json().catch(() => ({}));
    if (!res.ok || body.error) {
      return { ok: false, error: `HTTP ${res.status} ${JSON.stringify(body.error ?? {}).slice(0, 120)}` };
    }
    const text = String(body.choices?.[0]?.message?.content ?? '').trim();
    if (text === '') return { ok: false, error: '模型未返回译文' };
    return {
      ok: true,
      text,
      usage: { prompt: body.usage?.prompt_tokens ?? 0, completion: body.usage?.completion_tokens ?? 0 },
    };
  }

  /**
   * 翻译待处理的一帧（异步，不阻塞抓屏）。
   *
   * 同一时刻只允许一次翻译在途：新帧只排队，旧帧直接丢弃。
   * 视觉通道失败时自动回退到 OCR 文本，不吞掉这一帧。
   */
  async function processPending() {
    if (!state.on || state.translating) return;
    const job = state.pendingJob;
    if (!job) return;
    state.pendingJob = null;
    state.translating = true;
    const abort = new AbortController();
    state.abort = abort;
    const text = job.text;
    try {
      let zh = '';
      if (job.kind === 'vision' && job.jpeg) {
        const v = await translateVision(job.jpeg, abort.signal);
        if (v.ok) {
          zh = v.text;
          log(`视觉翻译 prompt=${v.usage.prompt} completion=${v.usage.completion}（${text.length} 字符）`);
        } else if (state.on) {
          warn(`视觉翻译失败（${v.error}），回退 OCR 文本`);
        }
      }
      if (zh === '') {
        const res = await translate(ctx, text, state.config.system, abort.signal, state.config.translateTimeoutMs);
        if (!res.ok) {
          // 关闭时中止属于正常流程，不当失败刷日志
          if (state.on) warn(`翻译失败：${res.error}（原文 ${text.slice(0, 50)}）`);
          return;
        }
        zh = res.text;
      }
      // 关闭 / 卸载过程中返回的结果一律丢弃
      if (!state.on || !state.alive) return;
      if (zh === state.lastTranslated) return;
      // OCR 抖动会让同一段画面被翻两次，两次译文通常只差几个字：
      // 开头 10 个字相同就当作同一句，跳过重复弹泡（调用已经发生，这里只治气泡刷屏）
      if (state.lastTranslated !== '' && zh.slice(0, 10) === state.lastTranslated.slice(0, 10)) {
        log(`译文与上一条相同，跳过重复弹泡：${zh.slice(0, 20)}`);
        state.lastTranslated = zh;
        return;
      }
      state.lastTranslated = zh;
      const said = await say(state.config.prefix + zh.slice(0, 300));
      if (!said) {
        warn('写气泡失败（占用锁可能已被用户收回），自动关闭');
        await stop();
        return;
      }
      log(`已翻译 ${text.length} 字符：${zh.slice(0, 40)}`);
    } catch (e) {
      warn(`翻译异常：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      // 视觉通道的临时 JPEG 用完即删
      if (job.jpeg) await unlink(job.jpeg).catch(() => {});
      state.translating = false;
      state.abort = null;
      // 期间又来了新帧就接着翻
      if (state.on && state.pendingJob) void processPending();
    }
  }

  /** 一轮：截屏 → OCR → 判重 → 排进翻译队列（翻译在后台跑，不堵下一拍）。 */
  async function tick() {
    if (!state.on) return;
    let nextDelay = state.config.intervalMs;
    try {
      const wantsVision = state.config.translateMode === 'vision';
      const jpegPath = wantsVision ? join(process.env.TEMP ?? '.', `dsh-st-vision-${Date.now()}.jpg`) : '';
      const ocr = await runOcr(jpegPath);
      if (!ocr?.ok) {
        warn(`OCR 失败：${ocr?.error ?? 'unknown'}`);
      } else {
        // 抓取范围变化时留一条痕迹：窗口没找到会一直回退整屏，不报出来很难查
        const src = `${ocr.source ?? '?'} ${ocr.w ?? '?'}x${ocr.h ?? '?'}${ocr.fallback ? ` fallback=${ocr.fallback}` : ''}`;
        if (src !== state.lastSource) {
          state.lastSource = src;
          log(`抓取范围：${src}`);
        }
        if (ocr.fallback && state.lastFallback !== ocr.fallback) {
          state.lastFallback = ocr.fallback;
          warn(`窗口「${state.config.window ?? ''}」没找到（${ocr.fallback}），已改用整屏`);
        } else if (!ocr.fallback) {
          state.lastFallback = '';
        }
        // 目标窗口不在前台：此时抓到的会是被遮挡后的画面，直接跳过这一拍
        if (ocr.skipped) {
          if (state.lastSkip !== ocr.skipped) {
            state.lastSkip = ocr.skipped;
            log(`跳过：${ocr.skipped === 'not-foreground' ? '目标窗口不在前台' : ocr.skipped}`);
          }
          if (jpegPath) await unlink(jpegPath).catch(() => {});
          return;
        }
        state.lastSkip = '';
        const text = cleanText(ocr.text).slice(0, state.config.maxChars);
        if (text.length >= state.config.minChars && looksEnglish(text)) {
          const norm = normalize(text);
          // 稳定判定以「文本」为准：像素每帧都在动（光标闪烁、精灵动画），拿哈希判稳会永远等不到稳定。
          // 相似度 ≥ 0.8 视为同一段（容忍 OCR 抖动），否则视为新文案、重新计时。
          const sameText = state.candidateNorm !== '' && similarity(norm, state.candidateNorm) >= 0.8;

          if (!sameText) {
            // 文案还在变（打字机效果）：记下新候选，settleMs 后再看它是否稳定
            state.candidateNorm = norm;
            state.candidateSince = Date.now();
            nextDelay = state.config.settleMs;
            if (!state.unstable) {
              state.unstable = true;
              log(`文案变化中，等 ${state.config.settleMs} ms 稳定后再翻`);
            }
            if (jpegPath) await unlink(jpegPath).catch(() => {});
          } else {
            state.candidateNorm = norm;
            if (norm !== state.translatedNorm && Date.now() - state.candidateSince >= state.config.settleMs) {
              // 稳定满 settleMs 且这段文案还没翻过 → 才真正触发
              state.unstable = false;
              state.translatedNorm = norm;
              log(`OCR ${text.length} 字符：${text.slice(0, 60)}`);
              state.pendingJob = { kind: wantsVision ? 'vision' : 'text', text, jpeg: jpegPath };
              void processPending();
            } else if (jpegPath) {
              // 还没稳够时间，或这段已经翻过
              await unlink(jpegPath).catch(() => {});
            }
          }
        } else if (jpegPath) {
          // 没通过字符数/语言门槛，这张图也不用了
          await unlink(jpegPath).catch(() => {});
        }
      }
    } catch (e) {
      warn(`tick 异常：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      if (state.on && state.alive) state.timer = setTimeout(tick, nextDelay ?? state.config.intervalMs);
    }
  }

  /** 打开：取锁 → 打招呼 → 开始循环 → 续期。 */
  async function start() {
    state.on = true;
    state.recent = [];
    state.lastTranslated = '';
    state.candidateNorm = '';
    state.candidateSince = 0;
    state.translatedNorm = '';
    state.unstable = false;
    state.pendingJob = null;
    const ok = await acquireLock();
    if (!ok) {
      state.on = false;
      warn('屏幕翻译开不了：桌宠正被别的插件占用。');
      return;
    }
    startRenew();
    await say('屏幕翻译已开启，画面里的英文我会念成中文。');
    log('已开启');
    state.timer = setTimeout(tick, 800);
  }

  /** 关闭：掐断在途翻译 → 停循环 → 停续期 → 放锁 → 道别。 */
  async function stop() {
    state.on = false;
    if (state.timer) clearTimeout(state.timer);
    state.timer = null;
    state.pendingJob = null;
    // 在途请求立刻中止：否则它返回后还会往已释放的锁上写气泡
    if (state.abort) {
      state.abort.abort();
      state.abort = null;
    }
    stopRenew();
    await say('屏幕翻译已关闭。');
    await releaseLock();
    log('已关闭');
  }

  /** 轮询菜单点击：长轮询 5s，一次点击只投递一次。 */
  async function pollMenu() {
    while (state.alive) {
      const r = await call('GET', `/menu/events?owner=${OWNER}&wait=5000`);
      if (!state.alive) return;
      // 服务端形状是 { ok: true, events: { [label]: petId | null } }，兼容直接平铺的形状。
      const events = r?.events ?? r;
      if (events && typeof events === 'object' && events[LABEL]) {
        try {
          if (state.on) await stop();
          else await start();
        } catch (e) {
          warn(`切换失败：${e instanceof Error ? e.message : String(e)}`);
        }
      }
      if (!r) await new Promise((res) => setTimeout(res, 2000));
    }
  }

  /** 注册菜单项：dsh-pet 可能比本插件晚加载，失败就退避重试。 */
  async function registerMenu() {
    for (let i = 0; state.alive && i < 60; i++) {
      const r = await call('POST', '/menu/item', { owner: OWNER, labels: [LABEL], target: 'both' });
      if (r && r.ok) {
        log('菜单项已注册');
        return;
      }
      await new Promise((res) => setTimeout(res, 3000));
    }
    warn('菜单项注册失败（dsh-pet 未就绪？）');
  }

  ctx.effect(
    () => {
      loadConfig().then((cfg) => {
        state.config = { ...DEFAULTS, ...cfg };
        log(`已加载配置 v3：mode=${state.config.region ? 'region' : state.config.window ? `window(${state.config.window})` : 'full'}`);
      });
      registerMenu().then(() => (state.polling = pollMenu()));
      return () => {
        state.alive = false;
        state.on = false;
        if (state.timer) clearTimeout(state.timer);
        if (state.abort) {
          state.abort.abort();
          state.abort = null;
        }
        stopRenew();
        void releaseLock();
        void call('DELETE', `/menu/item?owner=${OWNER}`);
      };
    },
    'dsh-pet-screen-translator: 屏幕翻译菜单项与识别循环',
  );
}
