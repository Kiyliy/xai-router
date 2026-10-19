import { h } from '../util.js';
import { icon } from '../icons.js';
import { t, getLang } from '../i18n.js';
import { auth } from '../api.js';
import { codeBlock, copyButton, spinner, button } from '../ui.js';
import { effectiveTheme } from '../theme.js';

// The only external resource in the console: Swagger UI, pinned, from jsDelivr.
const SWAGGER_VERSION = '5.17.14';
// Vendored copy of swagger-ui-dist (see web/static/vendor/swagger-ui).
const CDN = './vendor/swagger-ui';

let swaggerPromise = null;
function loadSwagger() {
  if (window.SwaggerUIBundle) return Promise.resolve(window.SwaggerUIBundle);
  if (swaggerPromise) return swaggerPromise;
  swaggerPromise = new Promise((resolve, reject) => {
    if (!document.getElementById('swagger-css')) {
      document.head.appendChild(h('link', { id: 'swagger-css', rel: 'stylesheet', href: `${CDN}/swagger-ui.css` }));
    }
    const sc = h('script', { src: `${CDN}/swagger-ui-bundle.js` });
    sc.onload = () => resolve(window.SwaggerUIBundle);
    sc.onerror = () => { swaggerPromise = null; sc.remove(); reject(new Error('failed to load Swagger UI')); };
    document.head.appendChild(sc);
  });
  return swaggerPromise;
}

// Chinese labels for the admin API reference: tag names and operation summaries (the
// list view). Applied to the YAML text before Swagger UI parses it; longer field
// descriptions stay as written in openapi.yaml.
const ZH_TAGS = {
  Auth: '认证', Keys: '上游密钥', Tokens: '客户端令牌', 'Admin tokens': '管理 API 令牌', Models: '模型与价格',
  Logs: '日志', Stats: '统计', Settings: '设置', System: '系统',
};
const ZH_TAG_DESC = {
  'Upstream xAI API keys (the key pool).': '',
  'Client tokens used by downstream applications to call `/v1/*`.': '',
  'Long-lived tokens for scripting this admin API.': '',
  'Model catalogue and price table.': '',
  'Detailed per-request logs.': '',
};
const ZH_SUMMARY = {
  'Log in with the admin password': '用管理员密码登录',
  'Revoke the current session': '退出当前会话',
  'Describe the current credential': '查看当前凭据',
  'Change the admin password': '修改管理员密码',
  'List upstream keys': '列出上游密钥（分页、状态筛选）',
  'Add a key': '添加上游密钥',
  'Bulk import keys, optionally testing each one': '批量导入上游密钥，可以逐个检测',
  'Test many keys': '批量检测上游密钥',
  'Get a key': '查看一个上游密钥',
  'Update a key (partial)': '修改名称、优先级、权重、并发和每分钟请求上限',
  'Delete a key': '删除',
  'Test one key': '快速检测（不消耗额度）',
  'Clear cooldown / auto-disable state and re-enable the key': '恢复上游密钥',
  'List client tokens': '列出客户端令牌',
  'Create a client token': '创建客户端令牌',
  'Get a client token': '查看一个客户端令牌',
  'Update a client token (partial)': '修改客户端令牌',
  'Delete a client token': '删除客户端令牌',
  'Replace the secret of a client token': '更换令牌值',
  "Adjust a client token's quota": '调整额度',
  "A token's quota records, newest first": '额度记录（最新的在前）',
  'List admin API tokens': '列出管理 API 令牌',
  'Create an admin API token': '创建管理 API 令牌',
  'Revoke an admin API token': '吊销管理 API 令牌',
  'List models and prices': '列出模型和价格',
  'Add or replace a model manually': '手动添加或替换模型',
  'Sync models and official prices from xAI now': '立即从 xAI 同步模型和官方价格',
  'Update a model (partial)': '修改模型',
  'Delete a model from the price table': '从价格表删除模型',
  'The log (requests, audit and system events)': '日志（请求、操作和系统记录）',
  'Delete logs older than a time': '删除早于某个时间的请求日志',
  'Get one log with request/response bodies and all upstream attempts': '查看一条日志，包括请求和响应内容、全部上游尝试',
  'Reproduce a request as a curl command': '把请求还原成 curl 命令',
  'Totals for a time range (client-request based)': '时间范围内的合计',
  'Requests, tokens and cost over time': '请求数、Token 数和费用随时间的变化',
  'Per-model / per-key / per-token totals': '按模型、上游密钥、客户端令牌的合计',
  'Read runtime settings': '读取设置',
  'Update runtime settings (partial). Takes effect immediately.': '修改设置（立即生效）',
  'Reclaim free space in the database file (background job)': '压缩数据库',
  'Recent background jobs (newest first, kept in memory)': '最近的后台任务',
  'Get a background job': '查看一个后台任务',
  'One audit or system event': '查看一条操作或系统记录',
  'Version and runtime information': '版本和运行信息',
  'Liveness probe': '存活检查',
};
function localizeSpec(text) {
  if (getLang() !== 'zh' || typeof text !== 'string') return text;
  return text.split('\n').map((line) => {
    let m = line.match(/^(\s*summary: )(.+)$/);
    if (m && ZH_SUMMARY[m[2].trim()]) return m[1] + ZH_SUMMARY[m[2].trim()];
    m = line.match(/^(\s*tags: \[)(.+)(\]\s*)$/);
    if (m) return m[1] + m[2].split(',').map((x) => ZH_TAGS[x.trim()] || x.trim()).join(', ') + m[3];
    m = line.match(/^(\s*- name: )(.+)$/);
    if (m && ZH_TAGS[m[2].trim()]) return m[1] + ZH_TAGS[m[2].trim()];
    m = line.match(/^(\s*description: )(.+)$/);
    if (m && m[2].trim() in ZH_TAG_DESC) return `${m[1]}""`;
    return line;
  }).join('\n');
}

