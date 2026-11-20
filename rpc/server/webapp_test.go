package server

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func TestWebAppServesControllerAndAssets(t *testing.T) {
	bridge, err := NewWebSocketBridge("127.0.0.1:0", "secret", &mockRuntime{}, newTestRegistry(), nil)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	defer func() { _ = bridge.listener.Close() }()

	cases := []struct {
		path        string
		contentType string
		want        []string
	}{
		{"/", "text/html", []string{"tau remote", "machine", "width:auto"}},
		{"/app.js", "text/javascript", []string{"list_sessions", "pin_session", "unpin_session", "hostname", "Notification", "ui_response", "Open a session first"}},
		{"/manifest.webmanifest", "application/manifest+json", []string{"standalone"}},
		{"/sw.js", "text/javascript", []string{"tau-remote-v1"}},
	}
	for _, tc := range cases {
		req, err := http.NewRequest(http.MethodGet, tc.path, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		rec := newResponseRecorder()
		bridge.server.Handler.ServeHTTP(rec, req)
		if rec.status != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", tc.path, rec.status)
		}
		if got := rec.header.Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Fatalf("%s content-type = %q, want prefix %q", tc.path, got, tc.contentType)
		}
		for _, want := range tc.want {
			if !strings.Contains(rec.body.String(), want) {
				t.Fatalf("%s body does not contain %q", tc.path, want)
			}
		}
	}
}

func TestCoordinatedWebAppServesInstanceController(t *testing.T) {
	mux := http.NewServeMux()
	registerCoordinatedWebApp(mux)
	for _, tc := range []struct {
		path string
		want []string
		omit []string
	}{
		{
			"/",
			[]string{"Running instances", "Saved sessions", "Start instance", "Detach", "Stop instance", `id="outcomes"`},
			[]string{`id="url"`, `<h2>Sessions</h2>`, `id="steer"`},
		},
		{
			"/app.js",
			[]string{
				"watch_instances", "instance_update", "ui_resolved",
				`"get_snapshot"`, `"get_history"`, "session_id: session, before", `"replay_events"`, "after: {...v.position}",
				"request_id", "launch_request_id", "randomUUID", "outcome_unknown",
				"event_omitted", "worker_link_lost", "STAGE_FRAMES", "STAGE_BYTES", "HISTORY_MAX",
				"textRevision", "filesRevision", "uploads.delete(file)",
				"link_state", "f.generation", "RECONNECT_MAX_MS", "RECONNECT_ATTEMPTS",
				"coordinator_time_unix_ms", "clock_skew",
			},
			[]string{`"get_messages"`, `"get_ui_requests"`, `"get_state"`},
		},
	} {
		body := serveCoordinatedAsset(t, mux, tc.path)
		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", tc.path, want)
			}
		}
		for _, omit := range tc.omit {
			if strings.Contains(body, omit) {
				t.Fatalf("%s unexpectedly contains %q", tc.path, omit)
			}
		}
	}
}

func TestCoordinatedWebAppScriptTargetsExistingElements(t *testing.T) {
	mux := http.NewServeMux()
	registerCoordinatedWebApp(mux)
	page, script := serveCoordinatedAsset(t, mux, "/"), serveCoordinatedAsset(t, mux, "/app.js")
	ids := regexp.MustCompile(`\$\("([A-Za-z]+)"\)`).FindAllStringSubmatch(script, -1)
	if len(ids) == 0 {
		t.Fatal("script references no elements")
	}
	for _, id := range ids {
		if !strings.Contains(page, `id="`+id[1]+`"`) {
			t.Errorf("script uses element %q missing from page", id[1])
		}
	}
}

