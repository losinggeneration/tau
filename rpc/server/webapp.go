package server

import "net/http"

const webAppShell = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="theme-color" content="#111827">
<link rel="manifest" href="/manifest.webmanifest">
<title>tau remote</title>
<style>
:root{color-scheme:dark light;font-family:system-ui,-apple-system,Segoe UI,sans-serif;background:#111827;color:#f9fafb}body{margin:0}.wrap{max-width:860px;margin:0 auto;padding:12px}.card{background:#1f2937;border:1px solid #374151;border-radius:12px;padding:12px;margin:10px 0}input,textarea,button{font:inherit;border-radius:10px;border:1px solid #4b5563;padding:10px;background:#111827;color:#f9fafb}input,textarea{box-sizing:border-box;width:100%}textarea{min-height:88px}button{background:#2563eb;border-color:#2563eb}button.secondary{background:#374151;border-color:#4b5563}.row{display:flex;gap:8px;flex-wrap:wrap}.row>*{flex:1 1 140px}.sessions button{display:block;width:100%;margin:6px 0;text-align:left;background:#111827;border-color:#374151}.sessrow{display:flex;gap:6px}.sessrow button:first-child{flex:1;min-width:0}.sessrow button.pin{flex:0 0 auto;width:auto}.muted{color:#9ca3af;font-size:.9rem}.log{white-space:pre-wrap;word-break:break-word;min-height:180px}.event{border-top:1px solid #374151;padding:6px 0}.ok{color:#86efac}.bad{color:#fca5a5}@media (max-width:600px){.wrap{padding:8px}.row>*{flex-basis:100%}}
</style>
</head>
<body>
<div class="wrap">
<h1>tau remote</h1>
<div class="card">
<div class="row"><label>Token<input id="token" type="password" autocomplete="current-password"></label></div>
<div class="row"><button id="connect">Connect</button><button id="disconnect" class="secondary">Disconnect</button></div>
<div id="status" class="muted">Disconnected</div>
</div>
`

const webManifest = `{"name":"tau remote","short_name":"tau","start_url":"/","display":"standalone","background_color":"#111827","theme_color":"#111827"}`

const serviceWorkerJS = `self.addEventListener("install",e=>e.waitUntil(caches.open("tau-remote-v1").then(c=>c.addAll(["/","/app.js","/manifest.webmanifest"]))));self.addEventListener("fetch",e=>e.respondWith(fetch(e.request).catch(()=>caches.match(e.request))));`

const coordinatedPanels = `<div class="card"><h2>Running instances</h2><div class="row"><label>Project directory<input id="cwd" placeholder="Absolute path"></label><button id="start">Start instance</button></div><div id="instances" class="sessions muted">Connect to list running instances.</div></div>
<div class="card"><h2>Saved sessions</h2><div id="sessions" class="muted"></div><button id="refresh" class="secondary">Refresh saved sessions</button></div>
<div class="card"><h2>Conversation</h2><div class="muted" id="active"></div><div id="queue" class="muted"></div><div id="history" class="log"></div><textarea id="message" placeholder="Message"></textarea><input id="files" type="file" multiple><div id="attachments" class="muted"></div><div class="row"><button id="send">Send</button><button id="follow" class="secondary">Follow up</button><button id="interrupt" class="secondary">Interrupt</button><button id="detach" class="secondary">Detach</button><button id="stop" class="secondary">Stop instance</button></div><div id="outcomes" class="muted" aria-live="polite"></div></div>
<div class="card"><h2>UI requests</h2><div id="uiRequests" class="muted">None</div></div>
<div class="card"><h2>Live activity</h2><div id="log" class="log"></div></div>`

func registerCoordinatedWebApp(mux *http.ServeMux) {
	registerWebAppAssets(mux, webAppShell+coordinatedPanels+`
</div>
<script src="/app.js"></script>
</body>
</html>`, coordinatedWebAppJS)
}

func registerWebAppAssets(mux *http.ServeMux, page, script string) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	})
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(script))
	})
	mux.HandleFunc("/manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		_, _ = w.Write([]byte(webManifest))
	})
	mux.HandleFunc("/sw.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		_, _ = w.Write([]byte(serviceWorkerJS))
	})
}
