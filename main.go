// adaptr: SPA Subpath Adapter — transparent path adapter for single-page
// applications behind subpath prefixes.
//
// Listens on a single port (EXTERNAL_PORT, default 8080). All requests are
// proxied to the upstream TARGET address. WebSocket upgrades are
// passed through as raw TCP pipes. HTML, CSS, JS, and manifest responses are
// rewritten so that absolute asset paths resolve correctly when the app is
// served behind a subpath prefix.
//
// Usage:
//
//	adaptr --target 127.0.0.1:3000 --base-path /myapp --external-port 8080
//	docker run -e TARGET=127.0.0.1:3000 -e BASE_PATH=/myapp -p 8080:8080 adaptr
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// version is the version of adaptr.
const version = "0.4.0"

// gitCommit is the short git commit hash, injected at build time via -ldflags.
var gitCommit = "unknown"

// basePath is the OOD subpath prefix set via --base-path / BASE_PATH
// (e.g. "/rnode/gnode001.cluster/65038"). When non-empty, rewriteHTML injects
// <base href="basePath/"> as the first child of <head> so that the browser
// resolves all relative asset URLs against the correct base from the start.
// Package-level so injectAfterHead can access it without threading it through
// every call site.
var basePath string

// passthroughPaths is the list of path prefixes that should be passed through
// without any rewriting. Set via PASSTHROUGH_PATHS env var (comma-separated).
var passthroughPaths []string

// maxRewriteBodyBytes is the maximum response body size (in bytes) that will be
// read for rewriting. Responses larger than this are passed through unmodified.
// Set via MAX_REWRITE_BODY_BYTES env var (default 10MB).
var maxRewriteBodyBytes int64 = 10 * 1024 * 1024

// basePathRe is the allowlist regex for BASE_PATH validation.
var basePathRe = regexp.MustCompile(`^[a-zA-Z0-9/_.-]+$`)

// startupInjection is the injection string built once at startup and reused for
// every HTML response. It is set in runDirectProxy after basePath is finalised.
// Building it once avoids repeated string concatenation per request.
var startupInjection string

// gzipWriterPool pools gzip.Writer instances to avoid per-request allocation on
// the gzip re-compression path.
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(nil) },
}

// assetsURLRe matches Vite's absolute-base assetsURL function (regular function form).
// Vite emits this when base: "/" (the default):
//
//	const assetsURL=function(e){return"/"+e}
//
// The regex handles minified and whitespace-expanded variants, any single-letter
// parameter name, and any base path string (not just "/").
var assetsURLRe = regexp.MustCompile(
	`const\s+assetsURL\s*=\s*function\s*\([a-z]\)\s*\{\s*return\s*"[^"]*"\s*\+\s*[a-z]\s*\}`,
)

// assetsURLArrowRe matches the arrow-function variant some Vite versions emit:
//
//	assetsURL=e=>"/"+e
var assetsURLArrowRe = regexp.MustCompile(
	`assetsURL\s*=\s*[a-z]\s*=>\s*"[^"]*"\s*\+\s*[a-z]`,
)

// assetsURLReplacement replaces Vite's absolute-base assetsURL with a
// relative-base version that resolves dynamic imports against the importing
// module's URL (import.meta.url), which already has the correct OOD subpath.
const assetsURLReplacement = `const assetsURL=function(dep,importerUrl){return new URL(dep,importerUrl||location.href).href}`

// assetsURLArrowReplacement is the replacement for the arrow-function variant.
const assetsURLArrowReplacement = `assetsURL=function(dep,importerUrl){return new URL(dep,importerUrl||location.href).href}`

// webpackPublicPathRe matches webpack's internal runtime variable that controls
// where chunks are loaded from, in the minified form emitted by Create React App,
// Vue CLI, and similar webpack-based bundlers when the public path is "/":
//
//	__webpack_require__.p="/"
//	__webpack_require__.p = "/"
//
// We only rewrite the exact value "/"  (bare root). Prefixes like "/_next/" are
// intentionally left untouched because they encode path structure that must be
// preserved for chunk resolution to work correctly.
var webpackPublicPathRe = regexp.MustCompile(
	`(__webpack_require__\.p\s*=\s*)"/"`,
)

// webpackPublicPathReplacement rewrites __webpack_require__.p to "./" so that
// all webpack chunk loads resolve relative to the current document URL, which
// (thanks to the <base href> injected by injectAfterHead) already includes the
// correct OOD subpath.
const webpackPublicPathReplacement = `${1}"./"`

// webpackPublicPathFullRe matches the non-minified assignment form used in
// unminified development builds:
//
//	__webpack_public_path__ = "/"
var webpackPublicPathFullRe = regexp.MustCompile(
	`__webpack_public_path__\s*=\s*"/"`,
)

// webpackPublicPathFullReplacement is the replacement for the non-minified form.
const webpackPublicPathFullReplacement = `__webpack_public_path__="./"`

// sourceMappingURLJSRe matches sourcemap URL comments in JS files.
var sourceMappingURLJSRe = regexp.MustCompile(`(//# sourceMappingURL=)(/)`)

// sourceMappingURLCSSRe matches sourcemap URL comments in CSS files.
var sourceMappingURLCSSRe = regexp.MustCompile(`(/\*# sourceMappingURL=)(/)`)

// importMapScriptRe matches <script type="importmap"> blocks in HTML.
var importMapScriptRe = regexp.MustCompile(`(?i)<script[^>]+type\s*=\s*["']importmap["'][^>]*>([\s\S]*?)</script>`)

func main() {
	externalPort := flag.Int("external-port", 8080, "Port for inbound HTTP traffic")
	target := flag.String("target", "", "Upstream address (host:port). Required.")
	rewriteHTML := flag.Bool("rewrite-html", true, "Rewrite absolute paths and inject fetch/XHR interceptor in HTML responses. Set to false to disable.")
	basePathFlag := flag.String("base-path", "", "OOD subpath prefix (e.g. /rnode/gnode001.cluster/65038). When set, injects <base href> into HTML responses so the browser resolves assets correctly.")

	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Printf("adaptr v%s (commit: %s)\n", version, gitCommit)
		os.Exit(0)
	}

	flag.Parse()

	// Accept env vars — useful inside Docker.
	if p := os.Getenv("EXTERNAL_PORT"); p != "" {
		fmt.Sscanf(p, "%d", externalPort)
	}
	if t := os.Getenv("TARGET"); t != "" {
		*target = t
	}
	if v := os.Getenv("REWRITE_HTML"); v != "" {
		*rewriteHTML = v != "false" && v != "0" && v != "no"
	}
	if bp := os.Getenv("BASE_PATH"); bp != "" {
		*basePathFlag = bp
	}
	if pp := os.Getenv("PASSTHROUGH_PATHS"); pp != "" {
		for _, p := range strings.Split(pp, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				passthroughPaths = append(passthroughPaths, p)
			}
		}
	}
	if mb := os.Getenv("MAX_REWRITE_BODY_BYTES"); mb != "" {
		var n int64
		if _, err := fmt.Sscanf(mb, "%d", &n); err == nil && n > 0 {
			maxRewriteBodyBytes = n
		}
	}

	if *target == "" {
		log.Fatal("TARGET or --target is required")
	}

	// Normalise base path: must start with "/" and must NOT end with "/".
	if *basePathFlag != "" {
		bp := *basePathFlag
		if !strings.HasPrefix(bp, "/") {
			bp = "/" + bp
		}
		bp = strings.TrimRight(bp, "/")
		// Validate BASE_PATH against allowlist to prevent XSS via injection
		// into <base href="...">. Only alphanumerics, slash, underscore, dash, dot.
		if !basePathRe.MatchString(bp) {
			log.Fatalf("BASE_PATH %q contains invalid characters (allowed: [a-zA-Z0-9/_.-])", bp)
		}
		basePath = bp
	}

	runDirectProxy(*target, *externalPort, *rewriteHTML)
}