// coordinatedWebAppEnv is a minimal DOM, WebSocket, and timer fake for
// driving the coordinated controller script. Timers fire only on demand.
const coordinatedWebAppEnv = `
var timers = [], timerSeq = 0, sockets = [], delays = [];
globalThis.setTimeout = (fn, ms) => { const id = ++timerSeq; timers.push({id, fn, ms: ms || 0}); return id; };
globalThis.clearTimeout = id => { timers = timers.filter(t => t.id !== id); };
// Filter by callback so a capped reconnect delay can equal a request timeout.
globalThis.shortTimers = () => timers.filter(t => !String(t.fn).includes("pending.delete")).map(t => t.ms);
globalThis.fireTimers = () => {
    const due = timers.filter(t => !String(t.fn).includes("pending.delete"));
    timers = timers.filter(t => String(t.fn).includes("pending.delete"));
    for (const t of due) t.fn();
};
class El {
    constructor() { this.children = []; this.text = ""; this.value = ""; this.disabled = false; this.dataset = {}; this.attrs = {}; this.parent = null; }
    get textContent() { return this.text + this.children.map(c => c.textContent).join(""); }
    set textContent(t) { for (const c of this.children) c.parent = null; this.children = []; this.text = String(t); }
    get firstChild() { return this.children[0] || null; }
    get firstElementChild() { return this.children[0] || null; }
    get childElementCount() { return this.children.length; }
    appendChild(c) { c.remove(); c.parent = this; this.children.push(c); return c; }
    append(...cs) { for (const c of cs) this.appendChild(c); }
    replaceChildren(...cs) { this.textContent = ""; this.append(...cs); }
    remove() { if (this.parent) { this.parent.children = this.parent.children.filter(c => c !== this); this.parent = null; } }
    setAttribute(k, v) { this.attrs[k] = v; }
}
const els = {};
globalThis.$el = id => els[id] || (els[id] = new El());
globalThis.document = {getElementById: $el, createElement: () => new El(), createDocumentFragment: () => new El()};
globalThis.location = {href: "http://127.0.0.1/", protocol: "http:"};
globalThis.URL = class { constructor(path) { this.path = path; this.protocol = "http:"; this.searchParams = {set() {}}; } };
globalThis.crypto = {randomUUID: () => "11111111-1111-4111-8111-111111111111", getRandomValues: b => b.fill(17)};
globalThis.WebSocket = class {
    constructor(url, protocols) { this.protocols = protocols; this.readyState = 0; this.bufferedAmount = 0; this.sent = []; sockets.push(this); }
    send(data) { this.sent.push(JSON.parse(data)); }
    close(code) { if (this.readyState === 3) return; this.readyState = 3; this.onclose({code: code || 1005}); }
};
Object.assign(WebSocket, {CONNECTING: 0, OPEN: 1, CLOSING: 2, CLOSED: 3});
globalThis.check = (ok, msg) => { if (!ok) throw new Error(msg + " (status: " + $el("status").textContent + ")"); };
globalThis.sock = () => sockets[sockets.length - 1];
globalThis.deliver = f => sock().onmessage({data: JSON.stringify(f)});
globalThis.answer = (match, fields) => {
    const f = sock().sent.find(x => !x.answered && match(x));
    check(f, "no unanswered request matching " + match);
    f.answered = true;
    deliver({type: "response", id: f.id, ...fields});
};
globalThis.op = type => x => x.type === type;
globalThis.cmd = type => x => x.type === "command" && x.command.type === type;
globalThis.ok = result => ({result: {type: "response", ok: true, result}});
globalThis.welcome = gen => { sock().readyState = 1; deliver({type: "welcome", role: "client", version: 1, generation: gen}); };
globalThis.drop = code => { const s = sock(); s.readyState = 3; s.onclose({code}); };
globalThis.mutations = () => sock().sent.filter(x => ["send_message", "follow_up", "interrupt", "attach_data", "ui_response", "start_instance", "stop_instance"].includes(x.type === "command" ? x.command.type : x.type));
`

// runCoordinatedWebApp loads the controller into a fake browser and returns
// a step runner. Each step drains promise jobs before returning.
func runCoordinatedWebApp(t *testing.T) func(string) goja.Value {
	t.Helper()
	vm := goja.New()
	for _, src := range []string{coordinatedWebAppEnv, coordinatedWebAppJS} {
		if _, err := vm.RunString(src); err != nil {
			t.Fatalf("load script: %v", err)
		}
	}
	return func(src string) goja.Value {
		t.Helper()
		v, err := vm.RunString(src)
		if err != nil {
			t.Fatalf("%v\nstep: %s", err, src)
		}
		return v
	}
}

// attachCoordinatedInstance connects with generation g1 and attaches
// instance A at position e1/3.
func attachCoordinatedInstance(step func(string) goja.Value) {
	for _, src := range []string{
		`$el("token").value = "tok"; $el("connect").onclick();`,
		`check(sockets.length === 1, "no socket"); welcome("g1");`,
		`answer(op("watch_instances"), {generation: "g1", instances: [{instance_id: "A", session_id: "S", cwd: "/p", link_state: "connected"}]});`,
		`$el("instances").children[0].onclick();`,
		`answer(op("attach"), {session_id: "S"});`,
		`answer(cmd("get_snapshot"), ok({position: {instance_id: "A", epoch: "e1", offset: 3}, instance: {session_id: "S"}, state: {}, history: {entries: []}}));`,
		`check(!$el("send").disabled, "attached view is not ready");`,
	} {
		step(src)
	}
}

