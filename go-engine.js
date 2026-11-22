/**
 * Go 引擎外壳：把「屏幕翻译开关」翻译成「Go 守护进程的起停」，并在菜单项上显示当前状态。
 *
 * 开：菜单点一次 → spawn `translatord.exe run`；
 * 关：再点一次（或插件卸载）→ 先 POST /shutdown 让它**释放占用锁**再退出，
 *     3 秒内没退再硬杀。Windows 下 child.kill() 是硬终止，Go 收不到信号，
 *     锁会挂到 TTL 过期，所以必须走控制通道。
 *
 * 状态展示：菜单项文字随状态变化（已关闭 / 启动中 / 运行中 / 识别中 / 翻译中 / 出错），
 * 点击判定用前缀匹配，所以文字变了也能点。
 *
 * @module dsh-pet-screen-translator/go-engine
 */

import { spawn } from 'node:child_process';
import { appendFile, mkdir } from 'node:fs/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = dirname(fileURLToPath(import.meta.url));
const WORKSPACE = process.env.DSH_PET_ST_WORKSPACE ?? 'D:\\deepseek\\projects';
const BIN = join(WORKSPACE, 'dsh-pet-screen-translator', 'bin', 'translatord.exe');
const LOG = join(WORKSPACE, 'logs', 'dsh-pet-screen-translator-shell.log');

const OWNER = 'screen-translator';
/** 菜单项统一前缀；状态显示在后缀里。 */
const PREFIX = '屏幕翻译';
const REGION_PREFIX = '判稳区域';

/** 判稳区域模式 → 菜单文案。 */
function regionLabelFor(mode) {
  const suffix = mode === 'auto' ? '自动学习' : '固定';
  return PREFIX + ' ▸ ' + REGION_PREFIX + '：' + suffix;
}

/** 状态 → 菜单文案。统一用「父 ▸ 子」写法，dsh-pet 会据此收进同一个子菜单。 */
function labelFor(state) {
  // 守护进程会报 blocked-occluded / blocked-not-foreground：此时抓到的会是别人的画面
  if (typeof state === 'string' && state.startsWith('blocked')) {
    const why = state.endsWith('occluded')
      ? '画面被遮挡'
      : state.endsWith('window-not-found')
        ? '游戏窗口未找到'
        : state.endsWith('window-minimized')
          ? '游戏已最小化'
        : '游戏不在前台';
    return `${PREFIX} ▸ 开关：${why}`;
  }
  const suffix = {
    starting: '启动中',
    idle: '运行中',
    settling: '识别中',
    translating: '翻译中',
    error: '出错',
    exited: '已退出',
  }[state] ?? '已关闭';
  return `${PREFIX} ▸ 开关：${suffix}`;
}

async function logLine(msg) {
  try {
    await mkdir(dirname(LOG), { recursive: true });
    await appendFile(LOG, `${new Date().toISOString()} [shell] ${String(msg).replace(/\s+/g, ' ').trim()}\n`, 'utf8');
  } catch {
    /* 日志失败不影响主流程 */
  }
}

/** 等到子进程退出，或超时。 */
function waitExit(child, ms) {
  if (!child || child.exitCode !== null) return Promise.resolve(true);
  return new Promise((resolve) => {
    const timer = setTimeout(() => resolve(false), ms);
    child.once('exit', () => {
      clearTimeout(timer);
      resolve(true);
    });
  });
}

/**
 * 插件主体：菜单点击 = 起/停守护进程，菜单文字 = 当前状态。
 * @param {any} ctx - DSH 宿主上下文。
 */