// swScript is the Service Worker JavaScript served at <basePath>/sw.js.
// It intercepts ALL same-origin fetch/navigation requests at the browser
// network layer and rewrites paths that are missing the basePath prefix.
// This is the only reliable fix for full-page navigations (window.location.replace,
// window.location.href=, Navigation API) that bypass JavaScript-level patching
// because window.location is an unforgeable object in Chrome/Edge.
//
// The single %s verb is replaced with the actual basePath value at request time
// via fmt.Sprintf.
const swScript = `const BASE_PATH = '%s';

self.addEventListener('fetch', function(event) {
  var url;
  try { url = new URL(event.request.url); } catch(e) { return; }

  // Only intercept same-origin requests.
  if (url.origin !== self.location.origin) return;

  // Skip paths that already have the correct prefix.
  if (url.pathname === BASE_PATH || url.pathname.startsWith(BASE_PATH + '/')) return;

  // Don't rewrite OOD system paths — these must reach OOD directly.
  if (url.pathname.startsWith('/pun/') ||
      url.pathname.startsWith('/nginx/') ||
      url.pathname.startsWith('/oidc')) return;

  // Only rewrite absolute paths that start with '/'.
  if (!url.pathname.startsWith('/') || url.pathname.startsWith('//')) return;

  var newUrl = new URL(BASE_PATH + url.pathname + url.search + url.hash, url.origin);
  event.respondWith(fetch(new Request(newUrl.toString(), event.request)));
});

self.addEventListener('install', function() { self.skipWaiting(); });
self.addEventListener('activate', function(event) { event.waitUntil(clients.claim()); });
`

