package server

import (
	"net/http"
	"strings"
)

const webAppHTML = `<!doctype html>
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
<div class="row"><label>WebSocket URL<input id="url" placeholder="ws://127.0.0.1:8080/rpc"></label><label>Token<input id="token" type="password" autocomplete="current-password"></label></div>
<div class="row"><button id="connect">Connect</button><button id="disconnect" class="secondary">Disconnect</button><button id="refresh" class="secondary">Refresh sessions</button></div>
<div id="status" class="muted">Disconnected</div>
</div>
<div class="card"><h2>Sessions</h2><div id="machine" class="muted"></div><div id="sessions" class="sessions muted">Connect to list sessions.</div></div>
<div class="card"><h2>Controller</h2><div class="muted" id="active">No active session</div><textarea id="message" placeholder="Message"></textarea><input id="files" type="file" multiple><div id="attachments" class="muted"></div><div class="row"><button id="send">Send</button><button id="steer" class="secondary">Steer</button><button id="follow" class="secondary">Follow up</button><button id="interrupt" class="secondary">Interrupt</button></div><div class="row"><input id="renameText" placeholder="Rename active session"><button id="rename" class="secondary">Rename</button></div></div>
<div class="card"><h2>UI requests</h2><div id="uiRequests" class="muted">None</div></div>
<div class="card"><h2>Events</h2><div id="log" class="log"></div></div>
</div>
<script src="/app.js"></script>
</body>
</html>`

const webAppJS = `let ws,seq=0,currentSession="";const pending=new Map();
const $=id=>document.getElementById(id);const log=m=>{$("log").insertAdjacentHTML("beforeend","<div class=event>"+escapeHtml(m)+"</div>");};
function escapeHtml(s){return String(s).replace(/[&<>"]/g,c=>({"&":"&amp;","<":"&lt;",">":"&gt;","\"":"&quot;"}[c]));}
function status(s,ok=true){$("status").innerHTML="<span class="+(ok?"ok":"bad")+">"+escapeHtml(s)+"</span>";}
function wsURL(){let u=$("url").value.trim()||((location.protocol==="https:"?"wss://":"ws://")+location.host+"/rpc");let t=$("token").value.trim();if(t){u+=(u.includes("?")?"&":"?")+"token="+encodeURIComponent(t);}return u;}
function cmd(type,args={}){if(!ws||ws.readyState!==WebSocket.OPEN)return Promise.reject(new Error("not connected"));let id="web-"+(++seq);ws.send(JSON.stringify({id,type,args}));return new Promise((resolve,reject)=>{pending.set(id,{resolve,reject});setTimeout(()=>{if(pending.delete(id))reject(new Error("timeout"));},30000);});}
function connect(){localStorage.tauWsURL=$("url").value;localStorage.tauWsToken=$("token").value;ws=new WebSocket(wsURL());ws.onopen=()=>{status("Connected");listSessions();};ws.onclose=()=>status("Disconnected",false);ws.onerror=()=>status("Connection error",false);ws.onmessage=e=>frame(JSON.parse(e.data));}
function frame(f){if(f.id){let p=pending.get(f.id);if(p){pending.delete(f.id);f.ok?p.resolve(f.result||{}):p.reject(new Error(f.error?.message||"command failed"));}return;}if(f.type==="meta"){$("machine").textContent="Machine: "+(f.hostname||"unknown");return;}if(f.session_id){currentSession=f.session_id;$("active").textContent="Session "+currentSession+(f.cwd?" — "+f.cwd:"");}if(f.type==="text_delta"){$("log").append(f.delta||"");return;}if(f.type==="error"){notify("tau errored",f.message||"");}if(f.type==="turn_end"&&document.hidden){notify("tau finished","Turn complete");}if(f.type&&f.type.startsWith("ui_"))return uiRequest(f);log(JSON.stringify(f));}
async function listSessions(){try{let r=await cmd("list_sessions",{limit:50});let box=$("sessions");box.innerHTML="";(r.sessions||[]).forEach(s=>{let row=document.createElement("div");row.className="sessrow";let b=document.createElement("button");b.textContent=(s.pinned?"\u{1F4CC} ":"")+(s.name||"(unnamed)")+" — "+s.id+" — "+(s.cwd||"");b.onclick=async()=>{try{await cmd("resume_session",{id:s.id});currentSession=s.id;$("active").textContent="Session "+s.id;}catch(e){status(e.message,false);}};row.appendChild(b);let p=document.createElement("button");p.className="pin";p.textContent=s.pinned?"Unpin":"Pin";p.onclick=async()=>{try{await cmd(s.pinned?"unpin_session":"pin_session",{id:s.id});}finally{listSessions();}};row.appendChild(p);box.appendChild(row);});if(!box.children.length)box.textContent="No sessions.";}catch(e){status(e.message,false);}}
function readFile(f){return new Promise((resolve,reject)=>{let r=new FileReader();r.onload=()=>resolve(String(r.result).split(",")[1]||"");r.onerror=()=>reject(r.error||new Error("read file"));r.readAsDataURL(f);});}
async function uploadFiles(){let ids=[];for(let f of $("files").files){let data=await readFile(f);let r=await cmd("attach_data",{filename:f.name,mime_type:f.type||"application/octet-stream",data});if(r.attachment?.id)ids.push(r.attachment.id);}return ids;}
async function send(type){try{if(!currentSession){status("Open a session first",false);return;}let text=$("message").value;let attachment_ids=await uploadFiles();if(!text&&!attachment_ids.length)return;await cmd(type,{text,attachment_ids});$("message").value="";$("files").value="";$("attachments").textContent="";}catch(e){status(e.message,false);}}
let uiTimers=new Map();
function notify(title,body){if(!("Notification" in window))return;if(Notification.permission==="granted"){new Notification(title,{body});return;}if(Notification.permission!=="denied"){Notification.requestPermission().then(p=>{if(p==="granted")new Notification(title,{body});});}}
function uiRequest(f){notify("tau needs input",(f.title||f.type)+(f.message?" — "+f.message:""));clearTimeout(uiTimers.get(f.id));uiTimers.set(f.id,setTimeout(()=>notify("tau still needs input",(f.title||f.type)),60000));let box=$("uiRequests");box.innerHTML="";let d=document.createElement("div");d.className="card";d.innerHTML="<strong>"+escapeHtml(f.title||f.type)+"</strong><p>"+escapeHtml(f.message||f.placeholder||"")+"</p>";let answer=v=>{clearTimeout(uiTimers.get(f.id));uiTimers.delete(f.id);cmd("ui_response",{id:f.id,value:v});};if(f.type==="ui_confirm"){["true","false"].forEach(v=>{let b=document.createElement("button");b.textContent=v==="true"?"Approve":"Reject";b.onclick=()=>answer(v);d.appendChild(b);});}else{let i=document.createElement("input");i.placeholder=f.placeholder||"response";d.appendChild(i);let b=document.createElement("button");b.textContent="Send response";b.onclick=()=>answer(i.value);d.appendChild(b);}box.appendChild(d);}
window.addEventListener("load",()=>{if("serviceWorker"in navigator)navigator.serviceWorker.register("/sw.js").catch(()=>{});$("url").value=localStorage.tauWsURL||"";$("token").value=localStorage.tauWsToken||"";$("connect").onclick=connect;$("disconnect").onclick=()=>ws&&ws.close();$("refresh").onclick=listSessions;$("send").onclick=()=>send("send_message");$("steer").onclick=()=>send("steer");$("follow").onclick=()=>send("follow_up");$("files").onchange=()=>$("attachments").textContent=Array.from($("files").files).map(f=>f.name).join(", ");$("interrupt").onclick=()=>currentSession?cmd("interrupt").catch(e=>status(e.message,false)):status("Open a session first",false);$("rename").onclick=()=>currentSession?cmd("set_session_name",{name:$("renameText").value}).then(()=>listSessions()).catch(e=>status(e.message,false)):status("Open a session first",false);});`

