/**
 * BreezeBlue 语音实现：把 Breeze TTS 注册进 dsh-pet 的语音注册表。
 *
 * 桌宠的 `/voice/speak` 只认注册表里的实现；这个模块在翻译插件加载时注册
 * provider `breeze`，音色 id 用 `voc_...`。翻译引擎只在完整译文出来后才带
 * `speak: true` 广播，所以流式半截不会念。
 *
 * Key 读取顺序：`BREEZE_API_KEY` 环境变量 → `breeze-key.txt`（gitignore）→
 * `config.json` 的 `breeze.apiKey`。Key 不进日志、不进错误消息。
 *
 * @module dsh-pet-screen-translator/breeze-voice
 */

import { appendFile, mkdir, readFile, rename, stat } from 'node:fs/promises';
import { join } from 'node:path';

/** 工作区根目录（与 index.js 同一口径：插件被复制进 profile，不能用 import.meta.url 推）。 */
const WORKSPACE = process.env.DSH_PET_ST_WORKSPACE ?? 'D:\\deepseek\\projects';
const PROJECT_DIR = join(WORKSPACE, 'dsh-pet-screen-translator');
const CONFIG_PATH = join(PROJECT_DIR, 'config.json');
const KEY_FILE = join(PROJECT_DIR, 'breeze-key.txt');
const LOG_DIR = join(WORKSPACE, 'logs');
const LOG_NAME = 'breeze-voice.log';
const LOG_MAX_BYTES = 1024 * 1024;
const LOG_KEEP = 3;

/** Breeze Developer API 基址。 */
const API_BASE = 'https://api.breeze.blue';
/** Breeze 单次合成的字符上限（官方默认 1000）。 */
const MAX_CHARS = 1000;

/** 缺省音色；config.json 的 `breeze` 段可覆盖。 */
const DEFAULTS = {
  providerName: 'BreezeBlue',
  voiceId: 'voc_puvq4zh92bvs',
  voiceName: '灵动顽皮幼女',
  voiceTag: 'Breeze · 大肥鱼',
  timeoutMs: 60000,
};

/** 写一行日志（单行、按大小轮转；不记 Key、不记全文）。 */
async function log(level, msg) {
  try {
    await mkdir(LOG_DIR, { recursive: true });
    const file = join(LOG_DIR, LOG_NAME);
    await appendFile(file, `${new Date().toISOString()} [${level}] ${String(msg).replace(/\s+/g, ' ').trim()}\n`, 'utf8');
    const info = await stat(file);
    if (info.size >= LOG_MAX_BYTES) {
      for (let i = LOG_KEEP; i >= 1; i--) {
        await rename(i === 1 ? file : `${file}.${i - 1}`, `${file}.${i}`).catch(() => {});
      }
    }
  } catch {
    /* 日志失败不影响合成 */
  }
}

/** 读配置：只取 `breeze` 段；文件缺失/损坏时用默认值。 */
async function loadConfig() {
  try {
    const raw = (await readFile(CONFIG_PATH, 'utf8')).replace(/^\uFEFF/, '');
    const user = JSON.parse(raw);
    const b = user && typeof user === 'object' ? user.breeze : undefined;
    const merged = { ...DEFAULTS };
    if (b && typeof b === 'object') {
      for (const key of ['providerName', 'voiceId', 'voiceName', 'voiceTag', 'timeoutMs', 'apiKey']) {
        if (typeof b[key] === 'string' && b[key].trim() !== '') merged[key] = b[key].trim();
        if (key === 'timeoutMs' && Number.isFinite(Number(b[key]))) merged[key] = Number(b[key]);
      }
    }
    return merged;
  } catch {
    return { ...DEFAULTS };
  }
}

/** 取 Breeze API Key；每次合成现读，填好 key 后不用重启。 */
async function resolveKey() {
  const env = process.env.BREEZE_API_KEY;
  if (typeof env === 'string' && env.trim() !== '') return env.trim();
  try {
    const raw = (await readFile(KEY_FILE, 'utf8')).replace(/^\uFEFF/, '').trim();
    if (raw !== '') return raw.split(/\r?\n/, 1)[0].trim();
  } catch {
    /* 文件不存在：继续看配置 */
  }
  const cfg = await loadConfig();
  return typeof cfg.apiKey === 'string' ? cfg.apiKey : '';
}

