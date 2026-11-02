// Serve mode: the same app without the window.
//
//	superleo serve -port 43117
//
// runs the identical App — same settings, same agent, same schedules out of the
// same database — behind a plain HTTP server instead of a WKWebView, so the
// frontend can be a browser tab on this machine. Three surfaces:
//
//   - POST /api/rpc/<Method>   — every exported App method, args as a JSON
//     array, result as JSON. This is the same contract Wails builds from the
//     same reflection, which is what keeps the two frontends one frontend:
//     the browser shim (frontend/src/lib/webshim.ts) fakes window.go over it.
//   - GET  /api/events         — one SSE stream carrying every event the app
//     would have emitted to the window, as {"name":..., "payload":...}.
//   - everything else          — the embedded frontend/dist.
//
// The listener binds 127.0.0.1 unless -bind says otherwise. Every route except
// login is behind the credential gate (auth.go), so widening the bind is an
// explicit decision — a container has to, since its loopback is unreachable
// from the Service — rather than something a default does by accident.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/liliang-cn/superleo/internal/backend"
)

// rpcDenied lists bound methods that only make sense with a native window on
// top. They stay bound in the desktop app; over HTTP they answer 404 with the
// reason instead of crashing into a nil Wails context.
var rpcDenied = map[string]string{
	"PickFiles":           "needs a native file dialog; not available in the browser",
	"ExportWorkspaceFile": "needs a native save dialog; use ReadWorkspaceFileDataURL and download instead",
	// Whoever is calling this over HTTP is already in a browser, so the button
	// has nothing to offer them — and opening listeners is not something a
	// remote caller should be able to ask for on the strength of one session.
	"OpenInBrowser": "you are already in a browser",
}

// eventHub fans one emitted event out to every connected SSE client. A slow
// client loses events rather than blocking the emitter: emit is called from
// the middle of chat turns, and a stalled browser tab must not stall the agent.
type eventHub struct {
	mu   sync.Mutex
	subs map[chan []byte]eventFilter
}

// eventFilter is which events one subscriber asked for, as name prefixes; an
// empty filter is every event.
//
// Every subscriber used to get everything: a watch that only shows its own
// answer received the meter twice a second, the pulse, the hive's chatter and
// every other conversation's tokens — 49 events in an idle 20 seconds,
// measured, all decoded on the wrist and thrown away. Worse, a slow phone's
// buffer filled with those and the one event it needed, the end of its own
// answer, was the one dropped.
type eventFilter []string

func (f eventFilter) wants(name string) bool {
	if len(f) == 0 {
		return true
	}
	for _, p := range f {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// parseEventFilter reads ?only=chat:,tool:approval into a filter.
func parseEventFilter(r *http.Request) eventFilter {
	var f eventFilter
	for _, v := range r.URL.Query()["only"] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				f = append(f, p)
			}
		}
	}
	return f
}

// Heartbeat bounds for /api/events. A browser or a phone in front keeps the
// default; a phone holding the stream open in the background (Android's
// 保持连接) asks for fewer wake-ups with ?heartbeat=<seconds> or its alias
// ?keepalive=<seconds>, which stays under the idle timeouts of mobile NATs and
// proxies at the top of the range.
const (
	defaultHeartbeat = 25 * time.Second
	minHeartbeat     = 10 * time.Second
	maxHeartbeat     = 300 * time.Second
)

// heartbeatEvery reads ?heartbeat= (or ?keepalive=) <seconds>, clamped to the bounds; the
// default when it is absent or not a number.
func heartbeatEvery(r *http.Request) time.Duration {
	q := r.URL.Query()
	v := strings.TrimSpace(q.Get("heartbeat"))
	if v == "" {
		v = strings.TrimSpace(q.Get("keepalive"))
	}
	if v == "" {
		return defaultHeartbeat
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultHeartbeat
	}
	return min(max(time.Duration(n)*time.Second, minHeartbeat), maxHeartbeat)
}

