import { h } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { auth } from '../api.js';
import { card, tabs, codeBlock, spinner, button } from '../ui.js';
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
  const sn = snippets();
  const tab = docsTab(ctx.params);
  el.append(h('div', { class: 'tabs-head tabs-page', role: 'tablist' }, TABS.map((id) => h('a', {
    class: `tab ${id === tab ? 'active' : ''}`, role: 'tab', href: `#/docs?tab=${id}`, 'aria-selected': id === tab ? 'true' : 'false',
  }, t(id === 'client' ? 'nav.docsClient' : 'nav.docsAdmin')))));
  return tab === 'admin' ? renderAdmin(el, ctx, sn) : renderClient(el, ctx, sn);
}

function renderClient(el, ctx, sn) {
  ctx.setActions(h('a', { class: 'btn', href: '#/tokens?new=1' }, icon('plus', 15), h('span', null, t('docs.createToken'))));
  const getToken = h('div', { class: 'callout callout-info' }, icon('ticket', 16),
    h('span', null, h('strong', null, t('docs.getToken')), ' ', t('docs.getTokenDesc'), ' ', h('a', { class: 'link', href: '#/tokens' }, t('docs.goTokens'))));
  const usage = card({
    title: t('docs.usingProxy'), subtitle: t('docs.usingProxySub'),
    body: h('div', { class: 'stack' },
      h('div', { class: 'base-url-row' },
        h('span', { class: 'muted small' }, t('docs.baseUrl')),
        h('code', { class: 'mono inline-code' }, `${location.origin}/v1`),
        h('span', { class: 'muted small' }, t('docs.authWith')),
        h('code', { class: 'mono inline-code nowrap-code' }, 'Authorization: Bearer sk-xr-YOUR_TOKEN')),
      tabs([
        { id: 'chat-py', label: t('docs.chatPy'), render: () => codeBlock(sn.chat.python) },
        { id: 'chat-node', label: t('docs.chatNode'), render: () => codeBlock(sn.chat.node) },
        { id: 'chat-curl', label: t('docs.chatCurl'), render: () => codeBlock(sn.chat.curl) },
        { id: 'anth-py', label: t('docs.anthPy'), render: () => h('div', { class: 'stack-sm' }, h('p', { class: 'muted small docs-note' }, t('docs.anthNote')), codeBlock(sn.anthropic.python)) },
        { id: 'anth-node', label: t('docs.anthNode'), render: () => h('div', { class: 'stack-sm' }, h('p', { class: 'muted small docs-note' }, t('docs.anthNote')), codeBlock(sn.anthropic.node)) },
        { id: 'anth-curl', label: t('docs.anthCurl'), render: () => codeBlock(sn.anthropic.curl) },
        { id: 'resp-py', label: t('docs.respPy'), render: () => codeBlock(sn.responses.python) },
        { id: 'resp-curl', label: t('docs.respCurl'), render: () => codeBlock(sn.responses.curl) },
      ], { cls: 'tabs-sm' })),
  });
  el.append(getToken, usage);
}

function renderAdmin(el, ctx, sn) {
  ctx.setActions(
    h('a', { class: 'btn', href: './openapi.yaml', target: '_blank', rel: 'noopener' }, icon('download', 15), h('span', null, 'openapi.yaml')),
  );
  const authCard = card({
    title: h('span', { class: 'card-title-ic' }, icon('shield', 16), t('docs.adminAuth')),
    subtitle: [t('docs.adminAuthDesc'), ' ', h('a', { class: 'link', href: '#/access' }, t('docs.goAccess'))],
    body: codeBlock(sn.admin),
  });

  const host = h('div', { class: 'swagger-host' });
  const status = h('div', { class: 'swagger-status' }, spinner(18), h('span', { class: 'muted' }, t('docs.loadingRef')));
  const refCard = card({ title: t('docs.reference'), subtitle: t('docs.referenceSub', { v: SWAGGER_VERSION }), body: [status, host], cls: 'card-swagger' });
  el.append(authCard, refCard);

  const applyTheme = () => host.classList.toggle('swagger-dark', effectiveTheme() === 'dark');
  applyTheme();
  document.addEventListener('xr:themechange', applyTheme);

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
    });
  }).catch((e) => {
    status.replaceChildren(icon('alert', 18), h('span', null, `${t('docs.loadFailed')} — ${e.message}`), button(t('common.retry'), { size: 'sm', onClick: () => ctx.navigate('#/docs?tab=admin&r=' + Date.now()) }));
  });

  return () => document.removeEventListener('xr:themechange', applyTheme);
}
