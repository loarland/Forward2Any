// 重新生成 README「界面预览」用的截图（docs/screenshots/*.png）。
//
// 一条命令跑完：起一个临时数据库的实例 → 灌一套演示数据 → 用 headless Chromium 截
// 概览 / 源 / 规则 / 投递日志（亮色），切到暗色再截概览与源。演示数据、窗口尺寸、主题
// 都在这个文件里定死，所以换台机器跑出来的是同一套图。
//
//   node scripts/screenshot.js               # 用默认浏览器、输出到 docs/screenshots
//   node scripts/screenshot.js --bin /tmp/f2a --chrome /path/to/chrome-headless-shell
//
// 依赖：node 22+（用到全局 fetch / WebSocket 与 node:sqlite）、go（没给 --bin 时自己构建）。
// 浏览器默认找 Playwright 缓存里的 chrome-headless-shell（不需要装 playwright 的 npm 包）：
// 本机的系统 Chrome 起 headless 截不出图，headless shell 可以。
const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn, spawnSync } = require('child_process');
const { DatabaseSync } = require('node:sqlite');

const ROOT = path.resolve(__dirname, '..');
const OUT_DIR = path.join(ROOT, 'docs', 'screenshots');
const PORT = Number(argValue('--port') || 18099);
const BASE = `http://127.0.0.1:${PORT}`;
const ADMIN_PASS = 'demo-password-123';
const WIDTH = 1440;
const HEIGHT = 900;
const CDP_PORT = 9222;

function argValue(name) {
  const i = process.argv.indexOf(name);
  return i >= 0 ? process.argv[i + 1] : '';
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function log(msg) {
  console.log(msg);
}

// ---------- 浏览器 ----------

function findChrome() {
  const explicit = argValue('--chrome') || process.env.CHROME_HEADLESS_SHELL;
  if (explicit) {
    return explicit;
  }
  const candidates = [];
  const cache = path.join(os.homedir(), 'Library', 'Caches', 'ms-playwright');
  if (fs.existsSync(cache)) {
    for (const dir of fs.readdirSync(cache).filter((d) => d.startsWith('chromium_headless_shell-')).sort().reverse()) {
      candidates.push(path.join(cache, dir, 'chrome-headless-shell-mac-arm64', 'chrome-headless-shell'));
      candidates.push(path.join(cache, dir, 'chrome-headless-shell-mac-x64', 'chrome-headless-shell'));
      candidates.push(path.join(cache, dir, 'chrome-headless-shell-linux64', 'chrome-headless-shell'));
    }
  }
  candidates.push('/Applications/Google Chrome.app/Contents/MacOS/Google Chrome');
  candidates.push('/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome');
  for (const c of candidates) {
    if (fs.existsSync(c)) {
      return c;
    }
  }
  throw new Error('找不到 Chromium，用 --chrome 指定一个（见文件顶部的说明）');
}

// ---------- 应用实例 ----------

function startApp(bin, dataDir, quiet) {
  const child = spawn(bin, [], {
    env: {
      ...process.env,
      F2A_DATA_DIR: dataDir,
      F2A_PORT: String(PORT),
      F2A_ADMIN_PASSWORD: ADMIN_PASS,
      F2A_BASE_URL: 'https://hooks.example.com',
      F2A_LOG_LEVEL: quiet ? 'warn' : 'info',
    },
    stdio: quiet ? 'ignore' : 'inherit',
  });
  return child;
}

async function waitHealthy(timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(BASE + '/healthz');
      if (r.ok) {
        return;
      }
    } catch (_) {
      /* 还没起来 */
    }
    await sleep(200);
  }
  throw new Error('服务没能在超时前就绪');
}

function buildBinary() {
  const out = path.join(fs.mkdtempSync(path.join(os.tmpdir(), 'f2a-shots-bin-')), 'f2a');
  log('构建二进制…');
  const r = spawnSync('go', ['build', '-o', out, './cmd/f2a'], { cwd: ROOT, stdio: 'inherit' });
  if (r.status !== 0) {
    throw new Error('go build 失败');
  }
  return out;
}