func newEventHub() *eventHub {
	return &eventHub{subs: make(map[chan []byte]eventFilter)}
}

func (h *eventHub) broadcast(name string, payload map[string]any) {
	b, err := json.Marshal(map[string]any{"name": name, "payload": payload})
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, f := range h.subs {
		if !f.wants(name) {
			continue
		}
		select {
		case ch <- b:
		default: // drop for this subscriber rather than block the run
		}
	}
}

func (h *eventHub) subscribe(f eventFilter) (<-chan []byte, func()) {
	ch := make(chan []byte, 256)
	h.mu.Lock()
	h.subs[ch] = f
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *eventHub) serveSSE(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	// Subscribed before anything is flushed: whoever opened the stream treats
	// the first byte as "listening" and sends its command — a queen asking a
	// worker does exactly that — and an answer quicker than the gap between
	// the flush and the subscription was lost, leaving the asker waiting for
	// an end that had already gone by.
	ch, off := h.subscribe(parseEventFilter(r))
	defer off()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// An opening comment makes EventSource fire `open` immediately, so the
	// frontend knows the stream is live before the first real event.
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	keepalive := time.NewTicker(heartbeatEvery(r))
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-keepalive.C:
			// Comment frames keep idle proxies and browsers from closing us.
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

// errorType is the `error` interface, for telling a method's error return from
// its result by signature.
var errorType = reflect.TypeOf((*error)(nil)).Elem()

// rpcCall dispatches one POST /api/rpc/<Method> onto the App by reflection —
// the same shape Wails derives from the same struct, so the browser gets
// exactly the binding surface the window gets, no hand-maintained route table
// to drift out of sync.
func rpcCall(app *App, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/api/rpc/")
	if reason, denied := rpcDenied[name]; denied {
		http.Error(w, name+": "+reason, http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var raw []json.RawMessage
	if len(strings.TrimSpace(string(body))) > 0 {
		if err := json.Unmarshal(body, &raw); err != nil {
			http.Error(w, "arguments must be a JSON array: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	acceptHTTPFlow(w, r, name, raw)
	result, err := callMethod(app, name, raw)
	if err != nil {
		code := http.StatusInternalServerError
		var ce *callError
		if errors.As(err, &ce) {
			code = ce.status
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// callError is a call that never reached the method: no such method, or
// arguments that do not fit it. Status is what HTTP says about it.
type callError struct {
	status int
	msg    string
}

func (e *callError) Error() string { return e.msg }

// callMethod runs one App method by name with its arguments as JSON — the one
// dispatcher behind /api/rpc and behind the calls core makes on an agent over
// the agent link, so the two can never disagree about a method.
func callMethod(app *App, name string, raw []json.RawMessage) (any, error) {
	m := reflect.ValueOf(app).MethodByName(name)
	if !m.IsValid() {
		return nil, &callError{http.StatusNotFound, "no such method: " + name}
	}
	mt := m.Type()
	if mt.NumIn() != len(raw) {
		return nil, &callError{http.StatusBadRequest, fmt.Sprintf("%s takes %d argument(s), got %d", name, mt.NumIn(), len(raw))}
	}
	args := make([]reflect.Value, len(raw))
	for i := range raw {
		v := reflect.New(mt.In(i))
		if err := json.Unmarshal(raw[i], v.Interface()); err != nil {
			return nil, &callError{http.StatusBadRequest, fmt.Sprintf("argument %d: %v", i, err)}
		}
		args[i] = v.Elem()
	}

	out := m.Call(args)

	// Wails semantics: a trailing error return rejects the promise; everything
	// before it resolves it.
	//
	// Which return is the error is decided by the method's signature and not by
	// the value in it. Asking the value — `o.Interface().(error)` — gets it
	// wrong in exactly the case that matters: a nil error reflects to an `any`
	// with no dynamic type at all, the assertion says "not an error", and the
	// nil then overwrites the result. Every method returning (T, error) was
	// answering `null` on success.
	var result any
	for i, o := range out {
		if mt.Out(i) == errorType {
			if err, _ := o.Interface().(error); err != nil {
				return nil, err
			}
			continue
		}
		result = o.Interface()
	}
	return result, nil
}

// spaHandler serves the embedded frontend build, falling back to index.html
// for any path that is not a real file — the frontend is a single page.
func spaHandler() (http.Handler, error) {
	dist, err := fs.Sub(Assets, "frontend/dist")
	if err != nil {
		return nil, err
	}
	files := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p != "" {
			if f, err := dist.Open(p); err == nil {
				f.Close()
				files.ServeHTTP(w, r)
				return
			}
		}
		r.URL.Path = "/"
		files.ServeHTTP(w, r)
	}), nil
}

func newAPIMux(app *App, hub *eventHub, creds *credentials, handoff *handoffStore) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	// Sign-in lives on the mux rather than in the middleware so it is a route
	// like any other; requireAuth lets exactly these four through. See auth.go.
	authRoutes(mux, creds)
	// A core's claim says where its agent link is, on the address the
	// claim came in by, when no address was configured (app_agentdoor.go).
	pairRoutesLinking(mux, creds, func() int { _, l, _ := app.agentDoor.get(); return l })
	registerPushRoute(mux, creds)
	agentHookRoute(mux, app)
	// A webhook channel's way in: its own token, its own door (channel_inbound.go).
	channelInboundRoute(mux, app)
	// The desktop app's "Open in Browser" hands the tab it opens a one-shot
	// token here, so nobody is asked for a password they never chose. nil in
	// serve mode: nothing there mints tokens, so every arrival is turned away.
	handoffRoute(mux, creds, handoff)
	mux.HandleFunc("/api/rpc/", func(w http.ResponseWriter, r *http.Request) { rpcCall(app, w, r) })
	mux.HandleFunc("/api/events", hub.serveSSE)
	// The sideloaded Android app's updates: latest.json and the APK it names.
	androidRoutes(mux, androidReleaseDir)
	// Speech. One sentence in, mp3 out. It is a route rather than an RPC
	// method because the answer is bytes: the RPC surface marshals results as
	// JSON, and base64 inside JSON would cost a third of the size and all of
	// the streaming.
	mux.HandleFunc("/api/tts", app.handleSpeak)
	// Its counterpart: a recording in, text out (the phone's server recognition).
	mux.HandleFunc("/api/stt", app.handleTranscribe)
	mux.HandleFunc(browserStreamPath, app.handleBrowserStream)
	mux.HandleFunc("/api/hive/join", app.handleHiveJoin)
	mux.HandleFunc("/api/hive/hold", app.handleHiveHold)
	mux.HandleFunc(hiveDevicesPath, app.handleHiveDevices)
	mux.HandleFunc("/api/hive/roster", app.handleHiveRoster)
	mux.HandleFunc("/api/hive/leave", app.handleHiveLeave)
	mux.HandleFunc("/api/hive/pulse", app.handleHivePulse)
	mux.HandleFunc("/api/hive/task", app.handleHiveTask)
	mux.HandleFunc("/api/hive/message", app.handleHiveMessage)
	mux.HandleFunc("/api/hive/abilities", app.handleHiveAbilities)
	// Uploading a file to import. It writes to this machine, so it is gated
	// like everything else under /api — see gatedPath.
	mux.HandleFunc(uploadPath, app.handleUpload)
	// The live graph view, proxied. It binds loopback on this host and has no
	// authentication of its own — see backend/graphproxy.go — so it is mounted
	// here, behind requireAuth, and nowhere else.
	mux.Handle(backend.GraphProxyPrefix, app.graphProxy())
	mux.Handle(backend.GraphProxyPrefix+"/", app.graphProxy())
	// The avatar bridge, same-origin. It binds loopback on this host, so over
	// the network the pet was asking the *viewer's* machine for port 47615 and
	// getting nothing — its sprites never loaded and its events never
	// connected. Behind requireAuth like everything else here.
	mux.Handle(backend.AvatarProxyPrefix+"/", app.avatarProxy())
	// SuperLeo as an MCP server — see mcp.go for why it lives in this process
	// and why it adds no gate of its own.
	mux.Handle(mcpPath, newMCPHandler(app, mcpServerVersion))
	spa, err := spaHandler()
	if err != nil {
		return nil, err
	}
	mux.Handle("/", NoCache(spa))
	return mux, nil
}

func envInt(name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	return n
}

// serveMain is the `superleo serve` entry point.
func ServeMain(argv []string) {
	fl := flag.NewFlagSet("serve", flag.ExitOnError)
	port := fl.Int("port", 43117, "listen on <bind>:<port>")
	bind := fl.String("bind", "127.0.0.1", "address to listen on; anything but loopback exposes the API to the network, so only do it behind the login gate (a container needs 0.0.0.0)")
	agentPort := fl.Int("agent-port", envInt("SUPERLEO_AGENT_PORT"), "listen for agents on <bind>:<agent-port> (gRPC); 0 is off (env SUPERLEO_AGENT_PORT)")
	lf := addAgentLinkFlags(fl)
	_ = fl.Parse(argv)

	backend.SetupLogging("serve")
	startPprof()

	hub := newEventHub()
	app := NewApp()
	app.emitFn = hub.broadcast // before startup: no window may ever see an event
	// Loopback whatever the bind: it is the address this instance's own local
	// workers join (app_localhive.go), and they are on this machine.
	app.serveURL = fmt.Sprintf("http://127.0.0.1:%d", *port)
	app.startupHeadless()
	app.startMemberAtLaunch()
	defer app.Shutdown(context.Background())

	// Serve mode is the desktop app with the window replaced by a socket, and
	// the socket has no idea who is on the other end. See auth.go.
	creds, err := loadOrCreateCredentials()
	if err != nil {
		slog.Error("auth", "error", err)
		os.Exit(1)
	}
	mux, err := newAPIMux(app, hub, creds, nil)
	if err != nil {
		slog.Error("assets", "error", err)
		os.Exit(1)
	}
	addr := net.JoinHostPort(*bind, fmt.Sprint(*port))
	srv := &http.Server{Addr: addr, Handler: requireAuth(creds, mux)}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// A local worker goes when its queen does: she holds the other end of
	// its stdin, and the operating system closes it when she exits.
	if os.Getenv(backend.ExitWithStdinEnv) == "1" {
		backend.WatchStdin(stop)
	}

	// Core: agents dial in here.
	if *agentPort > 0 && !app.isHiveMember() {
		app.agentDoor.set(*port, *agentPort, nil)
		agentAddr := net.JoinHostPort(*bind, fmt.Sprint(*agentPort))
		go func() {
			if err := ServeAgentLink(ctx, app, agentAddr, credentialsCheck(creds)); err != nil {
				slog.ErrorContext(ctx, "agent link", "error", err)
			}
		}()
	}
	// Claude Code's hooks on this machine (cchook.go) report here.
	go app.serveCCHookSocket(ctx)
	go autoUpgradeCCHooks()
	// The coding CLIs' activity hooks: not on a first start that has not yet
	// been told it may use the agents here (FinishOnboarding installs them
	// then), so a fresh SuperLeo never rewrites another one's plugins.
	if app.mayInstallCLIHooks() {
		go autoInstallCLIHooks()
	}
	// Agent: this instance also dials a core (a node that keeps its own web
	// surface, like the Mac's).
	if o, ok := lf.options(); ok {
		go app.RunAgentLink(ctx, o)
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	slog.InfoContext(ctx, "serving", "url", "http://"+addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.ErrorContext(ctx, "serve", "error", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "superleo-serve: stopped")
}