func TestCoordinatedWebAppLaunchIntentUsesUUIDv7(t *testing.T) {
	step := runCoordinatedWebApp(t)
	step(`$el("token").value = "tok"; $el("connect").onclick();`)
	step(`sock().readyState = 1; deliver({type: "welcome", version: 1, role: "client", generation: "gen", launch_request_version: 7});`)
	step(`answer(op("watch_instances"), {generation: "gen", instances: []});`)
	step(`$el("cwd").value = "/project"; $el("start").onclick();`)
	step(`const launch = mutations().at(-1); check(launch && launch.type === "start_instance", "start not sent");
 const id = launch.request_id;
 check(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(id), "not a UUIDv7");
 check(Math.abs(Date.now() - parseInt(id.slice(0, 8) + id.slice(9, 13), 16)) < 5000, "missing issuance timestamp");`)
}

func TestCoordinatedWebAppLaunchIntentUsesCoordinatorClock(t *testing.T) {
	step := runCoordinatedWebApp(t)
	step(`var mono = 1000; globalThis.performance = {now: () => mono}; var coordMS = Date.now() - 3 * 24 * 3600 * 1000;`)
	step(`$el("token").value = "tok"; $el("connect").onclick();`)
	step(`sock().readyState = 1; deliver({type: "welcome", version: 1, role: "client", generation: "gen", launch_request_version: 7, coordinator_time_unix_ms: coordMS});`)
	step(`answer(op("watch_instances"), {generation: "gen", instances: []});`)
	step(`mono = 6250.7; $el("cwd").value = "/project"; $el("start").onclick();`)
	step(`
		var idTime = id => parseInt(id.slice(0, 8) + id.slice(9, 13), 16);
		var first = mutations().at(-1);
		check(first && first.type === "start_instance", "start not sent");
		check(idTime(first.request_id) === coordMS + 5250, "launch ID not from coordinator time: " + (idTime(first.request_id) - coordMS));
		answer(op("start_instance"), {error: {code: "clock_skew", message: "launch intent is older than the receipt window and was never recorded"}});
	`)
	step(`
		check($el("status").textContent.includes("No instance was started"), "clock refusal not reported as definite");
		check(!$el("outcomes").textContent.includes(first.request_id), "definite refusal reported as unknown");
		mono = 7000; $el("start").onclick();
	`)
	step(`
		var second = mutations().at(-1);
		check(second.request_id !== first.request_id && idTime(second.request_id) === coordMS + 6000, "retry after refusal did not use a fresh coordinator-time ID");
		answer(op("start_instance"), {error: {code: "outcome_unknown", message: "launch intent already recorded"}});
	`)
	step(`
		check($el("outcomes").textContent.includes(second.request_id), "unknown start lost its original request ID");
		check(mutations().filter(op("start_instance")).length === 2, "unknown start was resent");
	`)
	// A reconnect to an older coordinator falls back to the local clock.
	step(`drop(1001); fireTimers();`)
	step(`sock().readyState = 1; deliver({type: "welcome", version: 1, role: "client", generation: "gen", launch_request_version: 7});`)
	step(`answer(op("watch_instances"), {generation: "gen", instances: []}); $el("start").onclick();`)
	step(`check(Math.abs(Date.now() - idTime(mutations().at(-1).request_id)) < 5000, "old coordinator did not use the local clock");`)
}

func TestCoordinatedWebAppReattachesAfterCoordinatorRestart(t *testing.T) {
	step := runCoordinatedWebApp(t)
	attachCoordinatedInstance(step)
	step(`$el("message").value = "hello"; $el("message").oninput(); $el("send").onclick();`)
	step(`check(sock().sent.some(cmd("send_message")), "message not sent"); drop(1001);`)
	step(`
		check(shortTimers().length === 1 && shortTimers()[0] <= 500, "reconnect not scheduled: " + shortTimers());
		check($el("outcomes").textContent.includes("outcome unknown"), "lost send not reported");
		check(sockets.length === 1, "reconnected without delay");
		fireTimers();
	`)
	step(`check(sockets.length === 2, "did not reconnect"); welcome("g2");`)
	step(`answer(op("watch_instances"), {generation: "g2"});`)
	step(`
		check(!sock().sent.some(op("attach")), "attached an instance missing from the baseline");
		check($el("status").textContent.includes("reattached if it registers"), "missing wait status");
		deliver({type: "instance_update", generation: "g1", instance_id: "A", instance: {instance_id: "A", session_id: "S"}});
	`)
	step(`
		check(!sock().sent.some(op("attach")), "stale generation update was applied");
		deliver({type: "instance_update", generation: "g2", instance_id: "A", instance: {instance_id: "A", session_id: "S", cwd: "/p", link_state: "connected"}});
	`)
	step(`answer(op("attach"), {session_id: "S"});`)
	step(`
		const r = sock().sent.find(cmd("replay_events"));
		check(r && r.command.args.after.epoch === "e1" && r.command.args.after.offset === 3, "did not replay from retained position");
		check(!sock().sent.some(cmd("get_snapshot")), "snapshot instead of replay");
		check(mutations().length === 0, "mutation sent after reconnect");
		check($el("message").value === "hello", "draft lost");
		answer(cmd("replay_events"), ok({events: [], more: false}));
	`)
	step(`check(!$el("send").disabled, "view not ready after replay"); check(mutations().length === 0, "mutation sent");`)
}