function snippets() {
  const origin = location.origin;
  const base = `${origin}/v1`;
  // A placeholder that reads as a placeholder (no ellipsis that looks like truncation).
  const KEY = 'sk-xr-YOUR_TOKEN';
  return {
    chat: {
      curl: `curl ${base}/chat/completions \\\n  -H "Authorization: Bearer ${KEY}" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "grok-4.3",\n    "messages": [{"role": "user", "content": "What is the meaning of life?"}],\n    "stream": false\n  }'`,
      python: `from openai import OpenAI\n\nclient = OpenAI(base_url="${base}", api_key="${KEY}")\n\n# Non-streaming\nresp = client.chat.completions.create(\n    model="grok-4.3",\n    messages=[{"role": "user", "content": "What is the meaning of life?"}],\n)\nprint(resp.choices[0].message.content)\n\n# Streaming\nstream = client.chat.completions.create(\n    model="grok-4.3",\n    messages=[{"role": "user", "content": "Write a haiku about routers."}],\n    stream=True,\n)\nfor chunk in stream:\n    if chunk.choices and chunk.choices[0].delta.content:\n        print(chunk.choices[0].delta.content, end="", flush=True)`,
      node: `import OpenAI from "openai";\n\nconst client = new OpenAI({ baseURL: "${base}", apiKey: process.env.XR_API_KEY ?? "${KEY}" });\n\n// Non-streaming\nconst resp = await client.chat.completions.create({\n  model: "grok-4.3",\n  messages: [{ role: "user", content: "What is the meaning of life?" }],\n});\nconsole.log(resp.choices[0].message.content);\n\n// Streaming\nconst stream = await client.chat.completions.create({\n  model: "grok-4.3",\n  messages: [{ role: "user", content: "Write a haiku about routers." }],\n  stream: true,\n});\nfor await (const chunk of stream) {\n  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");\n}`,
    },
    anthropic: {
      python: `import anthropic\n\n# The Anthropic SDK appends /v1/messages itself, so use the bare origin.\nclient = anthropic.Anthropic(base_url="${origin}", api_key="${KEY}")\n\nmsg = client.messages.create(\n    model="grok-4.3",\n    max_tokens=1024,\n    messages=[{"role": "user", "content": "What is the meaning of life?"}],\n)\nprint(msg.content[0].text)\n\n# Streaming\nwith client.messages.stream(\n    model="grok-4.3",\n    max_tokens=1024,\n    messages=[{"role": "user", "content": "Write a haiku about routers."}],\n) as stream:\n    for text in stream.text_stream:\n        print(text, end="", flush=True)`,
      node: `import Anthropic from "@anthropic-ai/sdk";\n\n// The Anthropic SDK appends /v1/messages itself, so use the bare origin.\nconst client = new Anthropic({ baseURL: "${origin}", apiKey: process.env.XR_API_KEY ?? "${KEY}" });\n\nconst msg = await client.messages.create({\n  model: "grok-4.3",\n  max_tokens: 1024,\n  messages: [{ role: "user", content: "What is the meaning of life?" }],\n});\nconsole.log(msg.content[0].text);`,
      curl: `curl ${base}/messages \\\n  -H "x-api-key: ${KEY}" \\\n  -H "anthropic-version: 2023-06-01" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "grok-4.3",\n    "max_tokens": 1024,\n    "messages": [{"role": "user", "content": "What is the meaning of life?"}]\n  }'`,
    },
    responses: {
      curl: `curl ${base}/responses \\\n  -H "Authorization: Bearer ${KEY}" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "grok-4.3",\n    "input": "Summarize the plot of Hamlet in two sentences."\n  }'`,
      python: `from openai import OpenAI\n\nclient = OpenAI(base_url="${base}", api_key="${KEY}")\n\nresp = client.responses.create(\n    model="grok-4.3",\n    input="Summarize the plot of Hamlet in two sentences.",\n)\nprint(resp.output_text)`,
    },
    admin: `curl ${origin}/api/v1/stats/overview?range=24h \\\n  -H "Authorization: Bearer xra-…"`,
  };
}