// rewriteScript is injected immediately after <head> (or <head ...>) in HTML
// responses served in direct proxy mode. It monkey-patches window.fetch,
// XMLHttpRequest.open, WebSocket, and EventSource so that absolute paths
// emitted by SPAs (e.g. /api/v1/auth) are resolved relative to the current
// page path instead of the domain root. This is necessary when the app is
// served behind a subpath like /rnode/gnode001/46681/ — without this, the
// browser would send those requests to the wrong location and they would never
// reach the proxy.
//
// The base path b is persisted in sessionStorage so that SPA routers that call
// history.pushState (changing window.location.pathname) do not invalidate b on
// a subsequent hard reload.
const rewriteScript = `<script>(function(){` +
	// Read base path from sessionStorage so that hard reloads after
	// history.pushState still use the original proxy subpath, not the SPA route.
	`var sk='__adaptr_base';var b=sessionStorage.getItem(sk);` +
	`if(!b){b=window.location.pathname.replace(/\/$/,'');sessionStorage.setItem(sk,b)}` +
	`if(!b)return;` +
	// Save original history methods before use. oRS is used by the
	// readystatechange URL strip below; oPS is kept for symmetry and
	// in case future patches need it.
	`var oPS=history.pushState.bind(history);` +
	`var oRS=history.replaceState.bind(history);` +
	// Strip the OOD subpath from the URL bar ONCE, right before React Router
	// reads window.location.pathname on initialisation.
	//
	// Why readystatechange/interactive and not immediately?
	//   At <head> parse time the browser has NOT yet resolved src="./index.js"
	//   attributes — they are resolved against the current window.location as
	//   the parser encounters them. Calling replaceState('/') at that point
	//   would make all relative asset paths resolve against '/' instead of the
	//   OOD subpath, breaking asset loading. At readyState===interactive the
	//   HTML has been fully parsed and all attribute URLs are already resolved,
	//   so it is safe to change window.location.pathname.
	//
	// Why DON'T we restore the URL afterward?
	//   Restoring (via a DOMContentLoaded setTimeout) was causing a race where
	//   React Router initialised against the clean path, then saw a popstate-
	//   like URL change back to the OOD path and either re-initialised or 404'd.
	//   Leaving the clean SPA path in the URL bar is acceptable: dynamic
	//   import() resolves against import.meta.url (the module's load URL, which
	//   still has the OOD subpath), not window.location, so chunk loading is
	//   unaffected. The fetch/XHR/WS interceptors below use b from sessionStorage
	//   and are independent of window.location entirely.
	//
	// Tradeoff: the browser address bar shows the clean SPA path (e.g. '/') for
	// the duration of the session. Page refresh navigates to the OOD root and
	// breaks — acceptable for ephemeral HPC sessions where refresh is rare.
	`document.addEventListener('readystatechange',function(){` +
	`if(document.readyState==='interactive'){` +
	`var cp=window.location.pathname;` +
	`if(cp.startsWith(b)){` +
	`var cl=cp.slice(b.length)||'/';` +
	`var qs=window.location.search;var hh=window.location.hash;` +
	`oRS(null,'',cl+qs+hh);` +
	// Restore the OOD subpath URL 200ms after DOMContentLoaded so users see
	// the real URL in the address bar once React has fully initialised.
	//
	// Registered inside the readystatechange if-block so that cl, qs, hh are
	// captured in the closure (they are declared with var in this scope).
	//
	// Calls the PATCHED history.replaceState (not oRS) so that the base path b
	// is automatically prepended — e.g. replaceState(null,'','/') becomes
	// replaceState to /rnode/.../35704/. This is safe because <base href> keeps
	// document.baseURI locked to the OOD subpath regardless of window.location.
	//
	// Gate on window.__adaptrBaseInject (set by injectAfterHead when basePath is
	// non-empty) so the restore is a no-op when no <base> tag was injected.
	`document.addEventListener('DOMContentLoaded',function(){` +
	`setTimeout(function(){` +
	`if(b&&window.__adaptrBaseInject){history.replaceState(null,'',cl+qs+hh)}` +
	`},200)})}` +
	`}});` +
	// pushState patch: prepend the OOD subpath to any clean SPA path that
	// React Router pushes. oPS is the original (unpatched) pushState saved
	// above. We do NOT dispatch a synthetic popstate event — that causes React
	// Router to re-read pathname and break routing.
	`history.pushState=function(s,t,u){` +
	`if(typeof u==='string'&&u.startsWith('/')&&!u.startsWith('//')&&!u.startsWith(b)){u=b+u}` +
	`return oPS(s,t,u)};` +
	// replaceState patch: same logic as pushState patch. oRS is the original
	// (unpatched) replaceState saved above. The readystatechange handler's
	// oRS(null,'',cl+qs+hh) call uses the saved original oRS, so the initial
	// URL strip still works correctly — it does NOT go through this patch.
	`history.replaceState=function(s,t,u){` +
	`if(typeof u==='string'&&u.startsWith('/')&&!u.startsWith('//')&&!u.startsWith(b)){u=b+u}` +
	`return oRS(s,t,u)};` +
	// Location prototype patches: intercept window.location.replace(),
	// window.location.assign(), and window.location.href= assignments that
	// bypass history.pushState entirely. AnythingLLM's NewThreadButton calls
	// window.location.replace("/workspace/<slug>/t/<uuid>") after creating a
	// thread; "Delete All" uses window.location.href = "/workspace/<slug>".
	// Both bypass the pushState/replaceState patches above and the Navigation
	// API interceptor (which OOD's iframe sandboxing prevents from firing).
	//
	// interceptNav checks whether a URL is a same-origin absolute path that is
	// missing the base prefix b. If so it returns the clean path so the caller
	// can convert it to a pushState + popstate instead of a full navigation.
	`var interceptNav=function(url){` +
	`if(typeof url!=='string')return null;` +
	`if(url.startsWith('/')&&!url.startsWith('//')&&!url.startsWith(b+'/')&&url!==b){return url}` +
	`try{var parsed=new URL(url);` +
	`if(parsed.origin===window.location.origin&&` +
	`!parsed.pathname.startsWith(b+'/')&&parsed.pathname!==b){` +
	`return parsed.pathname+parsed.search+parsed.hash}}catch(e){}` +
	`return null};` +
	// window.__wlr is a JS-bundle-level wrapper for window.location.replace().
	// Because Location.prototype.replace is unforgeable (the prototype patch
	// above only fires when called through a Location object), calls of the form
	// window.location.replace(url) that are baked into a minified bundle bypass
	// the prototype patch entirely — the bundle text resolves the reference at
	// parse time, not at call time. Renaming those call sites in rewriteJS()
	// to window.__wlr() routes them through interceptNav instead, so the
	// New Thread navigation becomes a pushState + popstate rather than a
	// full page reload that escapes the OOD proxy subpath.
	`window.__wlr=function(u){` +
	`var clean=interceptNav(u);` +
	`if(clean!==null){history.pushState(null,'',clean);window.dispatchEvent(new PopStateEvent('popstate',{state:null}));return;}` +
	`window.location.replace(u)};` +
	// Patch Location.prototype.replace — used by AnythingLLM's NewThreadButton.
	`var oLocReplace=Location.prototype.replace;` +
	`Location.prototype.replace=function(url){` +
	`var clean=interceptNav(url);` +
	`if(clean!==null){history.pushState(null,'',clean);window.dispatchEvent(new PopStateEvent('popstate',{state:null}));return}` +
	`return oLocReplace.call(this,url)};` +
	// Patch Location.prototype.assign — same logic, different entry point.
	`var oLocAssign=Location.prototype.assign;` +
	`Location.prototype.assign=function(url){` +
	`var clean=interceptNav(url);` +
	`if(clean!==null){history.pushState(null,'',clean);window.dispatchEvent(new PopStateEvent('popstate',{state:null}));return}` +
	`return oLocAssign.call(this,url)};` +
	// Patch Location.prototype.href setter — catches window.location.href = "..."
	// direct assignments (e.g. AnythingLLM's "Delete All" handler).
	`var locDesc=Object.getOwnPropertyDescriptor(Location.prototype,'href');` +
	`if(locDesc&&locDesc.set){` +
	`var oLocHrefSet=locDesc.set;` +
	`Object.defineProperty(Location.prototype,'href',{` +
	`get:locDesc.get,` +
	`set:function(url){` +
	`var clean=interceptNav(url);` +
	`if(clean!==null){history.pushState(null,'',clean);window.dispatchEvent(new PopStateEvent('popstate',{state:null}));return}` +
	`return oLocHrefSet.call(this,url)},` +
	`configurable:true})}` +
	// popstate capture listener: fires on back/forward navigation before React
	// Router's listener (capture phase). Strips the OOD prefix so React Router
	// sees clean paths. Uses oRS (original, unpatched replaceState) to avoid
	// triggering this patch again and causing an infinite loop.
	`window.addEventListener('popstate',function(){` +
	`var pp=window.location.pathname;` +
	`if(pp.startsWith(b+'/')||pp===b){` +
	`var cl2=pp.slice(b.length)||'/';` +
	`var qs3=window.location.search;var hh3=window.location.hash;` +
	`oRS(null,'',cl2+qs3+hh3);` +
	// After React Router reads the clean path, restore the full OOD URL.
	// setTimeout(0) defers to the next microtask — React Router's bubble-phase
	// popstate listener will have already read the stripped pathname by then.
	`setTimeout(function(){oRS(null,'',b+cl2+qs3+hh3)},0)}` +
	`},true);` +
	// rewriteURL rewrites a string URL so that:
	//   - relative absolute paths ("/foo") get the base prepended
	//   - full same-origin URLs that are missing the base path get it inserted
	// Defined as a local var so all patches below can share the same logic.
	`var rewriteURL=function(u){` +
	`if(typeof u!=='string')return u;` +
	`if(u.startsWith('/')&&!u.startsWith('//')){return b+u}` +
	`var o=window.location.origin+'/';` +
	`if(u.startsWith(o)&&!u.startsWith(window.location.origin+b+'/')){` +
	`return window.location.origin+b+'/'+u.slice(o.length)}` +
	`return u};` +
	// Patch fetch: handle both string URLs and Request objects.
	// For Request objects, construct a new Request with the rewritten URL while
	// preserving all other properties (method, headers, body, credentials, etc.)
	// by passing the original Request as the init parameter.
	`var F=window.fetch;window.fetch=function(u,o){` +
	`if(typeof u==='string'){u=rewriteURL(u)}` +
	`else if(u instanceof Request){` +
	`var ru=u.url;` +
	`var parsed=new URL(ru);` +
	`if(parsed.origin===window.location.origin&&parsed.pathname.startsWith('/')&&!parsed.pathname.startsWith(b)){` +
	`u=new Request(parsed.origin+b+parsed.pathname+parsed.search+parsed.hash,u)}}` +
	`return F.call(this,u,o)};` +
	// Fix 1: XHR patch — build a copy of arguments so the modified URL is passed.
	`var X=XMLHttpRequest.prototype.open;XMLHttpRequest.prototype.open=function(m,u){` +
	`if(typeof u==='string'){u=rewriteURL(u)}` +
	`var a=Array.from(arguments);a[1]=u;return X.apply(this,a)};` +
	// Fix 3: WebSocket constructor patch — handles both relative paths and full
	// ws:// / wss:// URLs (same-origin derived from window.location.origin or
	// window.location.host). AnythingLLM builds URLs like:
	//   `${wsProtocol}//${window.location.host}/api/agent-invocation/${id}`
	// which produces ws://host/path — matching by host (not origin) is required
	// because the protocol component (ws vs wss) may differ from http/https.
	`var W=window.WebSocket;window.WebSocket=function(u,p){` +
	`if(typeof u==='string'){` +
	`if(u.startsWith('/')&&!u.startsWith('//')){u=b+u}` +
	`else{` +
	// Path 1: same-origin check via derived ws:// / wss:// origin prefix.
	`var wo=window.location.origin.replace(/^http/,'ws')+'/';` +
	`if(u.startsWith(wo)&&!u.startsWith(window.location.origin.replace(/^http/,'ws')+b+'/')){` +
	`u=window.location.origin.replace(/^http/,'ws')+b+'/'+u.slice(wo.length)` +
	`}else if(u.startsWith('ws://')||u.startsWith('wss://')){` +
	// Path 2: parse the URL and check if host matches window.location.host.
	// This catches patterns like ws://host/path where the ws/wss protocol
	// does not derive from the page's http/https protocol (e.g. mixed OOD proxies).
	`try{var wu=new URL(u);` +
	`if(wu.host===window.location.host&&!wu.pathname.startsWith(b+'/')&&wu.pathname!==b){` +
	`wu.pathname=b+wu.pathname;u=wu.toString()}}catch(e){}}}}` +
	`return p!==undefined?new W(u,p):new W(u)};` +
	`window.WebSocket.prototype=W.prototype;` +
	`window.WebSocket.CONNECTING=W.CONNECTING;window.WebSocket.OPEN=W.OPEN;` +
	`window.WebSocket.CLOSING=W.CLOSING;window.WebSocket.CLOSED=W.CLOSED;` +
	// Fix 4: EventSource constructor patch — also handles full same-origin URLs.
	`if(window.EventSource){` +
	`var E=window.EventSource;window.EventSource=function(u,o){` +
	`if(typeof u==='string'){u=rewriteURL(u)}` +
	`return new E(u,o)};` +
	`window.EventSource.prototype=E.prototype}` +
	// MutationObserver: catch dynamically injected <link>, <script>, <img> elements
	// whose src/href is an absolute path that hasn't been prefixed with the base yet.
	// This covers Vite's runtime <link rel="stylesheet" href="/github.css"> injection.
	`new MutationObserver(function(ms){ms.forEach(function(m){m.addedNodes.forEach(function(n){` +
	`if(n.nodeType!==1)return;` +
	`var t=n.tagName;` +
	`if(t==='LINK'||t==='SCRIPT'||t==='IMG'){` +
	`var a=t==='LINK'?'href':'src';` +
	`var v=n.getAttribute(a);` +
	`if(v&&v.startsWith('/')&&!v.startsWith('//')&&!v.startsWith(b)){n.setAttribute(a,b+v)}}` +
	`})})}).observe(document.documentElement,{childList:true,subtree:true});` +
	// Click interceptor for <a> tags with absolute href paths.
	// AnythingLLM's workspace sidebar uses raw <a href="/workspace/slug">
	// instead of React Router <Link>, causing full page navigations that
	// bypass our pushState patch. This captures those clicks and converts
	// them to pushState navigations with the correct OOD subpath.
	`document.addEventListener('click',function(e){` +
	`var a=e.target.closest('a');` +
	`if(!a)return;` +
	`var href=a.getAttribute('href');` +
	`if(!href||href==='#'||href.startsWith('http')||href.startsWith('//')||href.startsWith('mailto:'))return;` +
	// Only intercept same-origin absolute paths that look like SPA routes
	`if(href.startsWith('/')){` +
	`e.preventDefault();` +
	`history.pushState(null,'',href);` +
	// Dispatch popstate so React Router picks up the navigation
	`window.dispatchEvent(new PopStateEvent('popstate',{state:null}))}` +
	`},true);` +
	// Service Worker registration — only when basePath is set (__adaptrBaseInject=true).
	// The SW is served at <basePath>/sw.js by the proxy and registered with
	// scope:'/' so it controls ALL navigations on the origin, including full-page
	// ones that bypass every JS-level patch above (window.location.replace,
	// window.location.href=, Navigation API). This is the definitive fix for
	// AnythingLLM's "New Thread" full-page navigation to /workspace/slug/t/uuid.
	// The 'Service-Worker-Allowed: /' response header (set by the Go handler)
	// is required to register a SW with a scope broader than its script URL.
	`if(window.__adaptrBaseInject&&'serviceWorker' in navigator){` +
	`navigator.serviceWorker.register(b+'/sw.js',{scope:b+'/'})` +
	`.catch(function(e){console.warn('[adaptr] SW registration failed:',e)})}` +
	// Patch navigator.serviceWorker.register to rewrite absolute scope and
	// script paths for any app that registers its own Service Worker.
	`if('serviceWorker' in navigator){` +
	`var oSWR=navigator.serviceWorker.register.bind(navigator.serviceWorker);` +
	`navigator.serviceWorker.register=function(u,o){` +
	`if(typeof u==='string'&&u.startsWith('/')&&!u.startsWith('//'))u=b+u;` +
	`if(o&&o.scope&&typeof o.scope==='string'&&o.scope.startsWith('/'))o.scope=b+o.scope;` +
	`return oSWR(u,o)};` +
	`}` +
	`})()</script>`