/** 把桌宠的语速百分比映射成 Breeze 的 speed 倍率（0.5–2.0）。 */
function speedOf(rate) {
  const n = Number(rate);
  const speed = 1 + (Number.isFinite(n) ? n : 0) / 100;
  return Math.min(2, Math.max(0.5, speed));
}

/** 从 Breeze 错误响应里取一句可读原因（不含 Key）。 */
async function errorDetail(res) {
  try {
    const text = await res.text();
    try {
      const parsed = JSON.parse(text);
      const detail = parsed && (parsed.detail || parsed.error);
      if (typeof detail === 'string' && detail !== '') return detail.slice(0, 200);
    } catch {
      /* 不是 JSON：退回截断文本 */
    }
    return text.slice(0, 200);
  } catch {
    return '';
  }
}

/**
 * 造 Breeze provider。
 *
 * @param {object} cfg - 已合并的 `breeze` 配置。
 * @returns {object} dsh-pet `voice.register` 认得的实现。
 */
function makeProvider(cfg) {
  return {
    id: 'breeze',
    name: cfg.providerName,
    voices: [{ id: cfg.voiceId, name: cfg.voiceName, tag: cfg.voiceTag }],
    format: 'mp3',
    timeoutMs: cfg.timeoutMs,
    async synthesize({ text, voice, rate, signal }) {
      const key = await resolveKey();
      if (!key) throw new Error('BREEZE_API_KEY not configured (breeze-key.txt)');
      const body = String(text ?? '');
      const chars = [...body].length;
      if (chars === 0) throw new Error('empty text');
      if (chars > MAX_CHARS) throw new Error(`text too long for one Breeze request: ${chars} > ${MAX_CHARS}`);
      const started = Date.now();
      let res;
      try {
        res = await fetch(`${API_BASE}/v1/text-to-speech/${encodeURIComponent(voice)}?output_format=mp3`, {
          method: 'POST',
          headers: { 'xi-api-key': key, 'content-type': 'application/json' },
          body: JSON.stringify({ text: body, voice_settings: { speed: speedOf(rate) } }),
          signal,
        });
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e);
        await log('warn', `synth fetch failed chars=${chars} ms=${Date.now() - started} err=${msg}`);
        throw e;
      }
      if (!res.ok) {
        const detail = await errorDetail(res);
        const msg = `breeze HTTP ${res.status}${detail ? `: ${detail}` : ''}`;
        await log('warn', `synth failed chars=${chars} ms=${Date.now() - started} ${msg}`);
        throw new Error(msg);
      }
      const bytes = new Uint8Array(await res.arrayBuffer());
      if (bytes.length === 0) {
        await log('warn', `synth empty chars=${chars} ms=${Date.now() - started}`);
        throw new Error('breeze returned empty audio');
      }
      await log('info', `synth ok voice=${voice} chars=${chars} bytes=${bytes.length} ms=${Date.now() - started}`);
      return bytes;
    },
  };
}

/**
 * 在 dsh-pet 的宿主服务上注册 Breeze 音色。
 *
 * `index.js` 已把 `petApi7340` 放进 `inject`，正常情况下这里拿得到；
 * 拿不到只告警，不阻塞翻译菜单。
 *
 * @param {any} ctx - DSH 宿主上下文。
 */
export function installBreezeVoice(ctx) {
  let dispose;
  let stopped = false;
  const run = async () => {
    try {
      const cfg = await loadConfig();
      if (stopped) return;
      const api = ctx.get?.('petApi7340');
      if (!api?.voice?.register) {
        await log('warn', 'petApi7340.voice.register 不可用，Breeze 音色未注册');
        return;
      }
      dispose = api.voice.register(makeProvider(cfg));
      await log('info', `provider registered: breeze/${cfg.voiceId} (${cfg.voiceName})`);
    } catch (e) {
      await log('warn', `register failed: ${e instanceof Error ? e.message : String(e)}`);
    }
  };
  const cleanup = () => {
    stopped = true;
    try {
      dispose?.();
    } catch {
      /* 注销失败不影响卸载 */
    }
  };
  if (typeof ctx.effect === 'function') ctx.effect(() => {
    void run();
    return cleanup;
  }, 'dsh-pet-screen-translator: Breeze voice provider');
  else void run();
}
