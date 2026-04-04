# resolvr

**Open-source SPA Subpath Router. Reverse proxy with automatic path rewriting for single-page applications behind subpath prefixes.**

Modern SPAs break when deployed behind subpath proxies — Open OnDemand, nginx location blocks, Kubernetes ingress, Azure App Service. The assets load from the wrong path, the client-side router 404s, and runtime API calls hit the domain root instead of the proxy prefix. Every proxy product in the industry either requires rebuilding the app or maintains a curated list of supported apps. resolvr fixes it transparently: one container, one env var, and any React/Vue/Angular/Svelte app works.

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

The standard answer is "set `base` in your build config and rebuild." That requires access to source code, a working build pipeline, and ongoing maintenance every time the subpath changes. resolvr handles all three walls at the proxy layer, with zero changes to the app.

---

## Quick Start

```bash
docker pull ghcr.io/sqoia-dev/resolvr:latest
docker run -e TARGET=127.0.0.1:3000 -e BASE_PATH=/my/subpath -p 8080:8080 \
  ghcr.io/sqoia-dev/resolvr:latest
```

Point your reverse proxy at `http://localhost:8080`. The app will work as if it were built for that subpath.

---

## How It Works

resolvr intercepts responses from the upstream app and applies a layered rewrite pipeline before serving them to the browser.

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

### 3. CSS `url()` rewriting

Absolute paths inside CSS `url()` values are rewritten to relative paths, covering all three quoting forms:

| Before | After |
|--------|-------|
| `url(/img/bg.png)` | `url(./img/bg.png)` |
| `url("/img/bg.png")` | `url("./img/bg.png")` |
| `url('/img/bg.png')` | `url('./img/bg.png')` |

### 4. JavaScript rewriting

Two bundler-specific rewrites run on every JS response:

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

Static asset paths and `/assets/` references embedded as strings in JS bundles are also rewritten to relative paths.

**PWA manifests** — `manifest.json` `start_url` and icon `src` fields are rewritten to relative paths.

### 5. Runtime interceptor script

A script injected immediately after `<head>` monkey-patches the browser's runtime APIs so that absolute paths emitted dynamically by JavaScript are prefixed with the correct subpath:

- **`fetch`** — handles both string URLs and `Request` objects
- **`XMLHttpRequest.open`** — patches the URL argument before the request is sent
- **`WebSocket`** — handles relative paths, same-origin `ws://` URLs, and patterns like `` `${protocol}//${window.location.host}/path` ``
- **`EventSource`** — patches the URL argument

The base path is stored in `sessionStorage` on first load, so SPA navigations that call `history.pushState` do not change what the interceptors use. Hard reloads after client-side navigation continue to use the original proxy subpath.

### 6. React Router integration

`pushState` and `replaceState` are patched so that any clean SPA path pushed by React Router (e.g. `/dashboard`) has the subpath prefix automatically prepended (e.g. `/my/subpath/dashboard`). A `popstate` capture listener strips the prefix before React Router reads `window.location.pathname`, so the router always sees clean paths. A `replaceState` call at `readyState === interactive` strips the subpath from the address bar at the moment React Router initialises — after all relative asset attributes have already been resolved — so the router sees `/` on first load without affecting asset resolution.

### 7. Click interceptor

An event listener in capture phase intercepts clicks on `<a href="/...">` elements that use raw absolute paths instead of the SPA's router component, converting them to `pushState` navigations with the correct subpath prepended.

### 8. MutationObserver

A `MutationObserver` watches `document.documentElement` for dynamically injected `<link>`, `<script>`, and `<img>` elements. Any element added after the initial parse with an absolute `src` or `href` attribute has the subpath prepended before the browser issues the request. This covers Vite's runtime stylesheet injection and similar patterns.

---

## Configuration

Each setting can be passed as a command-line flag or environment variable. When running in Docker, use `-e`.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--external-port` | `EXTERNAL_PORT` | `8080` | Port for inbound HTTP traffic (browsers, OOD, nginx, etc.) |
| `--target` | `TARGET` | **required** | Upstream address `host:port`. Must be set. |
| `--base-path` | `BASE_PATH` | — | Subpath prefix (e.g. `/rnode/gnode001/46681`). Injects `<base href>` and seeds `sessionStorage`. |
| `--rewrite-html` | `REWRITE_HTML` | `true` | Enable/disable all HTML/CSS/JS rewriting and script injection. Set to `false` to disable. |

**Port mapping:** The `-p` host mapping must match the configured port. The container listens on the port you configure.

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
  resolvr.sif
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
  ghcr.io/sqoia-dev/resolvr:latest
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
                name: resolvr
                port:
                  number: 8080
```

```bash
docker run \
  -e TARGET=127.0.0.1:3000 \
  -e BASE_PATH=/dashboard \
  -p 8080:8080 \
  ghcr.io/sqoia-dev/resolvr:latest
```

### Run without Docker

```bash
./resolvr --target 127.0.0.1:3000 --base-path /myapp --external-port 8080
```

### Custom port

```bash
docker run \
  -e EXTERNAL_PORT=9000 \
  -e TARGET=127.0.0.1:3000 \
  -p 9000:9000 \
  ghcr.io/sqoia-dev/resolvr:latest
```

---

## Supported Frameworks

| Framework / Bundler | Support | Mechanism |
|---------------------|---------|-----------|
| Vite (React, Vue, Svelte) | Full | `assetsURL` regex replacement + `<base href>` injection |
| webpack (CRA, Vue CLI) | Full | `__webpack_require__.p` replacement + `<base href>` injection |
| Angular | Full | `<base href>` (Angular's native base-href mechanism) |
| Next.js static export | Partial | webpack rewrite; `/_next/` prefix is preserved |
| Any SPA with `base: '/'` | Full | Generic HTML/CSS/JS rewriting + runtime interceptors |
| Apps with WebSocket connections | Full | WebSocket constructor patch handles relative and same-origin `ws://` URLs |
| Apps using EventSource (SSE) | Full | EventSource constructor patch |
| Apps with PWA manifests | Full | `manifest.json` `start_url` and icon paths rewritten |

---

## Architecture

```
Browser
  │
  ▼
Reverse Proxy (OOD / nginx / K8s ingress)
  │
  ▼  HTTP on EXTERNAL_PORT (default 8080)
resolvr
  │
  Rewrites HTML responses:
    • Injects <base href="BASE_PATH/">
    • Rewrites src/href/action attributes  ("/..." → "./...")
    • Rewrites CSS url() paths
    • Patches Vite assetsURL + webpack __webpack_require__.p
    • Injects fetch / XHR / WebSocket / EventSource interceptors
    • Injects pushState / replaceState / popstate patches
    • Injects MutationObserver for dynamic DOM elements
    • Injects click interceptor for raw <a href> navigations
  Passes WebSocket upgrades as raw TCP pipes
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

## Build from Source

```bash
git clone https://github.com/sqoia-dev/resolvr.git
cd resolvr
go build -o resolvr .
```

No external dependencies — pure Go standard library.

---

## Cloud Alternative

resolvr is designed for local development and on-premise deployments behind private proxies. For cloud-hosted tunnels with HTTPS, traffic inspection, request replay, and persistent session history, see the [tunnl](https://github.com/sqoia-dev/tunnl) project.

---

Built by [Sqoia Labs](https://sqoia.dev)