// detectFramework inspects the first 4KB of a JS response to identify the
// bundler framework, allowing framework-specific rewrites to be gated on
// detection rather than applied universally.
func detectFramework(body []byte) string {
	sample := body
	if len(sample) > 4096 {
		sample = sample[:4096]
	}
	s := string(sample)
	if strings.Contains(s, "__vite_preload__") || strings.Contains(s, "const assetsURL") {
		return "vite"
	}
	if strings.Contains(s, "__webpack_require__") {
		return "webpack"
	}
	if strings.Contains(s, "System.register") {
		return "systemjs"
	}
	return ""
}

// rewriteHTML rewrites absolute asset paths in an HTML document to relative
// paths so that they resolve correctly when served behind a subpath proxy.
// It also injects rewriteScript immediately after the opening <head> tag.
//
// Rewrites applied to attribute values (src, href, action):
//   - ` src="/`  →  ` src="./`    (double-quote form)
//   - ` src='/`  →  ` src='./`    (single-quote form)
//
// The space prefix prevents false matches on compound attributes such as
// data-src="..." — HTML attributes are always preceded by whitespace.
//
// Protocol-relative URLs (`//`) are intentionally left untouched.
func rewriteHTMLBody(body []byte) []byte {
	s := string(body)

	// Rewrite quoted attribute values that start with a single "/".
	// We must not touch "//" (protocol-relative) so we replace `="/` with
	// `="./` only when the character after "/" is not another "/".
	//
	// Fix 7: include a leading space in the needle so that compound attribute
	// names like data-src="/" are not matched. HTML spec requires whitespace
	// before each attribute name.
	for _, attr := range []string{"src", "href", "action"} {
		// Double-quote form: (space)attr="/...
		dq := ` ` + attr + `="/`
		dqRepl := ` ` + attr + `="./`
		// Single-quote form: (space)attr='/...
		sq := ` ` + attr + `='/`
		sqRepl := ` ` + attr + `='./`

		s = replaceAttrPath(s, dq, dqRepl)
		s = replaceAttrPath(s, sq, sqRepl)
	}

	// Rewrite <script type="importmap"> blocks.
	s = rewriteImportMaps(s)

	// Inject the fetch/XHR/WebSocket/EventSource interceptor script right after
	// the opening <head> tag.
	s = injectAfterHead(s)

	return []byte(s)
}