// ---------- 演示数据 ----------
//
// 和 README 里那套图保持一致：6 个源、2 条规则、12 条投递（3 轮 × 4 个目标，
// 每轮里成功/失败各两个）。时间取「一小时前」的同一个时刻，几张图之间对得上。
function seed(dataDir) {
  const db = new DatabaseSync(path.join(dataDir, 'f2a.db'));
  const now = Math.floor(Date.now() / 1000);
  const stamp = now - 3600;

  const srcCols = ['name', 'kind', 'usage', 'enabled', 'slug', 'url', 'http_method', 'headers', 'auth_mode',
    'auth_header', 'auth_secret', 'ip_allow', 'smtp_host', 'smtp_port', 'smtp_user', 'smtp_pass', 'smtp_tls',
    'mail_from', 'mail_to', 'imap_host', 'imap_port', 'imap_user', 'imap_pass', 'imap_tls', 'imap_folder',
    'imap_interval', 'created_at', 'updated_at', 'use_proxy', 'tg_token', 'tg_chat_id', 'tg_thread_id',
    'tg_endpoint', 'channel_secret', 'channel_target', 'default_body_template', 'default_subject_template'];
  const srcDefs = {
    enabled: 1, slug: '', url: '', http_method: 'POST', headers: '{}', auth_mode: 'none', auth_header: '',
    auth_secret: '', ip_allow: '', smtp_host: '', smtp_port: 587, smtp_user: '', smtp_pass: '',
    smtp_tls: 'starttls', mail_from: '', mail_to: '', imap_host: '', imap_port: 993, imap_user: '',
    imap_pass: '', imap_tls: 1, imap_folder: 'INBOX', imap_interval: 60, use_proxy: 0, tg_token: '',
    tg_chat_id: '', tg_thread_id: '', tg_endpoint: '', channel_secret: '', channel_target: '',
    default_body_template: '', default_subject_template: '',
  };
  const sources = [
    { name: 'GitHub 推送', kind: 'webhook', usage: 'in', slug: 'github-push-a1b2', auth_mode: 'token',
      auth_header: 'X-F2A-Token', auth_secret: 'gh-demo-secret-9f3a' },
    { name: '值班邮件', kind: 'email', usage: 'in', imap_host: 'imap.example.com', imap_port: 993,
      imap_user: 'alerts@example.com', imap_pass: 'demo-pass', imap_folder: 'INBOX' },
    { name: '自建通知服务', kind: 'webhook', usage: 'out', url: 'http://127.0.0.1:19099/notify',
      headers: '{"Content-Type":"application/json"}' },
    { name: '团队邮箱', kind: 'email', usage: 'out', smtp_host: 'smtp.example.com', smtp_port: 587,
      smtp_user: 'build@example.com', smtp_pass: 'demo-pass', mail_from: 'build@example.com',
      mail_to: 'dev-team@example.com' },
    { name: 'Telegram 运维群', kind: 'telegram', usage: 'out',
      tg_token: '8123456789:AAHdemoTokenNotReal_9f3a', tg_chat_id: '-1001234567890',
      tg_endpoint: 'https://api.telegram.org/bot' },
    { name: '备用端点', kind: 'webhook', usage: 'out', url: 'https://backup.example.com/hook/f2a',
      headers: '{}' },
  ];
  const ins = db.prepare(`INSERT INTO sources (${srcCols.join(',')}) VALUES (${srcCols.map(() => '?').join(',')})`);
  sources.forEach((s, i) => {
    const row = { ...srcDefs, ...s, created_at: now - 86400 * 3 + (i + 1) * 60, updated_at: now - 86400 * 3 + (i + 1) * 60 };
    ins.run(...srcCols.map((c) => row[c]));
  });

  const ruleIns = db.prepare(`INSERT INTO rules (name,enabled,from_source_ids,to_source_ids,filters,
    body_template,subject_template,headers_template,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`);
  const filters = JSON.stringify([{ path: 'action', op: 'eq', value: 'push' }]);
  ruleIns.run('构建结果转发', 1, '[1,2]', '[3,4,5,6]', filters, '',
    '[{{.Payload.repository.full_name}}] {{.Payload.action}}', '{}', now - 86400 * 2, now - 86400 * 2);
  ruleIns.run('邮件告警转发', 1, '[2]', '[5]', '[]', '', '', '{}', now - 86400 * 2 + 60, now - 86400 * 2 + 60);

  const payload = JSON.stringify({
    action: 'push', ref: 'refs/heads/main',
    repository: { full_name: 'loarland/Forward2Any', private: false },
    pusher: { name: 'loarland' },
    commits: [{ id: '8c1f2ab', message: 'feat: 内置渠道支持加签', author: { name: 'loarland' } }],
  });
  const outcome = {
    3: ['success', 200, '', '{"ok":true}'],
    4: ['failed', 0, 'SMTP 连接超时（10s）', ''],
    5: ['success', 200, '', '{"ok":true,"result":{"message_id":4211}}'],
    6: ['failed', 0, 'dial tcp: i/o timeout', ''],
  };
  const delIns = db.prepare(`INSERT INTO deliveries (trace_id,rule_id,in_source_id,out_source_id,hop_chain,
    status,attempt,payload,rendered,subject,req_headers,response_code,response_body,last_error,next_retry_at,
    created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`);
  let id = 0;
  for (let round = 0; round < 3; round++) {
    for (const out of [3, 4, 5, 6]) {
      id++;
      const [status, code, err, body] = outcome[out];
      delIns.run((id.toString(16) + 'f2a').padEnd(16, '0'), 1, 1, out, 'github-push-a1b2', status, 1,
        payload, payload, '[loarland/Forward2Any] push', '{"Content-Type":"application/json"}',
        code, body, err, status === 'failed' ? now + 86400 * 365 : 0, stamp, stamp);
    }
  }
  db.prepare("UPDATE settings SET value='light' WHERE key='theme_mode'").run();
  db.close();
}

