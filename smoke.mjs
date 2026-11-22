// go-engine.js 的加载冒烟测试：不启动真实 DSH，只验证 apply() 能装配起来不抛异常。
// 用途：防止「函数没定义 / 语法错」这类问题把宿主带崩（已经踩过一次）。

const mod = await import('file:///D:/deepseek/projects/dsh-pet-screen-translator/go-engine.js');
console.log('导出:', Object.keys(mod).join(', '));

// 假 fetch：菜单注册返回 ok，事件轮询返回空，区域切换返回 ok
let region = 'fixed';
globalThis.fetch = async (url, init = {}) => {
  const u = String(url);
  // 事件轮询在真实宿主里是长轮询（wait=5000），这里必须模拟延迟，否则会变成忙循环
  if (u.includes('/menu/events')) await new Promise((r) => setTimeout(r, 100));
  const body = u.includes('/menu/item')
    ? { ok: true }
    : u.includes('/menu/events')
      ? { ok: true, events: {} }
      : u.includes('/region')
        ? { ok: true, region: region }
        : { ok: true };
  if (u.includes('/region') && init.method === 'POST') {
    region = u.includes('mode=auto') ? 'auto' : 'fixed';
    body.region = region;
  }
  return { ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) };
};

let disposer = null;
const ctx = {
  webServer: { port: 55555 },
  effect(fn) {
    disposer = fn();
  },
  logger: { warn: (...a) => console.log('[warn]', ...a), info: () => {} },
};

mod.apply(ctx);
await new Promise((r) => setTimeout(r, 300));
console.log('apply 成功，disposer =', typeof disposer);
if (typeof disposer === 'function') disposer();
await new Promise((r) => setTimeout(r, 100));
console.log('冒烟测试通过');
process.exit(0);