// rewriteImportMaps finds <script type="importmap"> blocks and rewrites any
// absolute path values (starting with "/") to relative paths ("./") in both
// "imports" and "scopes" keys of the import map JSON.
func rewriteImportMaps(s string) string {
	return importMapScriptRe.ReplaceAllStringFunc(s, func(match string) string {
		// Extract the JSON content between the script tags.
		submatches := importMapScriptRe.FindStringSubmatch(match)
		if len(submatches) < 2 {
			return match
		}
		jsonContent := submatches[1]

		var m map[string]interface{}
		if err := json.Unmarshal([]byte(jsonContent), &m); err != nil {
			// Not valid JSON — return unchanged.
			return match
		}

		rewriteImportMapPaths(m)

		rewritten, err := json.Marshal(m)
		if err != nil {
			return match
		}

		// Replace the JSON content within the original match.
		return strings.Replace(match, jsonContent, string(rewritten), 1)
	})
}

// rewriteImportMapPaths recursively rewrites absolute path values in an import
// map object. Both "imports" (flat map) and "scopes" (nested map) are handled.
func rewriteImportMapPaths(m map[string]interface{}) {
	for k, v := range m {
		switch val := v.(type) {
		case string:
			if strings.HasPrefix(val, "/") && !strings.HasPrefix(val, "//") {
				m[k] = "./" + strings.TrimPrefix(val, "/")
			}
		case map[string]interface{}:
			rewriteImportMapPaths(val)
		}
	}
}

// replaceAttrPath replaces occurrences of needle with replacement, but only
// when the character immediately following needle is not "/" (to skip
// protocol-relative URLs like href="//cdn.example.com/...").
func replaceAttrPath(s, needle, replacement string) string {
	var b strings.Builder
	for {
		idx := strings.Index(s, needle)
		if idx == -1 {
			b.WriteString(s)
			break
		}
		// The needle ends with "/". Check the character right after it.
		afterNeedle := idx + len(needle)
		if afterNeedle < len(s) && s[afterNeedle] == '/' {
			// Protocol-relative URL — skip this occurrence.
			b.WriteString(s[:afterNeedle])
			s = s[afterNeedle:]
			continue
		}
		b.WriteString(s[:idx])
		b.WriteString(replacement)
		s = s[idx+len(needle):]
	}
	return b.String()
}

// rewriteCSS rewrites absolute path references inside CSS url() values to
// relative paths. This is the CSS equivalent of the HTML attribute rewriting
// done in rewriteHTML, and is needed when a CSS file contains rules like:
//
//	background: url(/images/bg.png);
//
// which would be sent to the wrong location when the app is served behind a
// subpath proxy.
//
// The following patterns are rewritten (absolute path starting with "/" but not
// "//"):
//
//	url(/…)     →  url(./…)     (unquoted)
//	url("/…")   →  url("./…")   (double-quoted)
//	url('/…')   →  url('./…')   (single-quoted)
//	@import "/…"  →  @import "./…"  (double-quoted string form)
//	@import '/…'  →  @import './…'  (single-quoted string form)
//
// Protocol-relative URLs (url(//…)) are intentionally left untouched.
func rewriteCSS(body []byte) []byte {
	s := string(body)
	for _, pair := range [][2]string{
		{`url(/`, `url(./`},
		{`url("/`, `url("./`},
		{`url('/`, `url('./`},
	} {
		needle, replacement := pair[0], pair[1]
		s = replaceAttrPath(s, needle, replacement)
	}

	// Rewrite @import string form (not url() form — handled above).
	for _, pair := range [][2]string{
		{`@import "/`, `@import "./`},
		{`@import '/`, `@import './`},
	} {
		needle, replacement := pair[0], pair[1]
		s = replaceAttrPath(s, needle, replacement)
	}

	// Rewrite sourcemap URLs so DevTools can find maps when behind a subpath.
	s = sourceMappingURLCSSRe.ReplaceAllString(s, "${1}./")

	return []byte(s)
}

// rewriteJS rewrites Vite's absolute asset import paths inside JavaScript files
// to relative paths. This is needed for dynamic import() calls and CSS preload
// links that Vite generates using absolute paths like "/assets/foo.js" —
// those would fail when the app is served behind a subpath proxy.
//
// Only the specific patterns Vite emits are rewritten:
//
//	"/assets/   →  "./assets/   (double-quoted)
//	'/assets/   →  './assets/   (single-quoted)
//	"/github.css →  "./github.css (double-quoted)
//	'/github.css →  './github.css (single-quoted)
//
// We do NOT rewrite all "/... patterns because API URL strings embedded in
// the JS bundle (e.g. "/api/v1/auth") must be left alone — the runtime fetch
// interceptor handles those.
func rewriteJS(body []byte) []byte {
	framework := detectFramework(body)
	s := string(body)

	// Replace Vite's absolute-base assetsURL with a relative-base version.
	// Gate on vite detection to reduce false-positive risk.
	if framework == "vite" || framework == "" {
		// Replace Vite's absolute-base assetsURL with a relative-base version.
		// This must run BEFORE the string replacements below so that the function
		// body itself is not partially transformed by earlier passes.
		//
		// Vite emits (for base: "/", the default):
		//   const assetsURL=function(e){return"/"+e}
		// which makes all dynamic import() calls resolve against "/" (domain root)
		// instead of the correct OOD subpath. Replacing it with:
		//   const assetsURL=function(dep,importerUrl){return new URL(dep,importerUrl).href}
		// makes dynamic imports resolve relative to import.meta.url, which already
		// carries the correct OOD subpath from the module's original load URL.
		s = assetsURLRe.ReplaceAllString(s, assetsURLReplacement)
		s = assetsURLArrowRe.ReplaceAllString(s, assetsURLArrowReplacement)
	}

	if framework == "webpack" || framework == "" {
		// Replace webpack's public path variable so that chunk loads resolve
		// relative to the current document URL rather than the domain root.
		// This fixes Create React App, Vue CLI, and other webpack-based SPAs
		// that set __webpack_require__.p="/" (or __webpack_public_path__="/")
		// in their runtime bundle.
		//
		// The minified form (__webpack_require__.p) must be checked first because
		// some builds emit both; applying the full-name replacement first could
		// leave a stale __webpack_require__.p assignment that overrides it.
		s = webpackPublicPathRe.ReplaceAllString(s, webpackPublicPathReplacement)
		s = webpackPublicPathFullRe.ReplaceAllString(s, webpackPublicPathFullReplacement)
	}

	// Rewrite Vite asset paths and known static file extensions.
	// We target specific prefixes ("/assets/") and file extensions
	// (.css, .png, .svg, .jpg, .jpeg, .gif, .ico, .woff, .woff2, .ttf, .eot)
	// that appear as root-absolute strings in the JS bundle.
	for _, pair := range [][2]string{
		{`"/assets/`, `"./assets/`},
		{`'/assets/`, `'./assets/`},
	} {
		s = strings.ReplaceAll(s, pair[0], pair[1])
	}
	// Rewrite root-absolute static file references by extension in a single pass.
	// This replaces 22 separate O(n) strings.Replace calls with one O(n) scan.
	s = rewriteJSRootPaths(s)

	// Rewrite window.location.replace( call sites baked into JS bundles.
	// Location.prototype.replace is unforgeable — patching the prototype in
	// rewriteScript only intercepts calls made through a real Location object at
	// runtime. Minified bundles often resolve "window.location.replace" at parse
	// time, so the prototype patch never fires. Renaming the call site here
	// routes those calls through window.__wlr, which is defined in rewriteScript
	// and calls interceptNav before falling back to the real replace().
	// This is what fixes the AnythingLLM New Thread navigation bug: the bundle
	// calls window.location.replace("/") which would escape the OOD subpath;
	// with this rewrite it becomes window.__wlr("/"), which pushState instead.
	s = strings.ReplaceAll(s, "window.location.replace(", "window.__wlr(")

	// Rewrite sourcemap URLs so DevTools can find maps when behind a subpath.
	s = sourceMappingURLJSRe.ReplaceAllString(s, "${1}./")

	return []byte(s)
}

