package server

const coordinatedWebAppJS = `(() => {
"use strict";
const $ = id => document.getElementById(id);
// Live frames staged while a snapshot or replay loads are bounded by count and
// bytes; overflowing discards them and catches up from the loaded position.
const STAGE_FRAMES = 1024, STAGE_BYTES = 4 << 20, REPLAY_PAGES = 16, SYNC_ATTEMPTS = 4;
const HISTORY_MAX = 1000, LIVE_TEXT_MAX = 64 << 10, LIVE_ROWS = 64, LOG_ROWS = 300;
const OUTCOMES_MAX = 20, LOST_STARTS_MAX = 16;
const RECONNECT_BASE_MS = 500, RECONNECT_MAX_MS = 30000, RECONNECT_ATTEMPTS = 10;
const instances = new Map(), drafts = new Map(), pending = new Map(), lostStarts = new Map();
const uploads = new WeakMap();
let ws, seq = 0, links = 0, ready = false, view = null, intended = null, outcomes = [], switching = Promise.resolve();
// watching is set once this connection's baseline is applied; generation
// identifies the coordinator process that produced it.
let launchIDVersion = 0, watching = false, generation = "", manual = false, fatal = "", retries = 0, retryTimer = 0;

function failure(message, fields = {}) { return Object.assign(new Error(message), fields); }
function uuid4() {
    if (crypto.randomUUID) return crypto.randomUUID();
    const b = crypto.getRandomValues(new Uint8Array(16));
    b[6] = (b[6] & 0x0f) | 0x40;
    b[8] = (b[8] & 0x3f) | 0x80;
    return uuidString(b);
}
function launchRequestID() {
    const b = crypto.getRandomValues(new Uint8Array(16));
    let ms = Date.now();
    for (let i = 5; i >= 0; i--, ms = Math.floor(ms / 256)) b[i] = ms % 256;
    b[6] = (b[6] & 0x0f) | 0x70;
    b[8] = (b[8] & 0x3f) | 0x80;
    return uuidString(b);
}
function uuidString(b) {
    const h = Array.from(b, x => x.toString(16).padStart(2, "0")).join("");
    return h.slice(0, 8) + "-" + h.slice(8, 12) + "-" + h.slice(12, 16) + "-" + h.slice(16, 20) + "-" + h.slice(20);
}
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
// Errors carry local (never sent), unknown (sent, no definite reply), or
// rejected (the worker definitely refused it).
function call(type, fields = {}) {
    if (!ready || ws?.readyState !== WebSocket.OPEN) return Promise.reject(failure("Not connected", {local: true}));
    if (pending.size >= 32 || ws.bufferedAmount > 1024 * 1024) return Promise.reject(failure("Connection is busy", {local: true}));
    const id = "web-" + ++seq;
    return new Promise((resolve, reject) => {
        const timer = setTimeout(() => {
            pending.delete(id);
            reject(failure(type + ": no reply; outcome unknown", {code: "outcome_unknown", unknown: true}));
        }, 30000);
        pending.set(id, {resolve, reject, timer});
        try { ws.send(JSON.stringify({type, id, ...fields})); }
        catch (err) { clearTimeout(timer); pending.delete(id); reject(failure(err.message, {local: true})); }
    });
}
async function command(id, type, args = {}, request_id) {
    const fields = {instance_id: id, command: {type, args}};
    if (request_id) fields.request_id = request_id;
    const {result} = await call("command", fields);
    if (result?.type !== "response") return result || {};
    if (!result.ok) throw failure(result.error?.message || "Command failed", {code: result.error?.code || "", rejected: true});
    return result.result || {};
}
// Each mutation gets its own random request_id. An unknown outcome is shown
// and never resent automatically.
async function mutate(label, run) {
    const request_id = uuid4();
    try { return await run(request_id); }
    catch (err) {
        if (err.unknown) noteOutcome(request_id, label + ": outcome unknown (request " + request_id + "). It was not resent.");
        throw err;
    }
}
function noteOutcome(id, text) {
    const found = outcomes.find(o => o.id === id);
    if (found) found.text = text;
    else outcomes.push({id, text});
    if (outcomes.length > OUTCOMES_MAX) outcomes.shift();
    renderOutcomes();
}
function renderOutcomes() {
    const box = $("outcomes");
    box.replaceChildren();
    for (const o of outcomes) {
        line(box, o.text).appendChild(button("Dismiss", () => { outcomes = outcomes.filter(x => x !== o); renderOutcomes(); }));
    }
}
// connect is an explicit user request, so it resets the backoff.
function connect() {
    manual = false;
    retries = 0;
    clearTimeout(retryTimer);
    retryTimer = 0;
    open();
}
function disconnect() {
    manual = true;
    clearTimeout(retryTimer);
    retryTimer = 0;
    if (ws && ws.readyState !== WebSocket.CLOSED) ws.close(1000, "disconnect");
    else status("Disconnected");
}
// An abnormal close retries with capped, jittered exponential backoff. Only
// connection setup and reads are automatic; mutations are never resent.
function reconnectLater(kept) {
    if (retries >= RECONNECT_ATTEMPTS) {
        status("Connection lost; stopped reconnecting after " + retries + " attempts. Connect to retry." + kept);
        return;
    }
    const delay = Math.round(Math.min(RECONNECT_MAX_MS, RECONNECT_BASE_MS * 2 ** retries) * (0.5 + Math.random() / 2));
    retries++;
    status("Connection lost; reconnecting in " + Math.ceil(delay / 1000) + "s." + kept);
    retryTimer = setTimeout(() => Promise.resolve().then(open).catch(report), delay);
}
function open() {
    retryTimer = 0;
    if (ws && ws.readyState !== WebSocket.CLOSED) return;
    const token = $("token").value.trim();
    if (!token) throw new Error("Enter a token first");
    fatal = "";
    const url = new URL("/rpc", location.href);
    url.protocol = location.protocol === "https:" ? "wss:" : "ws:";
    const proto = ["tau.coord.v1"];
    if (/^[A-Za-z0-9!#$%&'*+.^_+-]+$/.test(token)) proto.push("tau.token." + token);
    else url.searchParams.set("token", token);
    const socket = ws = new WebSocket(url, proto);
    status("Connecting…");
    socket.onclose = e => {
        if (ws !== socket) return;
        ready = false;
        watching = false;
        for (const {timer, reject} of pending.values()) {
            clearTimeout(timer);
            reject(failure("Connection closed before reply; outcome unknown", {code: "outcome_unknown", unknown: true}));
        }
        pending.clear();
        // Keep the view, its position, drafts, and lost launch markers so a
        // reconnect can resume the intended instance from where it was.
        if (view) unlink(view);
        instances.clear();
        renderInstances();
        renderActive();
        const kept = intended ? " Drafts are kept." : "";
        if (manual) status(intended ? "Disconnected; connect again to resume. Drafts are kept." : "Disconnected");
        else if (fatal || [1002, 1003, 1008].includes(e?.code)) status((fatal || "Connection refused: " + (e.reason || e.code)) + ". Connect again to retry." + kept);
        else reconnectLater(kept);
    };
    socket.onmessage = ({data}) => {
        if (ws !== socket) return;
        try { frame(JSON.parse(data), socket, data.length); } catch (err) { report(err); }
    };
}
function frame(f, socket, size) {
    if (f.type === "welcome") {
        if (f.role !== "client" || f.version !== 1) { fatal = "Unsupported coordinator protocol"; socket.close(); return; }
        launchIDVersion = f.launch_request_version;
        ready = true;
        status("Connected");
        // A watch lost to a close is retried by reconnecting; keep that status.
        call("watch_instances").then(r => { if (ws === socket && ready) rediscover(r, f.generation || ""); })
            .catch(err => { if (ws === socket && ready) report(err); });
        return;
    }
    if (f.type === "response") {
        const p = pending.get(f.id);
        if (!p) return;
        pending.delete(f.id);
        clearTimeout(p.timer);
        if (f.error) p.reject(failure(f.error.message || f.error.code, {code: f.error.code, unknown: f.error.code === "outcome_unknown"}));
        else p.resolve(f);
        return;
    }
    if (f.type === "instance_update") {
        if (!watching || (f.generation && generation && f.generation !== generation)) return;
        if (f.instance) instances.set(f.instance_id, f.instance);
        else if (f.error) instances.delete(f.instance_id);
        reconcileStarts();
        if (intended && f.instance_id === intended) linkChanged();
        renderInstances();
        renderActive();
        return;
    }
    const v = view;
    if (f.type !== "event" || !v || f.instance_id !== v.id || !v.subscribed) return;
    // Worker events carry a position; link and summary events do not.
    if (!f.position) { unpositioned(v, f); return; }
    if (v.syncing) { stage(v, f, size); return; }
    if (v.failed) return;
    follow(v, apply(v, f));
}
// Each watch response is a complete baseline, and updates follow it in
// order. A new generation means the coordinator restarted: surviving workers
// re-register over time, and the intended one is reattached when it does.
function rediscover(r, welcomed) {
    const next = r.generation || welcomed, restarted = !!(generation && next && next !== generation);
    generation = next;
    watching = true;
    retries = 0;
    instances.clear();
    for (const inst of r.instances || []) instances.set(inst.instance_id, inst);
    reconcileStarts();
    renderInstances();
    renderActive();
    refreshSaved();
    if (restarted) status("Coordinator restarted; waiting for instances to register again.");
    resume();
}
// Summaries without link_state come from coordinators that drop lost workers.
function connected(inst) { return !!inst && (!inst.link_state || inst.link_state === "connected"); }
function unlink(v) {
    v.subscribed = false;
    v.syncing = false;
    v.link = ++links;
}
function linkChanged() {
    const v = view, inst = instances.get(intended);
    if (!connected(inst) && v?.id === intended && v.subscribed) {
        unlink(v);
        note(v, "Instance " + (inst?.link_state || "link lost") + "; it is reattached if it returns.");
    }
    resume();
}
function unpositioned(v, f) {
    if (f.error?.code === "worker_link_lost" || (f.instance && !connected(f.instance))) {
        unlink(v);
        renderActive();
        renderInstances();
        note(v, f.error?.message || f.error?.code || "Instance " + f.instance.link_state);
        return;
    }
    if (f.error) { note(v, f.error.message || f.error.code); resync(v, "snapshot"); return; }
    const session = f.instance?.session_id || f.session_id;
    if (session && v.session && session !== v.session) resync(v, "snapshot");
}
// resume only attaches and reads; it never sends a mutation.
function resume() {
    if (!intended || !ready || view?.subscribed) return;
    const inst = instances.get(intended);
    if (connected(inst)) { select(intended).catch(report); return; }
    if (inst?.link_state === "exited") status("Instance " + intended + " exited; its draft is kept.");
    else if (inst) status("Instance " + intended + " is disconnected; it is reattached if it returns. Its draft is kept.");
    else status("Instance " + intended + " is not running; it is reattached if it registers. Its draft is kept.");
}
// A lost start is reconciled only from instance summaries; it is never retried.
function reconcileStarts() {
    for (const inst of instances.values()) {
        const rid = inst.launch_request_id;
        if (!rid || !lostStarts.has(rid)) continue;
        lostStarts.delete(rid);
        noteOutcome(rid, "Start in " + (inst.cwd || "?") + " (request " + rid + ") did start instance " + inst.instance_id + ".");
    }
}
function isCurrent(v, link) { return ready && view === v && v.link === link && v.subscribed; }
function stage(v, f, size) {
    if (v.overflow) return;
    if (v.stage.length >= STAGE_FRAMES || v.stageBytes + size > STAGE_BYTES) {
        v.overflow = true;
        v.stage = [];
        v.stageBytes = 0;
        return;
    }
    v.stage.push(f);
    v.stageBytes += size;
}
// apply returns ok, dup, gap, or snapshot. Unknown epochs, omitted events,
// and session changes invalidate the view.
function apply(v, f) {
    const p = f.position, cur = v.position;
    if (!cur || !p || p.instance_id !== v.id || p.epoch !== cur.epoch) return "snapshot";
    const offset = Number(p.offset);
    if (!Number.isSafeInteger(offset)) return "snapshot";
    if (offset <= cur.offset) return "dup";
    if (offset !== cur.offset + 1) return "gap";
    const e = f.event;
    if (!e || typeof e !== "object" || ["event_omitted", "context_edited", "compaction_end"].includes(e.type)) return "snapshot";
    if (f.session_id && v.session && f.session_id !== v.session) return "snapshot";
    cur.offset = offset;
    event(v, e);
    return "ok";
}
function follow(v, result) {
    if (result === "gap") resync(v, "replay");
    else if (result === "snapshot") { status("Event stream changed; resynchronizing…"); resync(v, "snapshot"); }
}
function beginStaging(v) {
    v.syncing = true;
    v.failed = false;
    v.stage = [];
    v.stageBytes = 0;
    v.overflow = false;
    v.dirty = false;
    renderActive();
}
function resync(v, mode) {
    if (v.syncing) { if (v.dirty !== "snapshot") v.dirty = mode; return; }
    beginStaging(v);
    runSync(v, v.link, mode).catch(report);
}
// runSync prefers replaying from the retained position and falls back to a
// snapshot on a gap, omission, or invalidation, then drains staged frames.
async function runSync(v, link, mode) {
    v.dirty = v.dirty === "snapshot" ? "snapshot" : mode;
    try {
        for (let attempt = 0; v.dirty; attempt++) {
            if (attempt === SYNC_ATTEMPTS) throw new Error("Could not resynchronize; select the instance to retry");
            const want = v.dirty;
            v.dirty = false;
            const replayed = want === "replay" && v.position !== null && await replay(v, link);
            if (!isCurrent(v, link)) return;
            if (!replayed) {
                try { await snapshot(v, link); }
                catch (err) {
                    if (!["busy", "resync_required"].includes(err.code)) throw err;
                    v.dirty = "snapshot";
                    await new Promise(resolve => setTimeout(resolve, 50 * (attempt + 1)));
                }
                if (!isCurrent(v, link)) return;
                if (v.dirty === "snapshot") continue;
            }
            if (v.overflow) { v.overflow = false; v.dirty = v.dirty || "replay"; }
            const staged = v.stage;
            v.stage = [];
            v.stageBytes = 0;
            if (v.dirty) continue;
            for (const f of staged) {
                const r = apply(v, f);
                if (r === "gap" || r === "snapshot") { v.dirty = r === "gap" ? "replay" : "snapshot"; break; }
            }
        }
        if (isCurrent(v, link)) { v.syncing = false; renderActive(); }
    } catch (err) {
        if (!isCurrent(v, link)) return;
        v.syncing = false;
        v.failed = true;
        renderActive();
        throw err;
    }
}
async function replay(v, link) {
    for (let page = 0; page < REPLAY_PAGES; page++) {
        let r;
        try { r = await command(v.id, "replay_events", {after: {...v.position}}); }
        catch (err) {
            if (err.unknown || err.local) throw err;
            return false;
        }
        if (!isCurrent(v, link)) return true;
        const events = r.events || [];
        for (const f of events) {
            const result = apply(v, f);
            if (result !== "ok" && result !== "dup") return false;
        }
        if (!r.more) return true;
        if (!events.length) return false;
    }
    return false;
}
async function snapshot(v, link) {
    const s = await command(v.id, "get_snapshot");
    if (!isCurrent(v, link)) return;
    const p = s.position, offset = Number(p?.offset);
    if (!p || p.instance_id !== v.id || typeof p.epoch !== "string" || !Number.isSafeInteger(offset)) throw new Error("Snapshot has no valid position");
    v.position = {instance_id: p.instance_id, epoch: p.epoch, offset};
    v.session = s.instance?.session_id || s.state?.session_id || "";
    resetChat(v);
    setHistory(v, s.history);
    showState(v, s.state);
    showLive(v, s.live);
    for (const d of s.ui_requests || []) v.chat.dialogs.set(d.id, {...d, method: d.method || String(d.type || "").replace(/^ui_/, "")});
    renderDialogs(v);
}
function renderInstances() {
    const box = $("instances");
    box.replaceChildren();
    const selected = view?.id ?? intended;
    for (const i of instances.values()) {
        const needs = i.pending_inputs ? " — Needs input (" + i.pending_inputs + ")" : "";
        const shown = connected(i) ? (i.state || "ready") + needs : i.link_state;
        const b = button(i.cwd + " — " + i.session_id + " — " + shown, () => select(i.instance_id));
        b.setAttribute("aria-pressed", String(i.instance_id === selected));
        b.title = i.node_id + "/" + i.instance_id;
        box.appendChild(b);
    }
    if (!box.firstChild) box.textContent = ready ? "No running instances." : "Connect to list running instances.";
}
function renderActive() {
    const v = view;
    let text = "No attached instance";
    if (v) {
        text = v.session || v.id;
        const st = v.state || {};
        if (st.model) text += " — " + st.model + (st.thinking_level ? " (" + st.thinking_level + ")" : "");
        if (st.is_streaming) text += " — working";
        const inst = instances.get(v.id);
        if (!v.subscribed) text += " — " + (inst && !connected(inst) ? inst.link_state : "not attached");
        else if (v.syncing) text += " — synchronizing…";
        else if (v.failed) text += " — out of sync; select the instance to retry";
    }
    $("active").textContent = text;
    const synced = !!(v?.subscribed && !v.syncing && !v.failed);
    $("send").disabled = $("follow").disabled = !synced;
    $("interrupt").disabled = $("stop").disabled = !v?.subscribed;
}
function draft(id) {
    if (!drafts.has(id)) drafts.set(id, {text: "", files: [], textRevision: 0, filesRevision: 0, uncertain: ""});
    return drafts.get(id);
}
function showAttachments() {
    const d = view ? draft(view.id) : null;
    const names = d ? d.files.map(f => f.name).join(", ") : "";
    $("attachments").textContent = d?.uncertain ? (names ? names + " — " : "") + d.uncertain : names;
}
function showDraft() {
    $("message").value = view ? draft(view.id).text : "";
    $("files").value = "";
    $("message").disabled = $("files").disabled = !view;
    showAttachments();
}
function clearView() {
    $("log").replaceChildren();
    $("history").replaceChildren();
    $("uiRequests").textContent = "None";
    $("queue").textContent = "";
}
function resetChat(v) {
    const liveBox = document.createElement("div"), eventBox = document.createElement("div");
    v.chat = {dialogs: new Map(), history: [], before: "", loadingOlder: false, messages: new Map(), tools: new Map(), bash: new Map(), liveBox, eventBox};
    clearView();
    $("log").append(liveBox, eventBox);
}
function select(id) {
    intended = id;
    // Serialize attach/detach so rapid A→B→A never accepts A's old subscription.
    const current = switching.then(async () => {
        if (view?.id === id && view.subscribed && !view.failed) return;
        const previous = view;
        if (previous && previous.id !== id) {
            if (previous.subscribed && ready) await call("detach", {instance_id: previous.id});
            previous.subscribed = false;
            previous.link = ++links;
        }
        if (!id) { view = null; clearView(); }
        else if (previous?.id !== id) {
            view = {id, subscribed: false, syncing: false, failed: false, session: "", position: null, state: {}, link: 0};
            resetChat(view);
        }
        showDraft();
        renderActive();
        renderInstances();
        if (!view || !ready) return;
        if (instances.has(view.id) && !connected(instances.get(view.id))) { resume(); return; }
        const v = view, link = v.link = ++links;
        v.subscribed = true;
        // Stage from before attach so no event between attach and load is lost.
        beginStaging(v);
        let attach;
        try { attach = await call("attach", {instance_id: v.id}); }
        catch (err) {
            if (v.link === link) { v.subscribed = false; v.syncing = false; renderActive(); }
            throw err;
        }
        if (!isCurrent(v, link)) return;
        const retained = v.position !== null && attach.session_id === v.session;
        runSync(v, link, retained ? "replay" : "snapshot").catch(report);
        refreshSaved();
    });
    switching = current.catch(() => {});
    return current;
}
function messageRole(m) { return m?.role || m?.Role || "message"; }
function messageText(m) {
    if (!m) return "";
    if (typeof m === "string") return m;
    const c = m.content ?? m.Content ?? m.text ?? m.Text;
    if (typeof c === "string") return c;
    if (Array.isArray(c)) return c.map(b => typeof b === "string" ? b : (b?.text ?? b?.Text ?? (b?.type ? "[" + b.type + "]" : ""))).join("");
    return "";
}
function setHistory(v, h = {}) {
    const s = v.chat, entries = h.entries || [];
    s.history = entries.slice(-HISTORY_MAX);
    s.before = h.before || "";
    if (entries.length > HISTORY_MAX) s.before = s.history[0].entry_id || s.before;
    renderHistory(v);
}
function addHistory(v, entry) {
    const s = v.chat;
    if (entry.entry_id && s.history.some(h => h.entry_id === entry.entry_id)) return;
    s.history.push(entry);
    if (s.history.length > HISTORY_MAX) {
        s.history.splice(0, s.history.length - HISTORY_MAX);
        s.before = s.history[0].entry_id || s.before;
    }
    renderHistory(v);
}
function renderHistory(v) {
    const s = v.chat, content = document.createDocumentFragment();
    if (s.before) {
        const full = s.history.length >= HISTORY_MAX;
        const older = button(full ? "History view is full" : s.loadingOlder ? "Loading…" : "Load older messages", () => loadOlder(v));
        older.className = "secondary";
        older.disabled = full || s.loadingOlder;
        content.appendChild(older);
    }
    for (const h of s.history) line(content, h.omitted ? "[message too large to show]" : messageRole(h.message) + ": " + messageText(h.message));
    $("history").replaceChildren(content);
}
async function loadOlder(v) {
    const s = v.chat, session = v.session, before = s.before;
    if (!before || s.loadingOlder || s.history.length >= HISTORY_MAX) return;
    s.loadingOlder = true;
    renderHistory(v);
    try {
        const r = await command(v.id, "get_history", {session_id: session, before});
        if (view !== v || v.chat !== s || v.session !== session || s.before !== before) return;
        const seen = new Set(s.history.map(h => h.entry_id).filter(Boolean));
        const older = (r.entries || []).filter(h => !h.entry_id || !seen.has(h.entry_id));
        s.history = older.slice(-(HISTORY_MAX - s.history.length)).concat(s.history);
        s.before = r.before || "";
    } finally {
        s.loadingOlder = false;
        if (view === v && v.chat === s) renderHistory(v);
    }
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
function showState(v, st = {}) {
    v.state = st || {};
    showQueue(v.state.queue || {});
    renderActive();
}
function note(v, text) {
    const box = v.chat?.eventBox || $("log");
    line(box, text);
    if (box.childElementCount > LOG_ROWS) box.firstElementChild.remove();
}
function clip(text) { return text.length > LIVE_TEXT_MAX ? text.slice(-LIVE_TEXT_MAX) : text; }
function liveItem(v, kind, id, init) {
    const map = v.chat[kind];
    let item = map.get(id);
    if (!item) {
        item = {...init, truncated: false, row: line(v.chat.liveBox, "")};
        map.set(id, item);
        if (map.size > LIVE_ROWS) {
            const [oldID, old] = map.entries().next().value;
            old.row.remove();
            map.delete(oldID);
        }
    }
    return item;
}
function appendLive(item, field, delta) {
    const next = item[field] + (delta || "");
    if (next.length > LIVE_TEXT_MAX) item.truncated = true;
    item[field] = clip(next);
}
function drawLive(kind, item) {
    let text;
    if (kind === "messages") text = (item.role || "assistant") + ": " + (item.thinking ? "[thinking] " + item.thinking.slice(-400) + "\n" : "") + item.text;
    else if (kind === "tools") text = "tool " + (item.name || "") + (item.input ? " " + item.input.slice(0, 400) : "") + (item.progress ? "\n" + item.progress : "");
    else text = "bash: " + item.text;
    item.row.textContent = (item.truncated ? "… " : "") + text;
}
function dropLive(v, kind, id) {
    const item = v.chat[kind].get(id);
    if (!item) return;
    item.row.remove();
    v.chat[kind].delete(id);
}
function showLive(v, l) {
    l = l || {};
    for (const m of l.messages || []) {
        const item = liveItem(v, "messages", m.id, {role: m.role});
        Object.assign(item, {text: clip(m.text || ""), thinking: clip(m.thinking || ""), truncated: !!m.truncated});
        drawLive("messages", item);
    }
    for (const t of l.tools || []) {
        const item = liveItem(v, "tools", t.id, {name: t.name});
        Object.assign(item, {input: String(t.input || ""), progress: clip(t.progress || ""), truncated: !!t.truncated});
        drawLive("tools", item);
    }
    for (const b of l.bash || []) {
        const item = liveItem(v, "bash", b.id, {});
        Object.assign(item, {text: clip(b.text || ""), truncated: !!b.truncated});
        drawLive("bash", item);
    }
    if (l.truncated) note(v, "Some live activity was too large to restore.");
}
function event(v, e) {
    const s = v.chat;
    switch (e.type) {
    case "queue_update": v.state = {...v.state, queue: e}; showQueue(e); break;
    case "ui_confirm": case "ui_select": case "ui_input":
        s.dialogs.set(e.id, {...e, method: e.method || e.type.slice(3)});
        renderDialogs(v);
        break;
    case "ui_resolved":
        s.dialogs.delete(e.id);
        renderDialogs(v);
        break;
    case "ui_set_editor_text":
        note(v, "An editor suggestion was received; your draft was not changed.");
        break;
    case "model_change":
        showState(v, {...v.state, model: e.model});
        break;
    case "thinking_level_change":
        showState(v, {...v.state, thinking_level: e.level});
        break;
    case "agent_start": case "agent_end":
        if (e.type === "agent_end") {
            for (const kind of ["messages", "tools"]) for (const id of s[kind].keys()) dropLive(v, kind, id);
        }
        v.state = {...v.state, is_streaming: e.type === "agent_start"};
        renderActive();
        break;
    case "message_start":
        drawLive("messages", liveItem(v, "messages", e.message_id, {role: e.role, text: "", thinking: ""}));
        break;
    case "text_delta": case "thinking_delta": {
        const item = liveItem(v, "messages", e.message_id, {role: "assistant", text: "", thinking: ""});
        appendLive(item, e.type === "text_delta" ? "text" : "thinking", e.delta);
        drawLive("messages", item);
        break;
    }
    case "message_end":
        dropLive(v, "messages", e.message_id);
        addHistory(v, {entry_id: e.entry_id || "", message: {role: e.role, content: e.content}});
        break;
    case "tool_call":
        drawLive("tools", liveItem(v, "tools", e.tool_call_id, {name: e.name, input: String(e.input || ""), progress: ""}));
        break;
    case "tool_progress": {
        const item = liveItem(v, "tools", e.tool_call_id, {name: "", input: "", progress: ""});
        item.truncated = String(e.chunk || "").length > LIVE_TEXT_MAX;
        item.progress = clip(String(e.chunk || ""));
        drawLive("tools", item);
        break;
    }
    case "tool_result": {
        const name = s.tools.get(e.tool_call_id)?.name || e.tool_call_id;
        dropLive(v, "tools", e.tool_call_id);
        note(v, "tool_result — " + name + (e.is_error ? " (error)" : "") + (e.output ? ": " + String(e.output).slice(0, 500) : ""));
        break;
    }
    case "bash_execution_end": dropLive(v, "bash", e.id); break;
    case "bash_execution_update": {
        const item = liveItem(v, "bash", e.id, {text: ""});
        appendLive(item, "text", e.delta);
        drawLive("bash", item);
        break;
    }
    default:
        note(v, e.type + (e.name ? " — " + e.name : "") + (e.output || e.chunk || e.delta || e.message ? ": " + (e.output || e.chunk || e.delta || e.message) : ""));
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
            await mutate("Response to " + (d.title || "request"), request_id => command(v.id, "ui_response", {id: d.id, method: d.method, ...fields}, request_id));
            if (view !== v || chat !== v.chat) return;
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
    if (!v?.subscribed || v.syncing || !v.session) throw new Error("Attach to an instance first");
    const d = draft(v.id), {text, files, textRevision, filesRevision} = d, session = v.session;
    const expected_session_id = session;
    const checkTarget = () => {
        if (!ready || instances.get(v.id)?.session_id !== session) throw new Error("Session changed; draft kept");
    };
    if (d.sending || (!text && !files.length)) return;
    d.sending = true;
    d.uncertain = "";
    let sending = false;
    try {
        const attachment_ids = [];
        for (const file of files) {
            const cached = uploads.get(file);
            if (cached?.session === session) {
                attachment_ids.push(cached.id);
                continue;
            }
            const data = await readFile(file);
            checkTarget();
            const r = await mutate("Upload " + file.name, request_id => command(v.id, "attach_data", {filename: file.name, mime_type: file.type || "application/octet-stream", data, expected_session_id}, request_id));
            uploads.set(file, {session, id: r.attachment.id});
            attachment_ids.push(r.attachment.id);
        }
        checkTarget();
        sending = true;
        await mutate(type === "follow_up" ? "Follow-up" : "Message", request_id => command(v.id, type, {text, attachment_ids, expected_session_id}, request_id));
        // Text and files clear independently, only if unchanged since send.
        if (textRevision === d.textRevision) d.text = "";
        if (filesRevision === d.filesRevision) d.files = [];
        if (view?.id === v.id) showDraft();
    } catch (err) {
        if (err.unknown) d.uncertain = (sending ? "Send" : "Upload") + " outcome unknown; draft kept and not resent.";
        // A definite rejection may have consumed the uploads; upload again next time.
        else if (sending && err.rejected) for (const file of files) uploads.delete(file);
        throw err;
    } finally {
        d.sending = false;
        if (view?.id === v.id) showAttachments();
    }
}
function on(id, action) { $(id).onclick = () => Promise.resolve().then(action).catch(report); }
on("connect", connect);
on("disconnect", disconnect);
on("refresh", refreshSaved);
on("start", async () => {
    const cwd = $("cwd").value.trim(), request_id = launchIDVersion === 7 ? launchRequestID() : uuid4();
    let r;
    try { r = await call("start_instance", {cwd, request_id}); }
    catch (err) {
        if (err.unknown) {
            lostStarts.set(request_id, cwd);
            if (lostStarts.size > LOST_STARTS_MAX) lostStarts.delete(lostStarts.keys().next().value);
            noteOutcome(request_id, "Start in " + cwd + ": outcome unknown (request " + request_id + "). It was not retried; it is reported here if it appears.");
            reconcileStarts();
        }
        throw err;
    }
    await select(r.instance.instance_id);
});
on("detach", () => select(null));
on("stop", () => {
    const v = view;
    if (!v?.subscribed) return;
    return mutate("Stop instance", request_id => call("stop_instance", {instance_id: v.id, request_id})).then(r => {
        if (r.result?.ok === false) throw new Error(r.result.error?.message || "Stop failed");
    });
});
on("send", () => send("send_message"));
on("follow", () => send("follow_up"));
on("interrupt", () => {
    const v = view;
    if (!v?.subscribed) return;
    return mutate("Interrupt", request_id => command(v.id, "interrupt", {}, request_id));
});
$("message").oninput = () => { if (view) { const d = draft(view.id); d.text = $("message").value; d.textRevision++; } };
$("files").onchange = () => {
    if (!view) return;
    const d = draft(view.id);
    d.files = Array.from($("files").files);
    d.filesRevision++;
    showAttachments();
};
showDraft();
renderActive();
renderOutcomes();
})();`