func TestCoordinatedWebAppDoesNotReconnectAfterDisconnectOrRefusal(t *testing.T) {
	step := runCoordinatedWebApp(t)
	step(`$el("token").value = "tok"; $el("connect").onclick();`)
	step(`welcome("g1");`)
	step(`answer(op("watch_instances"), {generation: "g1"}); $el("disconnect").onclick();`)
	step(`
		check(sock().readyState === 3, "socket left open");
		check(shortTimers().length === 0, "reconnect scheduled after user disconnect");
		check($el("status").textContent === "Disconnected", "wrong status");
		$el("connect").onclick();
	`)
	step(`check(sockets.length === 2, "connect did not reopen"); welcome("g1");`)
	step(`answer(op("watch_instances"), {generation: "g1"}); drop(1008);`)
	step(`
		check(shortTimers().length === 0, "reconnect scheduled after policy close");
		check($el("status").textContent.includes("Connect again"), "refusal not reported");
	`)
}

func TestCoordinatedWebAppReconnectBackoffIsBounded(t *testing.T) {
	step := runCoordinatedWebApp(t)
	step(`Math.random = () => 0.999999; $el("token").value = "tok"; $el("connect").onclick();`)
	step(`drop(1006);`)
	for range 20 {
		if step(`shortTimers().length`).ToInteger() == 0 {
			break
		}
		step(`delays.push(shortTimers()[0]); fireTimers();`)
		step(`drop(1006);`)
	}
	step(`
		check(delays.length === 10, "attempts = " + delays.length);
		check(sockets.length === 11, "sockets = " + sockets.length);
		for (let i = 1; i < delays.length; i++) check(delays[i] >= delays[i - 1] && delays[i] <= 30000, "delays = " + delays);
		check(delays[delays.length - 1] > 25000, "delay not capped near max: " + delays);
		check($el("status").textContent.includes("stopped reconnecting"), "exhaustion not reported");
	`)
}

func TestCoordinatedWebAppFollowsWorkerLinkLossAndReturn(t *testing.T) {
	step := runCoordinatedWebApp(t)
	attachCoordinatedInstance(step)
	step(`deliver({type: "instance_update", generation: "g1", instance_id: "A", instance: {instance_id: "A", session_id: "S", cwd: "/p", link_state: "disconnected"}});`)
	step(`
		check($el("active").textContent.includes("disconnected"), "link loss not shown: " + $el("active").textContent);
		check($el("send").disabled, "send enabled while worker is disconnected");
		check(sock().sent.filter(op("attach")).length === 1, "attached a disconnected worker");
		deliver({type: "instance_update", generation: "g1", instance_id: "A", instance: {instance_id: "A", session_id: "S2", cwd: "/p"}});
	`)
	step(`check(sock().sent.filter(op("attach")).length === 2, "did not reattach returned worker"); answer(op("attach"), {session_id: "S2"});`)
	step(`
		check(sock().sent.filter(cmd("get_snapshot")).length === 2, "changed session did not load a snapshot");
		check(!sock().sent.some(cmd("replay_events")), "replayed across a session change");
		check(mutations().length === 0, "mutation sent");
	`)
}

func serveCoordinatedAsset(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := newResponseRecorder()
	mux.ServeHTTP(rec, req)
	if rec.status != http.StatusOK {
		t.Fatalf("%s status = %d", path, rec.status)
	}
	return rec.body.String()
}

type responseRecorder struct {
	header http.Header
	body   strings.Builder
	status int
}

func newResponseRecorder() *responseRecorder {
	return &responseRecorder{header: make(http.Header), status: http.StatusOK}
}

func (r *responseRecorder) Header() http.Header { return r.header }

func (r *responseRecorder) Write(data []byte) (int, error) { return r.body.Write(data) }

func (r *responseRecorder) WriteHeader(statusCode int) { r.status = statusCode }

var (
	_ http.ResponseWriter = (*responseRecorder)(nil)
	_ io.Writer           = (*responseRecorder)(nil)
)