// jsRootExts is the set of file extensions that rewriteJSRootPaths will rewrite.
// Only root-level files (no "/" in the filename) ending with one of these
// extensions are rewritten. This avoids touching API paths like "/api/v1/auth".
var jsRootExts = map[string]bool{
	".css": true, ".js": true, ".mjs": true, ".cjs": true,
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
	".svg": true, ".ico": true, ".webp": true,
	".woff": true, ".woff2": true, ".ttf": true, ".eot": true,
	".map": true,
}

// rewriteJSRootPaths rewrites root-absolute static file string literals in
// JavaScript — patterns like "/filename.ext" (double- or single-quoted) where
// the filename contains no "/" — to relative paths: "./filename.ext".
//
// This is a single O(n) pass that replaces the previous 22-call loop (11 extensions
// × 2 quote styles). It scans for `"/` and `'/` patterns and checks whether the
// enclosed filename ends with a known static extension.
func rewriteJSRootPaths(s string) string {
	if len(s) < 2 {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))

	i := 0
	for i < len(s) {
		c := s[i]
		// Look for quote followed by / (need at least 2 bytes ahead)
		if (c == '"' || c == '\'') && i+1 < len(s) && s[i+1] == '/' {
			quote := c
			// Skip protocol-relative: `"//`
			if i+2 < len(s) && s[i+2] == '/' {
				out.WriteByte(c)
				i++
				continue
			}
			// Find closing quote (don't cross newlines — these are inline literals)
			j := i + 2
			for j < len(s) && s[j] != byte(quote) && s[j] != '\n' {
				j++
			}
			if j < len(s) && s[j] == byte(quote) {
				filename := s[i+2 : j] // content after the opening /
				// Only rewrite root-level files (no subdirectory) with known ext
				if !strings.Contains(filename, "/") {
					dotIdx := strings.LastIndex(filename, ".")
					if dotIdx >= 0 && jsRootExts[filename[dotIdx:]] {
						out.WriteByte(quote)
						out.WriteString("./")
						out.WriteString(filename)
						out.WriteByte(quote)
						i = j + 1
						continue
					}
				}
			}
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// rewriteManifest rewrites absolute paths in manifest.json (PWA manifest).
// Handles all path-valued fields in the manifest spec:
//   - top-level: start_url, scope
//   - icons[].src, shortcuts[].url, screenshots[].src
//   - share_target.action, file_handlers[].action
func rewriteManifest(body []byte) []byte {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		// Not valid JSON — fall back to simple string replacement.
		s := string(body)
		for _, pair := range [][2]string{
			{`"/"`, `"./"`},
			{`"/favicon`, `"./favicon`},
		} {
			s = strings.ReplaceAll(s, pair[0], pair[1])
		}
		return []byte(s)
	}

	// Rewrite top-level string fields that hold URL paths.
	for _, field := range []string{"start_url", "scope"} {
		if v, ok := m[field].(string); ok {
			if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
				m[field] = "./" + strings.TrimPrefix(v, "/")
			}
		}
	}

	// Rewrite share_target.action
	if st, ok := m["share_target"].(map[string]interface{}); ok {
		rewriteManifestPathField(st, "action")
	}

	// Rewrite icons[].src
	rewriteManifestSlice(m, "icons", "src")

	// Rewrite shortcuts[].url
	rewriteManifestSlice(m, "shortcuts", "url")

	// Rewrite screenshots[].src
	rewriteManifestSlice(m, "screenshots", "src")

	// Rewrite file_handlers[].action
	rewriteManifestSlice(m, "file_handlers", "action")

	out, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return out
}

// rewriteManifestSlice rewrites a named path field within each object of a
// top-level array in the manifest (e.g. icons[].src).
func rewriteManifestSlice(m map[string]interface{}, arrayKey, pathField string) {
	arr, ok := m[arrayKey].([]interface{})
	if !ok {
		return
	}
	for _, item := range arr {
		if obj, ok := item.(map[string]interface{}); ok {
			rewriteManifestPathField(obj, pathField)
		}
	}
}

// rewriteManifestPathField rewrites a single string field in a map if it is an
// absolute path.
func rewriteManifestPathField(m map[string]interface{}, field string) {
	if v, ok := m[field].(string); ok {
		if strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
			m[field] = "./" + strings.TrimPrefix(v, "/")
		}
	}
}

// injectAfterHead inserts content immediately after the first occurrence of a
// <head> opening tag (with or without attributes). If no <head> tag is found
// the document is returned unchanged.
//
// What gets injected, in order:
//  1. <base href="basePath/"> (only when the basePath package var is non-empty)
//  2. rewriteScript — the fetch/XHR/WebSocket/EventSource interceptor
//
// Injecting <base href> first ensures the browser locks document.baseURI to
// the OOD subpath before it resolves any subsequent src/href attributes. The
// interceptor script can then safely call replaceState('/') at
// readyState===interactive (so React Router sees a clean path) without
// affecting asset resolution, because document.baseURI is now controlled by
// <base>, not window.location.
func injectAfterHead(s string) string {
	// Only lowercase the first 512 bytes — <head> is always in the preamble.
	prefixLen := 512
	if prefixLen > len(s) {
		prefixLen = len(s)
	}
	lower := strings.ToLower(s[:prefixLen])
	// Match <head> or <head ...> — find the closing ">" of the opening tag.
	headIdx := strings.Index(lower, "<head")
	if headIdx == -1 {
		log.Printf("rewrite: no <head> tag found, skipping injection for response")
		return s
	}
	// Find the ">" that closes this opening tag.
	closeIdx := strings.Index(s[headIdx:], ">")
	if closeIdx == -1 {
		return s
	}
	insertAt := headIdx + closeIdx + 1 // position right after ">"

	return s[:insertAt] + startupInjection + s[insertAt:]
}

// isPassthrough returns true if the request path matches any configured
// passthrough prefix. Passthrough responses are forwarded without rewriting.
func isPassthrough(reqPath string) bool {
	for _, prefix := range passthroughPaths {
		if strings.HasPrefix(reqPath, prefix) {
			return true
		}
	}
	return false
}

// retryTransport wraps an http.RoundTripper and retries once on transient
// network errors for idempotent, bodyless requests (GET/HEAD).
type retryTransport struct {
	base http.RoundTripper
}

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	// Only retry idempotent, bodyless requests on network errors.
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return nil, err
	}
	if req.ContentLength != 0 {
		return nil, err
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		return nil, err
	}
	log.Printf("adaptr: retry %s %s after transient error: %v", req.Method, req.URL.RequestURI(), err)
	return t.base.RoundTrip(req)
}

