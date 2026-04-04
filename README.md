# adaptr

**Serve any SPA at any subpath. No rebuild required.**

Open-source SPA Subpath Adapter — transparent path adapter for single-page applications behind subpath prefixes.

Modern SPAs break when deployed behind subpath proxies — Open OnDemand, nginx location blocks, Kubernetes ingress, Azure App Service. The assets load from the wrong path, the client-side router 404s, and runtime API calls hit the domain root instead of the proxy prefix. Every tool in the industry either requires rebuilding the app or maintains a curated list of supported apps. adaptr fixes it transparently: one container, one env var, and any React/Vue/Angular/Svelte app works.

---

## The Problem

SPAs built with the default `base: '/'` embed absolute paths throughout their output:

```html
<script src="/assets/index.js"></script>
<link href="/assets/style.css" rel="stylesheet">
```

When your reverse proxy serves that app at `/myapp/`, the browser requests `/assets/index.js` — against the domain root — and gets a 404. Even if you fix the HTML, the client-side router still reads `window.location.pathname` as `/myapp/something` and 404s. Even if you fix the router, `fetch('/api/data')` still goes to the wrong place.

There are three walls:

1. **Absolute asset paths** — `src="/..."` resolves against the domain root, not the proxy subpath
2. **Client-side routing** — React Router / Vue Router initialise against the full URL including the subpath prefix, breaking all routes
3. **Runtime-constructed URLs** — `fetch('/api/...')`, `new WebSocket('/ws')`, `new EventSource('/events')` all bypass static rewrites entirely

The standard answer is "set `base` in your build config and rebuild." That requires access to source code, a working build pipeline, and ongoing maintenance every time the subpath changes. adaptr handles all three walls at the proxy layer, with zero changes to the app.

---

## Quick Start

```bash
docker pull ghcr.io/sqoia-dev/adaptr:latest
docker run -e TARGET=127.0.0.1:3000 -e BASE_PATH=/my/subpath -p 8080:8080 \
  ghcr.io/sqoia-dev/adaptr:latest
```

Point your reverse proxy at `http://localhost:8080`. The app will work as if it were built for that subpath.

---

## Verify It's Working

**1. Check startup logs**

adaptr logs its version, config, and per-request latency on startup:

```
adaptr v0.4.0 (commit: abc1234)
adaptr GET / → 200 (12ms)
adaptr GET /assets/index.js → 200 (3ms)
```

If you see `[passthrough]` on a request, that path matched `PASSTHROUGH_PATHS` and was not rewritten.

**2. Health check**

```bash
curl http://localhost:8080/health
# {"status":"ok"}
```

**3. Verify `<base href>` injection**

Open the browser DevTools (Cmd+U / Ctrl+U) and search the page source for `<base`. You should see:

```html
<base href="/my/subpath/">
```

This must appear as the first child of `<head>` before any other elements.

**4. Verify asset loading**

In the browser Network tab, confirm that JS and CSS assets are loading with 200 status codes. If you see 404s on `/assets/...` paths, check that `TARGET` is correct and reachable.

---

## How It Works

adaptr intercepts responses from the upstream app and applies a layered rewrite pipeline before serving them to the browser.

### 1. Server-side `<base href>` injection

A `<base href="BASE_PATH/">` tag is injected as the first child of `<head>`. This locks `document.baseURI` before the browser parses any other element. All relative asset URLs — including those already rewritten in steps 2 and 3 — resolve correctly without any further logic.

### 2. HTML attribute rewriting

`src`, `href`, and `action` attributes starting with an absolute path are rewritten to relative paths before the response is sent:

| Before | After |
|--------|-------|
| `src="/assets/main.js"` | `src="./assets/main.js"` |
| `href="/assets/style.css"` | `href="./assets/style.css"` |
| `action="/submit"` | `action="./submit"` |

Protocol-relative URLs (`//cdn.example.com/...`) are left untouched.

### 3. CSS `url()` and `@import` rewriting

Absolute paths inside CSS `url()` values are rewritten to relative paths, covering all three quoting forms:

| Before | After |
|--------|-------|
| `url(/img/bg.png)` | `url(./img/bg.png)` |
| `url("/img/bg.png")` | `url("./img/bg.png")` |
| `url('/img/bg.png')` | `url('./img/bg.png')` |

CSS `@import` string form is also rewritten:

| Before | After |
|--------|-------|
| `@import "/theme.css"` | `@import "./theme.css"` |
| `@import '/theme.css'` | `@import './theme.css'` |