export function apply(ctx) {
  const state = { child: null, controlPort: 0, alive: true, state: 'stopped', label: '', pollTimer: null, region: 'fixed', regionLabel: '', restarts: 0, stopping: false };

  const dshPort = () => ctx.webServer?.port ?? 0;

  async function call(method, path, body, timeoutMs = 15000, base) {
    try {
      const root = base ?? `http://127.0.0.1:${dshPort()}`;
      const res = await fetch(root + path, {
        method,
        headers: body === undefined ? undefined : { 'content-type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(timeoutMs),
      });
      return await res.json().catch(() => null);
    } catch {
      return null;
    }
  }

  /** 把状态写到菜单项上（同一 owner 覆盖式注册）。 */
  async function render(next) {
    state.state = next;
    const label = labelFor(next);
    if (label === state.label) return;
    state.label = label;
    await call('POST', '/dsh-pet-7340/menu/item', { owner: OWNER, labels: [label, regionLabelFor(state.region)], target: 'both' });
  }

  /** 轮询守护进程状态，把 settling/translating 显示出来。 */
  function startStatusPoll() {
    stopStatusPoll();
    state.pollTimer = setInterval(async () => {
      if (!state.controlPort) return;
      const r = await call('GET', '/status', undefined, 2000, `http://127.0.0.1:${state.controlPort}`);
      if (r?.ok && typeof r.state === 'string') {
        void render(r.state);
        if (typeof r.region === 'string') state.region = r.region;
      }
    }, 1500);
  }

  function stopStatusPoll() {
    if (state.pollTimer) clearInterval(state.pollTimer);
    state.pollTimer = null;
  }

  /** 停止：先礼貌关闭（释放锁），再兜底硬杀。 */
  async function stopDaemon() {
    state.stopping = true;
    const child = state.child;
    stopStatusPoll();
    if (!child || child.exitCode !== null) {
      state.child = null;
      state.controlPort = 0;
      await render('stopped');
      return;
    }
    if (state.controlPort) {
      try {
        await fetch(`http://127.0.0.1:${state.controlPort}/shutdown`, {
          method: 'POST',
          signal: AbortSignal.timeout(1500),
        });
        void logLine('已请求优雅关闭（等待进程释放占用锁）');
      } catch (e) {
        void logLine(`优雅关闭请求失败：${e instanceof Error ? e.message : String(e)}`);
      }
      if (await waitExit(child, 3000)) {
        void logLine('守护进程已优雅退出');
        state.child = null;
        state.controlPort = 0;
        await render('stopped');
        return;
      }
      void logLine('优雅关闭超时，改为强制结束');
    }
    child.kill();
    await waitExit(child, 1500);
    state.child = null;
    state.controlPort = 0;
    await render('stopped');
  }

  /** 启动：spawn 守护进程，菜单先显示"启动中"，拿到控制端口后转"运行中"。 */
  async function startDaemon() {
    if (state.child && state.child.exitCode === null) return;
    const p = dshPort();
    if (!p) {
      void logLine('拿不到 DSH 端口，拒绝启动守护进程');
      return;
    }
    state.controlPort = 0;
    state.stopping = false;
    await render('starting');
    const child = spawn(BIN, ['run', '--dsh-port', String(p), '--workspace', WORKSPACE], {
      windowsHide: true,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    state.child = child;

    let buf = '';
    child.stdout?.on('data', (d) => {
      buf += String(d);
      const m = buf.match(/CONTROL_PORT=(\d+)/);
      if (m) {
        state.controlPort = Number(m[1]);
        buf = '';
        void logLine(`控制端口=${state.controlPort}`);
        void render('idle');
        startStatusPoll();
      }
      void logLine(`stdout: ${String(d).slice(0, 300)}`);
    });
    child.stderr?.on('data', (d) => void logLine(`stderr: ${String(d).slice(0, 400)}`));
    child.on('exit', (code, signal) => {
      void logLine(`daemon exited code=${code} signal=${signal}`);
      if (state.child !== child) return;
      state.child = null;
      state.controlPort = 0;
      stopStatusPoll();
      void render('exited');

      // 意外退出（非主动关闭、非 0 退出码）→ 自动重启，最多 3 次，避免玩到一半悄无声息地停掉
      const unexpected = state.alive && !state.stopping && code !== 0;
      if (unexpected && state.restarts < 3) {
        state.restarts += 1;
        void logLine(`守护进程异常退出，${3}s 后自动重启（第 ${state.restarts}/3 次）`);
        setTimeout(() => {
          if (state.alive && !state.child) void startDaemon();
        }, 3000);
      }
    });
    child.on('error', (e) => {
      void logLine(`daemon spawn failed: ${e.message}`);
      void render('error');
    });
    void logLine(`菜单点击：启动守护进程 pid=${child.pid} port=${p}`);
  }

  async function toggleRegion() {
    if (!state.controlPort) return;
    const next = state.region === 'auto' ? 'fixed' : 'auto';
    const r = await call('POST', `/region?mode=${next}`, undefined, 3000, `http://127.0.0.1:${state.controlPort}`);
    if (r?.ok) {
      state.region = r.region ?? next;
      state.regionLabel = regionLabelFor(state.region);
      await call('POST', '/dsh-pet-7340/menu/item', { owner: OWNER, labels: [labelFor(state.state), state.regionLabel], target: 'both' });
      void logLine(`判稳区域切换为 ${state.region}`);
    }
  }

  async function toggle() {
    if (state.child && state.child.exitCode === null) {
      await stopDaemon();
    } else {
      await startDaemon();
    }
  }

  async function registerMenu() {
    for (let i = 0; state.alive && i < 60; i++) {
      const r = await call('POST', '/dsh-pet-7340/menu/item', { owner: OWNER, labels: [labelFor('stopped'), regionLabelFor(state.region)], target: 'both' });
      if (r?.ok) {
        state.label = labelFor('stopped');
        state.regionLabel = regionLabelFor(state.region);
        void logLine('菜单项已注册');
        return;
      }
      await new Promise((res) => setTimeout(res, 3000));
    }
    void logLine('菜单项注册失败（dsh-pet 未就绪？）');
  }

  async function poll() {
    while (state.alive) {
      const r = await call('GET', `/dsh-pet-7340/menu/events?owner=${OWNER}&wait=5000`, undefined, 20000);
      if (!state.alive) return;
      // 桌宠 reload / 重装插件会把注册表清空（unknown-owner）→ 自动重新注册，避免菜单消失
      if (!r || r?.ok === false) {
        void logLine('菜单注册已失效，重新注册');
        state.label = '';
        await registerMenu();
        await new Promise((res) => setTimeout(res, 1000));
        continue;
      }
      const events = r?.events ?? r;
      if (events && typeof events === 'object') {
        // 标签形如「屏幕翻译 ▸ 开关：运行中」「屏幕翻译 ▸ 判稳区域：固定」，
        // 先认区域项（含「判稳区域」），再认开关项。
        const keys = Object.keys(events).filter((k) => events[k]);
        const clickedRegion = keys.find((k) => k.includes(REGION_PREFIX));
        if (clickedRegion) {
          await toggleRegion();
        } else {
          const clicked = keys.find((k) => k.startsWith(PREFIX));
          if (clicked) await toggle();
        }
      }
      if (!r) await new Promise((res) => setTimeout(res, 2000));
    }
  }

  ctx.effect(
    () => {
      registerMenu().then(() => poll());
      return () => {
        state.alive = false;
        stopStatusPoll();
        void stopDaemon();
      };
    },
    'dsh-pet-screen-translator-go: 守护进程生命周期与状态展示',
  );
}