// runDirectProxy starts a single HTTP server on externalPort that acts as a
// transparent path adapter for all requests to target (host:port). WebSocket
// upgrade requests are handled via a raw TCP pipe so that the full duplex stream
// is preserved. HTTP requests are handled by httputil.ReverseProxy which
// preserves all headers and supports streaming responses.
//
// When rewriteHTML is true (the default), text/html responses have their
// absolute asset paths rewritten to relative paths, and a small fetch/XHR
// interceptor script is injected after <head>. This fixes SPAs served behind
// a subpath adapter layer (e.g. Open OnDemand's /rnode/…/ prefix) that embed
// absolute paths like src="/index.js" in their HTML.
func runDirectProxy(target string, externalPort int, rewriteHTMLEnabled bool) {
	// Normalise: strip any leading scheme the caller may have included.
	target = strings.TrimPrefix(target, "http://")
	target = strings.TrimPrefix(target, "https://")

	targetURL := &url.URL{
		Scheme: "http",
		Host:   target,
	}

	rp := httputil.NewSingleHostReverseProxy(targetURL)

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   3 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		DisableCompression:    true, // adaptr manages encoding manually; don't let transport auto-decompress
	}
	rp.Transport = &retryTransport{base: transport}

	rp.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		errorResponse(w, req, err, target)
	}

	// Preserve the original Director and add hop-by-hop header cleanup.
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		// Strip basePath prefix before forwarding to backend.
		// The backend app knows nothing about OOD's subpath prefix.
		if basePath != "" {
			p := req.URL.Path
			if strings.HasPrefix(p, basePath+"/") {
				req.URL.Path = p[len(basePath):]
			} else if p == basePath {
				req.URL.Path = "/"
			}
			if req.URL.RawPath != "" {
				rp := req.URL.RawPath
				if strings.HasPrefix(rp, basePath+"/") {
					req.URL.RawPath = rp[len(basePath):]
				} else if rp == basePath {
					req.URL.RawPath = "/"
				}
			}
		}
		// Strip Accept-Encoding: br so the upstream responds with gzip or plain,
		// which adaptr can decode for rewriting. Brotli cannot be decoded with
		// stdlib and would skip all rewriting if allowed through.
		req.Header.Set("Accept-Encoding", "gzip, deflate")
		// Ensure the downstream app sees a clean X-Forwarded-For chain.
		if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
			if prior, ok := req.Header["X-Forwarded-For"]; ok {
				req.Header.Set("X-Forwarded-For", strings.Join(prior, ", ")+", "+clientIP)
			} else {
				req.Header.Set("X-Forwarded-For", clientIP)
			}
		}
	}

	rp.ModifyResponse = func(resp *http.Response) error {
		start := time.Now()
		path := resp.Request.URL.RequestURI()

		logRequest := func(note string) {
			log.Printf("adaptr %s %s → %d (%dms)%s",
				resp.Request.Method, path, resp.StatusCode,
				time.Since(start).Milliseconds(), note)
		}

		if !rewriteHTMLEnabled {
			logRequest("")
			return nil
		}

		// Skip rewriting for passthrough paths (API, CDN, etc.).
		if isPassthrough(resp.Request.URL.Path) {
			logRequest(" [passthrough]")
			return nil
		}

		// Lowercase Content-Type once to ensure case-insensitive matching.
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		isHTML := strings.Contains(ct, "text/html")
		isCSS := strings.Contains(ct, "text/css")
		isJS := strings.Contains(ct, "application/javascript") || strings.Contains(ct, "text/javascript")
		isJSON := strings.Contains(ct, "application/json") || strings.Contains(ct, "application/manifest+json")
		isManifest := isJSON && strings.HasSuffix(resp.Request.URL.Path, "manifest.json")

		// Path-based fallbacks: some servers serve assets with wrong or missing
		// Content-Type. When the URL path ends with a known extension, treat it
		// as that type so rewriting still runs.
		if !isJS {
			p := resp.Request.URL.Path
			if strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".mjs") || strings.HasSuffix(p, ".cjs") {
				isJS = true
			}
		}
		if !isCSS {
			if strings.HasSuffix(resp.Request.URL.Path, ".css") {
				isCSS = true
			}
		}

		if !isHTML && !isCSS && !isJS && !isManifest {
			logRequest("")
			return nil
		}

		// Check Content-Length against the size cap before reading the body.
		// If the upstream declared a size that exceeds our limit, pass through.
		if cl := resp.ContentLength; cl > maxRewriteBodyBytes {
			log.Printf("rewrite: skipping %s (Content-Length %d exceeds MAX_REWRITE_BODY_BYTES %d)",
				path, cl, maxRewriteBodyBytes)
			logRequest(" [size-cap]")
			return nil
		}

		ce := resp.Header.Get("Content-Encoding")

		// Tee the raw (possibly compressed) body so that if the decompressed
		// content exceeds the size cap, we can pass through the original
		// compressed bytes unchanged instead of truncating.
		var rawBuf bytes.Buffer
		rawReader := io.TeeReader(resp.Body, &rawBuf)

		var bodyReader io.Reader = rawReader

		if ce == "gzip" {
			gr, err := gzip.NewReader(rawReader)
			if err != nil {
				log.Printf("rewrite: gzip reader error for %s: %v", path, err)
				logRequest("")
				return nil
			}
			defer gr.Close()
			bodyReader = gr
		}

		// Wrap with LimitReader as a safety net for chunked responses that
		// declare no Content-Length. Exceeding the limit triggers the size-cap path.
		limitedReader := io.LimitReader(bodyReader, maxRewriteBodyBytes+1)

		// Read the entire (possibly decompressed) body so we can rewrite it.
		original, err := io.ReadAll(limitedReader)
		resp.Body.Close()
		if err != nil {
			return err
		}

		// If the body hit the limit exactly (+1 sentinel), it's too large.
		// Pass through the original compressed body unchanged rather than truncating.
		if int64(len(original)) > maxRewriteBodyBytes {
			log.Printf("rewrite: skipping %s (body exceeds MAX_REWRITE_BODY_BYTES %d after read)",
				path, maxRewriteBodyBytes)
			// Drain the rest of the original body so rawBuf has the full raw bytes.
			_, _ = io.Copy(&rawBuf, resp.Body)
			resp.Body = io.NopCloser(&rawBuf)
			// Leave Content-Encoding intact — we're passing through the original.
			resp.ContentLength = int64(rawBuf.Len())
			resp.Header.Del("Transfer-Encoding")
			logRequest(" [size-cap]")
			return nil
		}

		var rewritten []byte
		if isHTML {
			rewritten = rewriteHTMLBody(original)
			log.Printf("rewrite: injected base-path interceptor into HTML response for %s %s",
				resp.Request.Method, path)
		} else if isCSS {
			rewritten = rewriteCSS(original)
			log.Printf("rewrite: rewrote CSS url() paths in response for %s %s",
				resp.Request.Method, path)
		} else if isManifest {
			rewritten = rewriteManifest(original)
			log.Printf("rewrite: rewrote manifest.json paths for %s %s",
				resp.Request.Method, path)
		} else {
			rewritten = rewriteJS(original)
			log.Printf("rewrite: rewrote JS asset paths in response for %s %s",
				resp.Request.Method, path)
		}

		// Re-compress rewritten body with gzip if the original was gzip-encoded.
		// This avoids the bandwidth regression of serving decompressed content.
		// A pooled gzip.Writer is used to avoid per-request heap allocation.
		if ce == "gzip" {
			var buf bytes.Buffer
			gz := gzipWriterPool.Get().(*gzip.Writer)
			gz.Reset(&buf)
			if _, err := gz.Write(rewritten); err == nil {
				if err := gz.Close(); err == nil {
					rewritten = buf.Bytes()
					resp.Header.Set("Content-Encoding", "gzip")
				}
			}
			gzipWriterPool.Put(gz)
		} else {
			// Original was not compressed (or was handled above). Remove any
			// stale encoding header so the client doesn't try to decompress.
			resp.Header.Del("Content-Encoding")
		}

		// Replace body. Set the exact Content-Length and remove Transfer-Encoding:
		// chunked if present (we've now read the full body).
		resp.Body = io.NopCloser(bytes.NewReader(rewritten))
		resp.ContentLength = int64(len(rewritten))
		resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(rewritten)))
		resp.Header.Del("Transfer-Encoding")

		logRequest("")
		return nil
	}

	mux := http.NewServeMux()

	// /health — liveness check.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	// Serve the Service Worker script at <basePath>/sw.js.
	// The SW intercepts ALL same-origin fetch and navigation requests at the
	// browser network layer, rewriting paths that are missing the basePath prefix.
	// This is the only reliable fix for full-page navigations (window.location.replace,
	// window.location.href=) that bypass JavaScript-level interception because
	// window.location is an unforgeable object in Chrome/Edge.
	//
	// Service-Worker-Allowed: / is required when the SW's scope ('/') is broader
	// than its script URL path (basePath + '/sw.js'). Without this header, Chrome
	// will refuse to register the SW with a wider scope.
	if basePath != "" {
		swPath := basePath + "/sw.js"
		swContent := fmt.Sprintf(swScript, basePath)
		swHandler := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
			w.Header().Set("Service-Worker-Allowed", "/")
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
			fmt.Fprint(w, swContent)
		}
		mux.HandleFunc(swPath, swHandler)
		// OOD strips basePath before the request reaches us, so /sw.js is what
		// actually arrives on the wire. Register at both paths so the SW is
		// served regardless of whether the prefix was stripped.
		if swPath != "/sw.js" {
			mux.HandleFunc("/sw.js", swHandler)
		}
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// WebSocket upgrade: use raw TCP proxy so the full duplex stream works.
		if isWebSocketUpgrade(r) {
			proxyWebSocket(w, r, target)
			return
		}
		rp.ServeHTTP(w, r)
	})

	addr := fmt.Sprintf("0.0.0.0:%d", externalPort)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// No read/write timeouts: streaming responses and long-lived WebSocket
		// connections would be killed prematurely by a fixed timeout.
	}

	// Build the per-HTML injection string once — basePath is immutable after startup.
	// HTML-escape basePath before inserting into the attribute value to prevent XSS.
	if basePath != "" {
		startupInjection = `<base href="` + html.EscapeString(basePath) + `/">` +
			`<script>window.__adaptrBaseInject=true;</script>` +
			rewriteScript
	} else {
		startupInjection = rewriteScript
	}

	log.Printf("adaptr v%s (commit: %s)", version, gitCommit)
	log.Printf("adaptr direct mode → %s", target)
	log.Printf("listening on %s", addr)
	log.Printf("max rewrite body: %d bytes", maxRewriteBodyBytes)
	if len(passthroughPaths) > 0 {
		log.Printf("passthrough paths: %s", strings.Join(passthroughPaths, ", "))
	}
	if rewriteHTMLEnabled {
		log.Printf("html rewriting: enabled (set REWRITE_HTML=false or --rewrite-html=false to disable)")
		if basePath != "" {
			log.Printf("base-path injection: enabled (base href=%s/)", basePath)
		} else {
			log.Printf("base-path injection: disabled (set --base-path or BASE_PATH to enable)")
		}
	} else {
		log.Printf("html rewriting: disabled")
	}

	// Graceful shutdown: listen for SIGTERM/SIGINT and drain in-flight requests
	// before exiting. This prevents request drops on container restarts.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-quit
		log.Printf("adaptr: shutting down gracefully (30s timeout)")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
	}()

	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("direct proxy server error: %v", err)
	}
}