export const TABS = ['client', 'admin'];
export const docsTab = (params) => (TABS.includes(params && params.tab) ? params.tab : 'client');

export function render(el, ctx) {
  el.classList.add('pg-docs');
  const sn = snippets();
  const tab = docsTab(ctx.params);
  el.append(h('div', { class: 'docs-tabs', role: 'tablist' }, TABS.map((id) => h('a', {
    class: `docs-tab ${id === tab ? 'active' : ''}`, role: 'tab', href: `#/docs?tab=${id}`, 'aria-selected': id === tab ? 'true' : 'false',
  }, t(id === 'client' ? 'nav.docsClient' : 'nav.docsAdmin')))));
  return tab === 'admin' ? renderAdmin(el, ctx, sn) : renderClient(el, ctx, sn);
}

// Code card with language tabs and one copy button (frame 70-3601, step 3).
function codeTabs(items) {
  let cur = items[0];
  const pre = h('pre', { class: 'code' });
  const head = h('div', { class: 'docs-code-tabs', role: 'tablist' });
  const paint = () => {
    head.replaceChildren(...items.map((it) => h('button', {
      type: 'button', role: 'tab', class: `docs-code-tab ${it === cur ? 'active' : ''}`, 'aria-selected': it === cur ? 'true' : 'false',
      onclick: () => { cur = it; paint(); },
    }, it.label)));
    pre.replaceChildren(h('code', null, cur.code));
  };
  paint();
  return h('div', { class: 'docs-code' }, h('div', { class: 'docs-code-head' }, head, copyButton(() => cur.code, { iconOnly: true })), pre);
}

function renderClient(el, ctx, sn) {
  const base = `${location.origin}/v1`;
  const step = (n, title, ...body) => h('li', { class: 'docs-step' }, h('span', { class: 'docs-step-n mono' }, String(n)),
    h('div', { class: 'docs-step-body' }, h('h2', null, title), ...body));
  const join = (...parts) => parts.filter(Boolean).join('\n\n');
  const steps = h('ol', { class: 'docs-steps' },
    step(1, t('docs.step1Title'), h('p', null, t('docs.step1Desc')), h('a', { class: 'docs-link', href: '#/tokens?new=1' }, t('docs.step1Link'))),
    step(2, t('docs.step2Title'), h('p', null, t('docs.step2Desc')),
      h('div', { class: 'docs-url' }, h('code', { class: 'mono' }, base), copyButton(base, { cls: 'btn-ghost' }))),
    step(3, t('docs.step3Title'), codeTabs([
      { label: 'Python', code: sn.chat.python },
      { label: 'Node.js', code: sn.chat.node },
      { label: 'curl', code: sn.chat.curl },
      { label: 'Anthropic SDK', code: join(`# ${t('docs.anthNote')}`, sn.anthropic.python, `// Node.js\n${sn.anthropic.node}`, `# curl\n${sn.anthropic.curl}`) },
      { label: 'Responses', code: join(sn.responses.python, `# curl\n${sn.responses.curl}`) },
    ])));
  const ep = (m, path, label) => h('div', { class: 'docs-ep' }, h('span', { class: `docs-method m-${m.toLowerCase()}` }, m), h('span', { class: 'mono docs-path' }, path), h('span', { class: 'docs-ep-l' }, label));
  const side = h('aside', { class: 'docs-side' },
    h('div', { class: 'docs-side-title' }, t('docs.endpoints')),
    ep('POST', '/v1/chat/completions', 'OpenAI Chat'),
    ep('POST', '/v1/responses', 'OpenAI Responses'),
    ep('POST', '/v1/messages', 'Anthropic Messages'),
    ep('POST', '/v1/images/generations', t('docs.ep.images')),
    ep('POST', '/v1/videos/generations', t('docs.ep.videos')),
    ep('GET', '/v1/models', t('docs.ep.models')),
    h('div', { class: 'docs-note-box' }, icon('info', 14), h('span', null, t('docs.gatewayNote'))));
  el.append(h('div', { class: 'docs-guide' }, steps, side));
}

