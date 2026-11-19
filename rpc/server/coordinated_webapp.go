package server

const coordinatedWebAppJS = `(() => {
"use strict";
const $ = id => document.getElementById(id);
const instances = new Map(), drafts = new Map(), pending = new Map();
const uploads = new WeakMap();
let ws, seq = 0, ready = false, view, switching = Promise.resolve();

function button(text, action) {
    const b = document.createElement("button");
    b.textContent = text;
    b.onclick = () => Promise.resolve().then(action).catch(report);
    return b;
}
function status(text) { $("status").textContent = text; }
function report(err) { status(err.message); }
function line(box, text) {
    const row = document.createElement("div");
    row.className = "event";
    row.textContent = text;
    box.appendChild(row);
    return row;
}
function call(type, fields = {}) {
    if (!ready || ws?.readyState !== WebSocket.OPEN) return Promise.reject(new Error("Not connected"));
    if (pending.size >= 32 || ws.bufferedAmount > 1024 * 1024) return Promise.reject(new Error("Connection is busy"));
    const id = "web-" + ++seq;
    return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
            pending.delete(id);
            reject(new Error(type + ": no reply; outcome unknown"));
        }, 30000);
        pending.set(id, {resolve, reject, timer});
        try { ws.send(JSON.stringify({type, id, ...fields})); }
        catch (err) { clearTimeout(timer); pending.delete(id); reject(err); }
    });
}
async function command(id, type, args = {}) {
    const {result} = await call("command", {instance_id: id, command: {type, args}});
    if (result?.type !== "response") return result || {};
    if (!result.ok) throw new Error(result.error?.message || "Command failed");
    return result.result || {};
}
function connect() {
    const token = $("token").value.trim();
    if (!token) throw new Error("Enter a token first");
    const url = new URL("/rpc", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    const proto = ["tau.coord.v1"];
    if (/^[A-Za-z0-9!#$%&'*+.^_+-]+$/.test(token)) proto.push("tau.token." + token);
    else url.searchParams.set("token", token);
    if (ws && ws.readyState !== WebSocket.CLOSED) return;
    const socket = ws = new WebSocket(url, proto);
    status("Connecting…");
    socket.onclose = () => {
        if (ws !== socket) return;
        ready = false;
        for (const {timer, reject} of pending.values()) {
            clearTimeout(timer);
            reject(new Error("Connection closed before reply; outcome unknown"));
        }
        pending.clear();
        if (view) view.subscribed = false;
        select(null).catch(report);
        instances.clear();
        renderInstances();
        status("Disconnected");
    };
    socket.onmessage = ({data}) => {
        if (ws !== socket) return;
        try { frame(JSON.parse(data), socket); } catch (err) { report(err); }
    };
}
function frame(f, socket) {
    if (f.type === "welcome") {
        if (f.role !== "client" || f.version !== 1) { socket.close(); return; }
        ready = true;
        status("Connected");
        call("watch_instances").then(r => {
            if (ws !== socket || !ready) return;
            instances.clear();
            for (const inst of r.instances || []) instances.set(inst.instance_id, inst);
            renderInstances();
            refreshSaved();
        }).catch(report);
        return;
    }
    if (f.type === "response") {
        const p = pending.get(f.id);
        if (!p) return;
        pending.delete(f.id);
        clearTimeout(p.timer);
        if (f.error) p.reject(new Error(f.error.message || f.error.code));
        else p.resolve(f);
        return;
    }
    if (f.type === "instance_update") {
        if (f.instance) instances.set(f.instance_id, f.instance);
        if (f.error) instances.delete(f.instance_id);
        renderInstances();
        return;
    }
    const v = view;
    if (f.type !== "event" || !v || f.instance_id !== v.id || !v.subscribed) return;
    if (f.error?.code === "worker_link_lost") {
        v.subscribed = false;
        renderActive();
        line($("log"), f.error.message);
        return;
    }
    if (f.instance && f.session_id !== v.session) loadSession(v, f.session_id);
    if (f.session_id !== v.session) return;
    if (f.error) { line($("log"), f.error.message || f.error.code); return; }
    if (f.event) event(v, f.event);
}
function renderInstances() {
    const box = $("instances");
    box.replaceChildren();
    for (const i of instances.values()) {
        const needs = i.pending_inputs ? " — Needs input (" + i.pending_inputs + ")" : "";
        const b = button(i.cwd + " — " + i.session_id + " — " + (i.state || "ready") + needs, () => select(i.instance_id));
        b.setAttribute("aria-pressed", String(i.instance_id === view?.id));
        b.title = i.node_id + "/" + i.instance_id;
        box.appendChild(b);
    }
    if (!box.firstChild) box.textContent = ready ? "No running instances." : "Connect to list running instances.";
}
function renderActive() {
    $("active").textContent = view ? (view.session || view.id) + (view.subscribed ? "" : " — not attached") : "No attached instance";
    for (const id of ["send", "follow", "interrupt", "stop"]) $(id).disabled = !view?.subscribed;
}
function draft(id) {
    if (!drafts.has(id)) drafts.set(id, {text: "", files: [], revision: 0});
    return drafts.get(id);
}
function showDraft() {
    $("message").value = view ? draft(view.id).text : "";
    $("files").value = "";
    $("message").disabled = $("files").disabled = !view;
    $("attachments").textContent = view ? draft(view.id).files.map(f => f.name).join(", ") : "";
}
function select(id) {
    // Serialize attach/detach so rapid A→B→A never accepts A's old subscription.
    const current = switching.then(async () => {
        if (view?.id === id && view.subscribed) return;
        const previous = view;
        if (previous?.subscribed && ready) await call("detach", {instance_id: previous.id});
        view = id ? {id, subscribed: false, session: ""} : null;
        $("log").replaceChildren();
        $("history").replaceChildren();
        $("uiRequests").textContent = "None";
        $("queue").textContent = "";
        showDraft();
        renderActive();
        renderInstances();
        if (!view || !ready) return;
        const v = view;
        const attach = await call("attach", {instance_id: v.id});
        if (!ready || v !== view) return;
        v.subscribed = true;
        loadSession(v, attach.session_id);
        refreshSaved();
    });
    switching = current.catch(() => {});
    return current;
}
function loadSession(v, session) {
    const s = v.chat = {session, dialogs: new Map(), loadingDialogs: true, resolved: new Set(), messages: new Map()};
    v.session = session;
    renderActive();
    $("log").replaceChildren();
    $("queue").textContent = "";
    $("history").replaceChildren();
    renderDialogs(v);
    refreshHistory(v);
    command(v.id, "get_ui_requests").then(r => {
        if (view !== v || s !== v.chat || !v.subscribed) return;
        for (const d of r.requests || []) if (!s.resolved.has(d.id)) s.dialogs.set(d.id, d);
        s.loadingDialogs = false;
        s.resolved.clear();
        renderDialogs(v);
    }).catch(err => { if (view === v && s === v.chat) { s.loadingDialogs = false; s.resolved.clear(); report(err); } });
    command(v.id, "get_state").then(r => {
        if (view === v && s === v.chat && !s.queueSeen && r.session_id === session) showQueue(r.queue || {});
    }).catch(err => { if (view === v && s === v.chat) report(err); });
}
async function refreshHistory(v) {
    const s = v.chat;
    if (s.historyPending) { s.historyDirty = true; return; }
    s.historyPending = true;
    try {
        do {
            s.historyDirty = false;
            const r = await command(v.id, "get_messages");
            if (view !== v || v.chat !== s || !v.subscribed || r.session_id !== s.session) return;
            const content = document.createDocumentFragment();
            for (const m of r.messages || []) {
                line(content, (m.Role || m.role || "message") + ": " + (m.Text || m.text || ""));
            }
            $("history").replaceChildren(content);
        } while (s.historyDirty);
    } catch (err) { if (view === v && v.chat === s && v.subscribed) report(err); }
    finally { s.historyPending = false; }
}
let savedSeq = 0;
async function refreshSaved() {
    const id = view?.subscribed ? view.id : instances.keys().next().value;
    const request = ++savedSeq;
    const box = $("sessions");
    if (!id) { box.textContent = "Start an instance to list saved sessions."; return; }
    try {
        const result = await command(id, "list_sessions", {limit: 50});
        if (request !== savedSeq || !ready) return;
        box.replaceChildren();
        for (const s of result.sessions || []) line(box, (s.name || s.id) + " — " + (s.cwd || ""));
        if (!box.firstChild) box.textContent = "No saved sessions.";
    } catch (err) { if (request === savedSeq && ready) box.textContent = err.message; }
}
function showQueue(q) { $("queue").textContent = "Steering: " + (q.steering || 0) + " / Follow-up: " + (q.follow_up || 0); }
function event(v, e) {
    const s = v.chat, log = $("log");
    switch (e.type) {
    case "queue_update": s.queueSeen = true; showQueue(e); break;
    case "ui_confirm": case "ui_select": case "ui_input":
        s.dialogs.set(e.id, {...e, method: e.method || e.type.slice(3)});
        renderDialogs(v);
        break;
    case "ui_resolved":
        if (s.loadingDialogs) s.resolved.add(e.id);
        s.dialogs.delete(e.id);
        renderDialogs(v);
        break;
    case "ui_set_editor_text":
        line(log, "An editor suggestion was received; your draft was not changed.");
        break;
    case "text_delta": {
        let row = s.messages.get(e.message_id);
        if (!row) { row = line(log, "assistant: "); s.messages.set(e.message_id, row); }
        row.append(e.delta || "");
        break;
    }
    case "message_end":
        s.messages.get(e.message_id)?.remove();
        s.messages.delete(e.message_id);
        refreshHistory(v);
        break;
    case "message_start": case "thinking_delta": break;
    default:
        line(log, e.type + (e.name ? " — " + e.name : "") + (e.output || e.chunk || e.delta || e.message ? ": " + (e.output || e.chunk || e.delta || e.message) : ""));
        if (log.childElementCount > 300) log.firstElementChild.remove();
    }
}
function renderDialogs(v) {
    const box = $("uiRequests");
    // Do not replace inputs for requests that are still pending.
    for (const row of Array.from(box.children)) {
        if (!v.chat.dialogs.has(row.dataset.request)) row.remove();
    }
    if (!v.chat.dialogs.size) { box.textContent = "None"; return; }
    if (!box.firstElementChild) box.replaceChildren();
    for (const d of v.chat.dialogs.values()) {
        if (Array.from(box.children).some(row => row.dataset.request === d.id)) continue;
        const row = line(box, d.title + (d.message ? " — " + d.message : ""));
        row.dataset.request = d.id;
        const chat = v.chat;
        const answer = async fields => {
            if (view !== v || chat !== v.chat || !v.subscribed) return;
            await command(v.id, "ui_response", {id: d.id, method: d.method, ...fields});
            if (view !== v || chat !== v.chat) return;
            if (chat.loadingDialogs) chat.resolved.add(d.id);
            chat.dialogs.delete(d.id);
            renderDialogs(v);
        };
        if (d.method === "confirm") {
            row.append(button("Approve", () => answer({confirmed: true})), button("Reject", () => answer({confirmed: false})));
        } else if (d.method === "select") {
            for (const [index, option] of (d.options || []).entries()) row.appendChild(button(option, () => answer({value: option || String(index)})));
        } else {
            const input = document.createElement("input");
            input.placeholder = d.placeholder || "Response";
            row.append(input, button("Respond", () => answer({value: input.value})));
        }
        row.appendChild(button("Dismiss", () => answer({cancelled: true})));
    }
}
function readFile(file) {
    return new Promise((resolve, reject) => {
        const reader = new FileReader();
        reader.onload = () => resolve(reader.result.split(",", 2)[1]);
        reader.onerror = () => reject(reader.error);
        reader.readAsDataURL(file);
    });
}
async function send(type) {
    const v = view;
    if (!v?.subscribed || !v.session) throw new Error("Attach to an instance first");
    const d = draft(v.id), {revision, text, files} = d, session = v.session;
    const expected_session_id = session;
    const checkTarget = () => {
        if (!ready || instances.get(v.id)?.session_id !== session) throw new Error("Session changed; draft kept");
    };
    if (d.sending || (!text && !files.length)) return;
    d.sending = true;
    try {
        const attachment_ids = [];
        for (const file of files) {
            const previous = uploads.get(file);
            if (previous?.session === session) {
                attachment_ids.push(previous.id);
                continue;
            }
            const data = await readFile(file);
            checkTarget();
            const r = await command(v.id, "attach_data", {filename: file.name, mime_type: file.type || "application/octet-stream", data, expected_session_id});
            const id = r.attachment.id;
            uploads.set(file, {session, id});
            attachment_ids.push(id);
        }
        checkTarget();
        await command(v.id, type, {text, attachment_ids, expected_session_id});
        if (revision === d.revision) {
            d.text = "";
            d.files = [];
            if (view?.id === v.id) showDraft();
        }
    } finally { d.sending = false; }
}
function on(id, action) { $(id).onclick = () => Promise.resolve().then(action).catch(report); }
on("connect", connect);
on("disconnect", () => ws?.close());
on("refresh", refreshSaved);
on("start", async () => {
    const {instance} = await call("start_instance", {cwd: $("cwd").value.trim()});
    await select(instance.instance_id);
});
on("detach", () => select(null));
on("stop", () => view?.subscribed && call("stop_instance", {instance_id: view.id}).then(r => {
    if (r.result?.ok === false) throw new Error(r.result.error?.message || "Stop failed");
}));
on("send", () => send("send_message"));
on("follow", () => send("follow_up"));
on("interrupt", () => view?.subscribed && command(view.id, "interrupt"));
$("message").oninput = () => { if (view) { const d = draft(view.id); d.text = $("message").value; d.revision++; } };
$("files").onchange = () => {
    if (!view) return;
    const d = draft(view.id);
    d.files = Array.from($("files").files);
    d.revision++;
    $("attachments").textContent = d.files.map(f => f.name).join(", ");
};
showDraft();
renderActive();
})();`