// isWebSocketUpgrade reports whether the request is an HTTP → WebSocket upgrade.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// proxyWebSocket dials the upstream target directly and copies bytes in both
// directions, giving the caller a raw TCP pipe through which WebSocket frames
// flow unmodified.
func proxyWebSocket(w http.ResponseWriter, r *http.Request, target string) {
	// Dial the upstream.
	upstream, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		log.Printf("adaptr WS dial %s: %v", target, err)
		errorResponse(w, r, err, target)
		return
	}
	defer upstream.Close()

	// Hijack the client connection so we can send/receive raw bytes.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		log.Printf("adaptr WS: ResponseWriter does not implement http.Hijacker")
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		log.Printf("adaptr WS hijack: %v", err)
		return
	}
	defer clientConn.Close()

	// Forward the original HTTP upgrade request to the upstream.
	if err := r.Write(upstream); err != nil {
		log.Printf("adaptr WS write request: %v", err)
		return
	}

	log.Printf("adaptr WS %s %s → connected", r.Method, r.URL.RequestURI())

	// Pipe bytes in both directions until either side closes.
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(upstream, clientBuf)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(clientConn, upstream)
		done <- struct{}{}
	}()
	<-done
}

func errorResponse(w http.ResponseWriter, req *http.Request, err error, target string) {
	var netErr net.Error
	var statusCode int
	var headline, detail string

	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		statusCode = http.StatusGatewayTimeout
		headline = "Upstream Timeout"
		detail = fmt.Sprintf("The upstream at %s did not respond within the timeout period.", target)
	case isConnectionRefused(err):
		statusCode = http.StatusBadGateway
		headline = "Upstream Unavailable"
		detail = fmt.Sprintf("Could not connect to %s. The application may still be starting.", target)
	default:
		statusCode = http.StatusBadGateway
		headline = "Bad Gateway"
		detail = fmt.Sprintf("adaptr could not reach the upstream at %s.", target)
	}

	log.Printf("adaptr error %s %s → %d: %v", req.Method, req.URL.RequestURI(), statusCode, err)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	fmt.Fprintf(w, errorPageHTML, statusCode, headline, headline, detail, req.URL.RequestURI(), target)
}

func isConnectionRefused(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok {
			return sysErr.Err == syscall.ECONNREFUSED
		}
	}
	return false
}

const errorPageHTML = `<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><title>%d %s</title>
<style>body{font-family:system-ui,sans-serif;max-width:640px;margin:80px auto;padding:0 16px;color:#1a1a2e;background:#fafaf9}
h1{font-size:1.5rem;margin-bottom:.5rem;color:#dc2626}p{color:#555;line-height:1.6}
code{background:#f0f0f0;padding:2px 6px;border-radius:3px;font-size:.9rem}
.meta{margin-top:1.5rem;padding-top:1rem;border-top:1px solid #e5e5e5;font-size:.85rem;color:#999}</style>
</head>
<body>
<h1>%s</h1>
<p>%s</p>
<div class="meta">Path: <code>%s</code> — Target: <code>%s</code> — adaptr</div>
</body>
</html>`