### 4. JavaScript rewriting

**Framework detection** — adaptr inspects each JS response to detect the bundler in use (Vite, webpack, or SystemJS). Framework-specific rewrites are gated on the detected framework, avoiding false-positive rewrites in unrelated files.

**Vite** — The `assetsURL` function that Vite emits for dynamic imports is replaced with a version that resolves relative to `import.meta.url` (the module's actual load URL, which carries the correct subpath) rather than the domain root:

| Before | After |
|--------|-------|
| `const assetsURL=function(e){return"/"+e}` | `const assetsURL=function(dep,importerUrl){return new URL(dep,importerUrl).href}` |
| `assetsURL=e=>"/"+e` | `assetsURL=function(dep,importerUrl){return new URL(dep,importerUrl).href}` |

**webpack** — The public path runtime variable is rewritten so chunk loads resolve relative to the current document:

| Before | After |
|--------|-------|
| `__webpack_require__.p="/"` | `__webpack_require__.p="./"` |
| `__webpack_public_path__ = "/"` | `__webpack_public_path__="./"` |

Only the exact bare-root value `"/"` is rewritten. Prefixes like `"/_next/"` are left untouched.

**Sourcemap URLs** — `//# sourceMappingURL=/...` references in JS files and `/*# sourceMappingURL=/...` in CSS files are rewritten to relative paths so DevTools source maps load correctly:

| Before | After |
|--------|-------|
| `//# sourceMappingURL=/assets/main.js.map` | `//# sourceMappingURL=./assets/main.js.map` |

**Import maps** — `<script type="importmap">` JSON blocks in HTML are parsed and all absolute path values are rewritten to relative paths.

**Service Worker registrations** — `navigator.serviceWorker.register` calls are intercepted and patched so that Service Worker scope and script URL resolve to the correct subpath. adaptr also registers its own Service Worker at `<basePath>/sw.js` for full-page navigation coverage.

Static asset paths and `/assets/` references embedded as strings in JS bundles are also rewritten to relative paths.

### 5. PWA manifest.json rewriting

All path fields in `manifest.json` are rewritten to relative paths, including `start_url`, `scope`, `icons[].src`, `shortcuts[].url`, and any other URL fields. This ensures PWA install and launch work correctly behind a subpath proxy.

### 6. Gzip re-compression

When the upstream serves gzip-compressed responses, adaptr decompresses, rewrites, and re-compresses the response before sending it to the browser. The `Content-Encoding: gzip` header is preserved. Brotli (`br`) encoding is stripped from `Accept-Encoding` before forwarding to the upstream since v0.4.0, ensuring adaptr always receives a decompressible response.

### 7. Runtime interceptor script

A script injected immediately after `<head>` monkey-patches the browser's runtime APIs so that absolute paths emitted dynamically by JavaScript are prefixed with the correct subpath:

- **`fetch`** — handles both string URLs and `Request` objects
- **`XMLHttpRequest.open`** — patches the URL argument before the request is sent
- **`WebSocket`** — handles relative paths, same-origin `ws://` URLs, and patterns like `` `${protocol}//${window.location.host}/path` ``
- **`EventSource`** — patches the URL argument

The base path is stored in `sessionStorage` on first load, so SPA navigations that call `history.pushState` do not change what the interceptors use. Hard reloads after client-side navigation continue to use the original proxy subpath.

### 8. React Router integration

`pushState` and `replaceState` are patched so that any clean SPA path pushed by React Router (e.g. `/dashboard`) has the subpath prefix automatically prepended (e.g. `/my/subpath/dashboard`). A `popstate` capture listener strips the prefix before React Router reads `window.location.pathname`, so the router always sees clean paths. A `replaceState` call at `readyState === interactive` strips the subpath from the address bar at the moment React Router initialises — after all relative asset attributes have already been resolved — so the router sees `/` on first load without affecting asset resolution.

### 9. Click interceptor

An event listener in capture phase intercepts clicks on `<a href="/...">` elements that use raw absolute paths instead of the SPA's router component, converting them to `pushState` navigations with the correct subpath prepended.

### 10. MutationObserver

A `MutationObserver` watches `document.documentElement` for dynamically injected `<link>`, `<script>`, and `<img>` elements. Any element added after the initial parse with an absolute `src` or `href` attribute has the subpath prepended before the browser issues the request. This covers Vite's runtime stylesheet injection and similar patterns.

---

## Configuration

Each setting can be passed as a command-line flag or environment variable. When running in Docker, use `-e`.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--target` | `TARGET` | **required** | Upstream address `host:port`. Must be set. |
| `--external-port` | `EXTERNAL_PORT` | `8080` | Port for inbound HTTP traffic (browsers, OOD, nginx, etc.) |
| `--base-path` | `BASE_PATH` | — | Subpath prefix (e.g. `/rnode/gnode001/46681`). Only `[a-zA-Z0-9/_.-]` characters allowed. Injects `<base href>` and seeds `sessionStorage`. |
| `--rewrite-html` | `REWRITE_HTML` | `true` | Enable/disable all HTML/CSS/JS rewriting and script injection. Set to `false` to disable. |
| — | `PASSTHROUGH_PATHS` | — | Comma-separated path prefixes to skip rewriting (e.g. `/api,/static`). Requests to these paths are proxied unmodified. |
| — | `MAX_REWRITE_BODY_BYTES` | `10485760` | Maximum response body size in bytes for rewriting (default 10 MB). Responses larger than this are passed through unchanged. |

**Port mapping:** The `-p` host mapping must match the configured port. The container listens on the port you configure.

**BASE_PATH validation:** `BASE_PATH` is validated against `[a-zA-Z0-9/_.-]` on startup. Invalid characters are rejected with a fatal error to prevent XSS via `<base href>` injection.

---

## Use Cases

### Open OnDemand (HPC)

OOD generates a dynamic subpath for each interactive app session (`/rnode/<node>/<port>/`). The app never knows this path at build time.

```bash
# In your OOD app's wrapper script:
singularity run \
  -e EXTERNAL_PORT=$PORT \
  -e TARGET=127.0.0.1:$APP_PORT \
  -e BASE_PATH="/rnode/$(hostname)/$PORT" \
  adaptr.sif
```

### nginx subpath

```nginx
location /myapp/ {
    proxy_pass http://localhost:8080/;
    proxy_set_header Host $host;
}
```

```bash
docker run \
  -e TARGET=127.0.0.1:3000 \
  -e BASE_PATH=/myapp \
  -p 8080:8080 \
  ghcr.io/sqoia-dev/adaptr:latest
```

### Kubernetes Ingress

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
spec:
  rules:
    - http:
        paths:
          - path: /dashboard
            pathType: Prefix
            backend:
              service:
                name: adaptr
                port:
                  number: 8080
```

```bash
docker run \
  -e TARGET=127.0.0.1:3000 \
  -e BASE_PATH=/dashboard \
  -p 8080:8080 \
  ghcr.io/sqoia-dev/adaptr:latest
```

### Passthrough for API routes

If your app has API routes or static assets that should not be rewritten, exclude them with `PASSTHROUGH_PATHS`:

```bash
docker run \
  -e TARGET=127.0.0.1:3000 \
  -e BASE_PATH=/myapp \
  -e PASSTHROUGH_PATHS=/api,/static \
  -p 8080:8080 \
  ghcr.io/sqoia-dev/adaptr:latest
```

### Run without Docker

```bash
./adaptr --target 127.0.0.1:3000 --base-path /myapp --external-port 8080
```

### Custom port

```bash
docker run \
  -e EXTERNAL_PORT=9000 \
  -e TARGET=127.0.0.1:3000 \
  -p 9000:9000 \
  ghcr.io/sqoia-dev/adaptr:latest
```

---

## Kubernetes (Helm)

```bash
helm install my-adaptr oci://ghcr.io/sqoia-dev/charts/adaptr \
  --set config.target=127.0.0.1:3000 \
  --set config.basePath=/myapp
```

See `charts/adaptr/README.md` for full configuration reference.

---

## Supported Frameworks

| Framework / Bundler | Support | Mechanism |
|---------------------|---------|-----------|
| Vite (React, Vue, Svelte) | Full | `assetsURL` regex replacement + `<base href>` injection |
| webpack (CRA, Vue CLI) | Full | `__webpack_require__.p` replacement + `<base href>` injection |
| SystemJS | Full | Framework detection gates SystemJS-specific rewrites |
| Angular | Full | `<base href>` (Angular's native base-href mechanism) |
| Next.js static export | Partial | webpack rewrite; `/_next/` prefix is preserved |
| Any SPA with `base: '/'` | Full | Generic HTML/CSS/JS rewriting + runtime interceptors |
| Apps with WebSocket connections | Full | WebSocket constructor patch handles relative and same-origin `ws://` URLs |
| Apps using EventSource (SSE) | Full | EventSource constructor patch |
| Apps with PWA manifests | Full | All `manifest.json` path fields rewritten (start_url, scope, icons, shortcuts, etc.) |
| Apps with import maps | Full | `<script type="importmap">` JSON paths rewritten |
| Apps with Service Workers | Full | `navigator.serviceWorker.register` patched; adaptr SW registered at `<basePath>/sw.js` |

---

## Architecture

```
Browser
  │
  ▼
Reverse Proxy (OOD / nginx / K8s ingress)
  │
  ▼  HTTP on EXTERNAL_PORT (default 8080)
adaptr
  │
  Rewrites HTML responses:
    • Injects <base href="BASE_PATH/">
    • Rewrites src/href/action attributes  ("/..." → "./...")
    • Rewrites CSS url() paths and @import strings
    • Rewrites <script type="importmap"> JSON paths
    • Patches Vite assetsURL + webpack __webpack_require__.p
    • Rewrites sourcemap URLs (//# sourceMappingURL=)
    • Patches navigator.serviceWorker.register
    • Injects fetch / XHR / WebSocket / EventSource interceptors
    • Injects pushState / replaceState / popstate patches
    • Injects MutationObserver for dynamic DOM elements
    • Injects click interceptor for raw <a href> navigations
  Rewrites manifest.json path fields
  Rewrites CSS @import and sourcemap URLs
  Re-compresses rewritten gzip responses
  Detects framework (Vite/webpack/SystemJS) per request
  Skips rewriting for PASSTHROUGH_PATHS prefixes
  Skips rewriting for responses > MAX_REWRITE_BODY_BYTES
  Passes WebSocket upgrades as raw TCP pipes
  Logs per-request latency
  Handles SIGTERM with 30s graceful drain
  │
  ▼
Your app (localhost:APP_PORT)
```

Single server, single port. No external dependencies.

---

## Debug Endpoints

| Endpoint | Description |
|----------|-------------|
| `GET /health` | Returns `{"status":"ok"}` |

---

## Troubleshooting

**Assets 404 after proxying**

Check that `TARGET` is reachable from inside the container:

```bash
curl http://127.0.0.1:3000/
```

If `TARGET` is a host on the Docker bridge network, use the host IP rather than `127.0.0.1`.

**`<base href>` not injected**

- Confirm `REWRITE_HTML` is not set to `false`.
- Confirm the upstream response has `Content-Type: text/html`. adaptr only injects `<base href>` into HTML responses.
- Check the startup logs for the `BASE_PATH` value adaptr read.

**API calls fail or hit the wrong URL**

If your API routes should not be rewritten, add them to `PASSTHROUGH_PATHS`:

```bash
-e PASSTHROUGH_PATHS=/api,/graphql
```

Requests to those prefixes are proxied unmodified — no rewriting, no script injection.

**Large responses not rewritten**

If a JS or CSS bundle exceeds `MAX_REWRITE_BODY_BYTES` (default 10 MB), adaptr passes it through unmodified. The request log will show `[size-cap]`. Increase the limit if needed:

```bash
-e MAX_REWRITE_BODY_BYTES=20971520   # 20 MB
```

**App uses brotli compression**

Since v0.4.0, adaptr automatically strips `Accept-Encoding: br` before forwarding requests to the upstream. This ensures the upstream returns gzip or uncompressed responses that adaptr can decompress, rewrite, and re-compress. No configuration needed.

**Service Worker not registering**

adaptr registers its own Service Worker at `<basePath>/sw.js`. If your app also registers a Service Worker, the scope may conflict. Use `PASSTHROUGH_PATHS` to pass your app's SW path through unmodified, or set `REWRITE_HTML=false` to disable all injection and handle rewrites yourself.

---

## Build from Source

```bash
git clone https://github.com/sqoia-dev/adaptr.git
cd adaptr
go build -o adaptr .
```

No external dependencies — pure Go standard library.

---

## Cloud Alternative

adaptr is designed for local development and on-premise deployments behind private proxies. For cloud-hosted tunnels with HTTPS, traffic inspection, request replay, and persistent session history, see the [tunnl](https://github.com/sqoia-dev/tunnl) project.

---

Built by [Sqoia Labs](https://sqoia.dev)