// ---------- CDP ----------

async function connectCdp() {
  const deadline = Date.now() + 10000;
  let target = null;
  while (Date.now() < deadline && !target) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`)).json();
      target = list.find((t) => t.type === 'page') || list[0];
    } catch (_) {
      await sleep(200);
    }
  }
  if (!target) {
    throw new Error('连不上 chromium 的调试端口');
  }
  const ws = new WebSocket(target.webSocketDebuggerUrl);
  let seq = 0;
  const pending = new Map();
  const events = [];
  ws.addEventListener('message', (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.id && pending.has(msg.id)) {
      const { resolve, reject } = pending.get(msg.id);
      pending.delete(msg.id);
      msg.error ? reject(new Error(JSON.stringify(msg.error))) : resolve(msg.result);
    } else if (msg.method) {
      events.push(msg.method);
    }
  });
  await new Promise((r) => ws.addEventListener('open', r));
  const send = (method, params) =>
    new Promise((resolve, reject) => {
      const id = ++seq;
      pending.set(id, { resolve, reject });
      ws.send(JSON.stringify({ id, method, params: params || {} }));
    });
  return { send, events, close: () => ws.close() };
}

async function login() {
  const body = new URLSearchParams({ username: 'admin', password: ADMIN_PASS });
  const r = await fetch(BASE + '/login', {
    method: 'POST',
    body,
    redirect: 'manual',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded' },
  });
  const cookie = (r.headers.getSetCookie ? r.headers.getSetCookie() : []).join(';');
  const m = /f2a_session=([^;]+)/.exec(cookie);
  if (!m) {
    throw new Error('登录失败，没拿到会话 cookie');
  }
  return m[1];
}

async function setTheme(token, mode) {
  const body = new URLSearchParams({
    admin_user: 'admin', base_url: 'https://hooks.example.com', theme_color: 'blue', theme_mode: mode,
  });
  const r = await fetch(BASE + '/settings', {
    method: 'POST',
    body,
    redirect: 'manual',
    headers: { 'Content-Type': 'application/x-www-form-urlencoded', Cookie: 'f2a_session=' + token },
  });
  if (r.status !== 303 && r.status !== 200) {
    throw new Error('切换主题失败：HTTP ' + r.status);
  }
}

async function capture(cdp, token, shots) {
  const { send, events } = cdp;
  await send('Network.enable');
  await send('Page.enable');
  await send('Network.setCookie', {
    name: 'f2a_session', value: token, url: BASE + '/', path: '/', httpOnly: true, sameSite: 'Lax',
  });
  for (const s of shots) {
    events.length = 0;
    await send('Page.navigate', { url: BASE + s.path });
    for (let i = 0; i < 80 && !events.includes('Page.loadEventFired'); i++) {
      await sleep(50);
    }
    await sleep(400); // 让字体与样式落定
    const { data } = await send('Page.captureScreenshot', { format: 'png' });
    fs.writeFileSync(path.join(OUT_DIR, s.file), Buffer.from(data, 'base64'));
    log('  ' + s.file);
  }
}

// ---------- 主流程 ----------

(async () => {
  fs.mkdirSync(OUT_DIR, { recursive: true });
  const bin = argValue('--bin') || buildBinary();
  const dataDir = fs.mkdtempSync(path.join(os.tmpdir(), 'f2a-shots-'));
  const chrome = findChrome();
  let app = null;
  let browser = null;
  try {
    log('起一个临时实例（' + dataDir + '）');
    app = startApp(bin, dataDir, true);
    await waitHealthy(15000);
    app.kill();
    await sleep(500);

    log('灌演示数据');
    seed(dataDir);

    app = startApp(bin, dataDir, true);
    await waitHealthy(15000);

    log('启动 ' + path.basename(chrome));
    browser = spawn(chrome, ['--headless', '--no-sandbox', '--disable-gpu',
      `--remote-debugging-port=${CDP_PORT}`, `--window-size=${WIDTH},${HEIGHT}`, 'about:blank'],
      { stdio: 'ignore' });
    const cdp = await connectCdp();
    const token = await login();

    log('亮色：');
    await capture(cdp, token, [
      { path: '/', file: 'dashboard-light.png' },
      { path: '/sources', file: 'sources-light.png' },
      { path: '/rules', file: 'rules-light.png' },
      { path: '/deliveries', file: 'deliveries-light.png' },
    ]);

    log('暗色：');
    await setTheme(token, 'dark');
    await capture(cdp, token, [
      { path: '/', file: 'dashboard-dark.png' },
      { path: '/sources', file: 'sources-dark.png' },
    ]);
    cdp.close();

    log('完成，写在 ' + path.relative(ROOT, OUT_DIR) + '/');
  } finally {
    if (browser) {
      browser.kill();
    }
    if (app) {
      app.kill();
    }
    fs.rmSync(dataDir, { recursive: true, force: true });
  }
})().catch((e) => {
  console.error(e.message || e);
  process.exit(1);
});