function renderAdmin(el, ctx, sn) {
  const authBar = h('div', { class: 'docs-auth' }, icon('shield', 15),
    h('span', null, t('docs.authPre'), ' ', h('code', { class: 'mono' }, 'Authorization: Bearer xra-…'), t('docs.authPost')),
    h('a', { class: 'docs-yaml mono', href: './openapi.yaml', target: '_blank', rel: 'noopener' }, 'openapi.yaml', icon('download', 14)));
  const example = h('details', { class: 'docs-example' }, h('summary', null, t('docs.example')), codeBlock(sn.admin));
  const nav = h('nav', { class: 'docs-ref-nav', 'aria-label': t('docs.sections') });
  const host = h('div', { class: 'swagger-host' });
  const status = h('div', { class: 'swagger-status' }, spinner(18), h('span', { class: 'muted' }, t('docs.loadingRef')));
  el.append(h('div', { class: 'docs-ref' }, nav, h('div', { class: 'docs-ref-main' }, authBar, example, status, host)));

  const applyTheme = () => host.classList.toggle('swagger-dark', effectiveTheme() === 'dark');
  applyTheme();
  document.addEventListener('xr:themechange', applyTheme);

  // Left list of API groups, built from the rendered Swagger tag sections.
  function buildNav(tries = 30) {
    const secs = [...host.querySelectorAll('.opblock-tag-section')];
    if (!secs.length) { if (tries > 0 && ctx.alive()) setTimeout(() => buildNav(tries - 1), 200); return; }
    nav.replaceChildren(...secs.map((s) => {
      const tag = s.querySelector('.opblock-tag');
      const name = (tag && (tag.getAttribute('data-tag') || tag.textContent) || '').trim();
      return h('a', { class: 'docs-ref-item', href: '#/docs?tab=admin', onclick: (e) => {
        e.preventDefault();
        nav.querySelectorAll('.docs-ref-item').forEach((x) => x.classList.toggle('active', x === e.currentTarget));
        s.scrollIntoView({ behavior: 'smooth', block: 'start' });
      } }, name);
    }));
  }

  loadSwagger().then((SwaggerUIBundle) => {
    if (!ctx.alive()) return;
    status.remove();
    SwaggerUIBundle({
      url: './openapi.yaml',
      domNode: host,
      deepLinking: false,
      docExpansion: 'list',
      defaultModelsExpandDepth: 0,
      tryItOutEnabled: false,
      persistAuthorization: false,
      requestInterceptor: (req) => {
        const tok = auth.token;
        if (tok && !req.headers.Authorization) req.headers.Authorization = `Bearer ${tok}`;
        return req;
      },
      responseInterceptor: (res) => {
        if (res && res.url && res.url.includes('openapi.yaml') && typeof res.text === 'string') { res.text = localizeSpec(res.text); res.data = res.text; }
        return res;
      },
      onComplete: () => buildNav(),
    });
  }).catch((e) => {
    status.replaceChildren(icon('alert', 18), h('span', null, `${t('docs.loadFailed')} — ${e.message}`), button(t('common.retry'), { size: 'sm', onClick: () => ctx.navigate('#/docs?tab=admin&r=' + Date.now()) }));
  });

  return () => document.removeEventListener('xr:themechange', applyTheme);
}
