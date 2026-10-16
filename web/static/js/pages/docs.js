import { h } from '../util.js';
import { icon } from '../icons.js';
import { t } from '../i18n.js';
import { auth } from '../api.js';
import { card, tabs, codeBlock, spinner, button } from '../ui.js';
import { effectiveTheme } from '../theme.js';

// The only external resource in the console: Swagger UI, pinned, from jsDelivr.
const SWAGGER_VERSION = '5.17.14';
const CDN = `https://cdn.jsdelivr.net/npm/swagger-ui-dist@${SWAGGER_VERSION}`;

let swaggerPromise = null;
function loadSwagger() {
  if (window.SwaggerUIBundle) return Promise.resolve(window.SwaggerUIBundle);
  if (swaggerPromise) return swaggerPromise;
  swaggerPromise = new Promise((resolve, reject) => {
    if (!document.getElementById('swagger-css')) {
      document.head.appendChild(h('link', { id: 'swagger-css', rel: 'stylesheet', href: `${CDN}/swagger-ui.css`, crossorigin: 'anonymous' }));
    }
    const sc = h('script', { src: `${CDN}/swagger-ui-bundle.js`, crossorigin: 'anonymous' });
    sc.onload = () => resolve(window.SwaggerUIBundle);
    sc.onerror = () => { swaggerPromise = null; sc.remove(); reject(new Error('failed to load Swagger UI from cdn.jsdelivr.net')); };
    document.head.appendChild(sc);
  });
  return swaggerPromise;
}

function snippets() {
  const origin = location.origin;
  const base = `${origin}/v1`;
  const KEY = 'sk-xr-…';
  return {
    chat: {
      curl: `curl ${base}/chat/completions \\\n  -H "Authorization: Bearer ${KEY}" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "grok-4.3",\n    "messages": [{"role": "user", "content": "What is the meaning of life?"}],\n    "stream": false\n  }'`,
      python: `from openai import OpenAI\n\nclient = OpenAI(base_url="${base}", api_key="${KEY}")\n\n# Non-streaming\nresp = client.chat.completions.create(\n    model="grok-4.3",\n    messages=[{"role": "user", "content": "What is the meaning of life?"}],\n)\nprint(resp.choices[0].message.content)\n\n# Streaming\nstream = client.chat.completions.create(\n    model="grok-4.3",\n    messages=[{"role": "user", "content": "Write a haiku about routers."}],\n    stream=True,\n)\nfor chunk in stream:\n    if chunk.choices and chunk.choices[0].delta.content:\n        print(chunk.choices[0].delta.content, end="", flush=True)`,
    },
    responses: {
      curl: `curl ${base}/responses \\\n  -H "Authorization: Bearer ${KEY}" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "grok-4.3",\n    "input": "Summarize the plot of Hamlet in two sentences."\n  }'`,
      python: `from openai import OpenAI\n\nclient = OpenAI(base_url="${base}", api_key="${KEY}")\n\nresp = client.responses.create(\n    model="grok-4.3",\n    input="Summarize the plot of Hamlet in two sentences.",\n)\nprint(resp.output_text)`,
    },
    admin: `curl ${origin}/api/v1/stats/overview?range=24h \\\n  -H "Authorization: Bearer xra-…"`,
  };
}

export function render(el, ctx) {
  const sn = snippets();
  ctx.setActions(
    h('a', { class: 'btn', href: './openapi.yaml', target: '_blank', rel: 'noopener' }, icon('download', 15), h('span', null, 'openapi.yaml')),
  );

  const usage = card({
    title: t('docs.usingProxy'), subtitle: t('docs.usingProxySub'),
    body: h('div', { class: 'stack' },
      h('div', { class: 'base-url-row' },
        h('span', { class: 'muted small' }, t('docs.baseUrl')),
        h('code', { class: 'mono inline-code' }, `${location.origin}/v1`),
        h('span', { class: 'muted small' }, t('docs.authWith')),
        h('code', { class: 'mono inline-code' }, 'Authorization: Bearer sk-xr-…'),
        h('a', { class: 'link small', href: '#/tokens' }, t('docs.createToken'))),
      tabs([
        { id: 'chat-py', label: t('docs.chatPy'), render: () => codeBlock(sn.chat.python) },
        { id: 'chat-curl', label: t('docs.chatCurl'), render: () => codeBlock(sn.chat.curl) },
        { id: 'resp-py', label: t('docs.respPy'), render: () => codeBlock(sn.responses.python) },
        { id: 'resp-curl', label: t('docs.respCurl'), render: () => codeBlock(sn.responses.curl) },
        { id: 'admin', label: t('docs.adminApi'), render: () => h('div', { class: 'stack-sm' }, h('p', { class: 'muted small' }, t('docs.adminApiDesc')), codeBlock(sn.admin)) },
      ], { cls: 'tabs-sm' })),
  });

  const host = h('div', { class: 'swagger-host' });
  const status = h('div', { class: 'swagger-status' }, spinner(18), h('span', { class: 'muted' }, t('docs.loadingRef')));
  const refCard = card({ title: t('docs.reference'), subtitle: t('docs.referenceSub', { v: SWAGGER_VERSION }), body: [status, host], cls: 'card-swagger' });
  el.append(usage, refCard);

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
    status.replaceChildren(icon('alert', 18), h('span', null, `${t('docs.loadFailed')} — ${e.message}`), button(t('common.retry'), { size: 'sm', onClick: () => ctx.navigate('#/docs?r=' + Date.now()) }));
  });

  return () => document.removeEventListener('xr:themechange', applyTheme);
}
