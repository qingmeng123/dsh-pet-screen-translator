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
import { access, appendFile, mkdir, readFile, realpath, rename, stat } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

/** 插件行 id（与 cordis.patch.yml 一致）。 */
export const name = 'screen-translator';

/** 需要注入的宿主服务：webServer 取端口，agentDefaultModel + llm 用来翻译。 */
export const inject = ['webServer', 'agentDefaultModel', 'llm'];

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
  /** 截屏区域：{x,y,width,height}；null = 主屏全屏。 */
  region: null,
  /** OCR 语言标识。 */
  lang: 'en-GB',
  /** 送进模型的提示词。 */
  system:
    '你是游戏画面的实时翻译。把用户给出的英文画面文字翻译成自然、简短的中文，只输出译文本身：' +
    '不要解释、不要加引号或括号注释、不要重复原文。遇到菜单项、按钮、提醒用语就按游戏里常见的中文说法翻。' +
    '原文里出现明显缺字或乱码（例如 T?E、TRAT）时按上下文补全成最可能的词再翻，不要照抄残缺字母。',
  /** 展示译文时的前缀。 */
  prefix: '译：',
};

/** 读取配置：优先工作区里的 `dsh-pet-screen-translator/config.json`，其次插件目录内的副本。 */
async function loadConfig() {
  const candidates = [join(PROJECT_DIR, 'config.json'), join(ROOT, 'config.json')];
  for (const candidate of candidates) {
    try {
      const raw = await readFile(candidate, 'utf8');
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
 * 插件主体。
 * @param {any} ctx - DSH 宿主上下文。
 */
export function apply(ctx) {
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
    pendingText: null,
    /** 是否有翻译在途。 */
    translating: false,
    /** 在途翻译的中止器：关闭时用它掐掉请求。 */
    abort: null,
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

  /** 调一次 OCR 脚本，拿回 { ok, text, ms }。 */
  async function runOcr() {
    const { region, lang } = state.config;
    const shell = await resolveShell();
    const args = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(ROOT, 'ocr.ps1'), '-Lang', lang];
    if (region && Number.isFinite(region.x)) {
      args.push('-X', String(region.x), '-Y', String(region.y), '-W', String(region.width), '-H', String(region.height));
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
   * 翻译待处理的文本（异步，不阻塞抓屏）。
   *
   * 同一时刻只允许一次翻译在途：新帧只在 pendingText 里排队，旧帧直接丢弃；
   * 关闭时由 stop() 通过 AbortController 掐断，避免关闭后还往气泡里写字。
   */
  async function processPending() {
    if (!state.on || state.translating) return;
    const text = state.pendingText;
    if (!text) return;
    state.pendingText = null;
    state.translating = true;
    const abort = new AbortController();
    state.abort = abort;
    try {
      const res = await translate(ctx, text, state.config.system, abort.signal, state.config.translateTimeoutMs);
      if (!res.ok) {
        warn(`翻译失败：${res.error}（原文 ${text.slice(0, 50)}）`);
        return;
      }
      const zh = res.text;
      // 关闭 / 卸载过程中返回的结果一律丢弃
      if (!state.on || !state.alive) return;
      if (zh === state.lastTranslated) return;
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
      state.translating = false;
      state.abort = null;
      // 期间又来了新帧就接着翻
      if (state.on && state.pendingText) void processPending();
    }
  }

  /** 一轮：截屏 → OCR → 判重 → 排进翻译队列（翻译在后台跑，不堵下一拍）。 */
  async function tick() {
    if (!state.on) return;
    try {
      const ocr = await runOcr();
      if (!ocr?.ok) {
        warn(`OCR 失败：${ocr?.error ?? 'unknown'}`);
      } else {
        const text = cleanText(ocr.text).slice(0, state.config.maxChars);
        if (text.length >= state.config.minChars && looksEnglish(text)) {
          // 判重：短文本 OCR 抖动大，阈值放宽到 0.45；长文本仍用 0.8。比对最近 3 帧。
          const norm = normalize(text);
          const threshold = norm.length < 60 ? 0.45 : 0.8;
          const repeat = state.recent.some((prev) => similarity(norm, prev) >= threshold);
          if (!repeat) {
            state.recent.unshift(norm);
            state.recent.length = Math.min(state.recent.length, 3);
            log(`OCR ${text.length} 字符：${text.slice(0, 60)}`);
            state.pendingText = text;
            void processPending();
          } else {
            log(`跳过（与最近帧相似）：${text.slice(0, 40)}`);
          }
        }
      }
    } catch (e) {
      warn(`tick 异常：${e instanceof Error ? e.message : String(e)}`);
    } finally {
      if (state.on && state.alive) state.timer = setTimeout(tick, state.config.intervalMs);
    }
  }

  /** 打开：取锁 → 打招呼 → 开始循环 → 续期。 */
  async function start() {
    state.on = true;
    state.recent = [];
    state.lastTranslated = '';
    state.pendingText = null;
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
    state.pendingText = null;
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