const webManifest = `{"name":"tau remote","short_name":"tau","start_url":"/","display":"standalone","background_color":"#111827","theme_color":"#111827"}`

const serviceWorkerJS = `self.addEventListener("install",e=>e.waitUntil(caches.open("tau-remote-v1").then(c=>c.addAll(["/","/app.js","/manifest.webmanifest"]))));self.addEventListener("fetch",e=>e.respondWith(fetch(e.request).catch(()=>caches.match(e.request))));`

const coordinatedPanels = `<div class="card"><h2>Running instances</h2><div class="row"><label>Project directory<input id="cwd" placeholder="Absolute path"></label><button id="start">Start instance</button></div><div id="instances" class="sessions muted">Connect to list running instances.</div></div>
<div class="card"><h2>Saved sessions</h2><div id="sessions" class="muted"></div><button id="refresh" class="secondary">Refresh saved sessions</button></div>
<div class="card"><h2>Conversation</h2><div class="muted" id="active"></div><div id="queue" class="muted"></div><div id="history" class="log"></div><textarea id="message" placeholder="Message"></textarea><input id="files" type="file" multiple><div id="attachments" class="muted"></div><div class="row"><button id="send">Send</button><button id="follow" class="secondary">Follow up</button><button id="interrupt" class="secondary">Interrupt</button><button id="detach" class="secondary">Detach</button><button id="stop" class="secondary">Stop instance</button></div></div>
<div class="card"><h2>UI requests</h2><div id="uiRequests" class="muted">None</div></div>
<div class="card"><h2>Live activity</h2><div id="log" class="log"></div></div>`

func registerWebApp(mux *http.ServeMux) {
	registerWebAppAssets(mux, webAppHTML, webAppJS)
}

func registerCoordinatedWebApp(mux *http.ServeMux) {
	shell, _, _ := strings.Cut(webAppHTML, `<div class="card"><h2>Sessions`)
	shell = strings.ReplaceAll(shell, `<label>WebSocket URL<input id="url" placeholder="ws://127.0.0.1:8080/rpc"></label>`, "")
	shell = strings.ReplaceAll(shell, `<button id="refresh" class="secondary">Refresh sessions</button>`, "")
	registerWebAppAssets(mux, shell+coordinatedPanels+`
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
