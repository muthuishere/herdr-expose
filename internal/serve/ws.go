package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/muthuishere/herdr-expose/internal/core"
	"github.com/muthuishere/herdr-expose/internal/upstream"
)

// Protocol tuning.
const (
	// CompressionThreshold: messages smaller than this skip deflate entirely.
	// Keystroke echoes and small repaints pay zero CPU and zero buffering delay;
	// only big TUI redraws (where the ~10x ratio lives) get compressed.
	CompressionThreshold = 1024
	// CompressionLevel is deliberately low: terminal output is highly
	// repetitive, so level 3 gets almost all the ratio at a fraction of the CPU.
	CompressionLevel = 3

	// SendQueueDepth is the per-connection outbound queue, in messages.
	SendQueueDepth = 512
	// SocketBacklogBytes is the queued-bytes level past which we shed scroll
	// and wheel input. We degrade scrolling; we never degrade typing.
	SocketBacklogBytes = 256 << 10

	writeWait  = 10 * time.Second
	pongWait   = 60 * time.Second
	pingPeriod = 25 * time.Second
)

// APIVersion is the wire protocol version reported in `welcome` and
// /v1/config. Bumped to "2" for MULTI-SESSION: the tree gained a `sessions[]`
// top level and every `target` on the wire is now `<session>/<pane_id>`. Both
// are breaking changes for a v1 client, so the number moves with them.
const APIVersion = "2"

// outMsg is one queued outbound WebSocket message.
// Exactly one of buf / text is set.
type outMsg struct {
	buf  *upstream.Buf
	text []byte
	at   time.Time
}

func (m outMsg) size() int {
	if m.buf != nil {
		return m.buf.Len()
	}
	return len(m.text)
}

// wsConn is one client connection.
type wsConn struct {
	c    *websocket.Conn
	log  Logger
	met  *core.Metrics
	id   string
	who  Identity
	sess *core.Session

	agentState map[string]string
	first      bool

	out     chan outMsg
	queued  atomic.Int64
	ctlSeq  atomic.Uint64
	closeMu sync.Once
	done    chan struct{}
}

// SendBinary queues a data-plane frame, taking ownership of one reference.
// It never blocks: a client that cannot keep up is disconnected rather than
// allowed to stall the hub.
func (w *wsConn) SendBinary(b *upstream.Buf) {
	m := outMsg{buf: b, at: time.Now()}
	select {
	case w.out <- m:
		w.queued.Add(int64(b.Len()))
	default:
		b.Release()
		w.log.Warn("ws: send queue full, closing connection", "conn", w.id)
		w.shutdown()
	}
}

// SendJSON queues a control-plane frame.
func (w *wsConn) SendJSON(typ string, data any) {
	env := map[string]any{"seq": w.ctlSeq.Add(1), "type": typ, "data": data}
	b, err := json.Marshal(env)
	if err != nil {
		w.log.Warn("ws: cannot encode control frame", "type", typ, "err", err)
		return
	}
	select {
	case w.out <- outMsg{text: b, at: time.Now()}:
		w.queued.Add(int64(len(b)))
	default:
		w.log.Warn("ws: send queue full, closing connection", "conn", w.id)
		w.shutdown()
	}
}

// backlogged reports whether the client is falling behind.
func (w *wsConn) backlogged() bool { return w.queued.Load() > SocketBacklogBytes }

func (w *wsConn) shutdown() {
	w.closeMu.Do(func() {
		close(w.done)
		_ = w.c.Close()
	})
}

// writeLoop is the only goroutine that touches the socket for writes.
func (w *wsConn) writeLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		w.shutdown()
		// Drain so no pooled buffer leaks.
		for {
			select {
			case m := <-w.out:
				if m.buf != nil {
					m.buf.Release()
				}
			default:
				return
			}
		}
	}()
	for {
		select {
		case <-w.done:
			return
		case m := <-w.out:
			w.queued.Add(-int64(m.size()))
			_ = w.c.SetWriteDeadline(time.Now().Add(writeWait))
			// Per-message deflate threshold: small messages skip compression.
			w.c.EnableWriteCompression(m.size() >= CompressionThreshold)
			var err error
			if m.buf != nil {
				err = w.c.WriteMessage(websocket.BinaryMessage, m.buf.B)
				m.buf.Release()
			} else {
				err = w.c.WriteMessage(websocket.TextMessage, m.text)
			}
			if err != nil {
				w.log.Debug("ws: write failed", "conn", w.id, "err", err)
				return
			}
			if w.met != nil {
				w.met.Write.Since(m.at)
			}
		case <-ticker.C:
			_ = w.c.SetWriteDeadline(time.Now().Add(writeWait))
			w.c.EnableWriteCompression(false)
			if err := w.c.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// looksLikeBrowser reports whether a WS upgrade came from a browser. Browsers
// always send Origin and a Sec-WebSocket-Key with a normal User-Agent; a CLI
// client typically sends neither Origin nor a browser UA.
func looksLikeBrowser(r *http.Request) bool {
	ua := r.UserAgent()
	return strings.Contains(ua, "Mozilla") || strings.Contains(ua, "Safari") ||
		strings.Contains(ua, "Chrome") || strings.Contains(ua, "Firefox")
}

// clientMsg is a control-plane message from the client.
//
// Clients send seq:0; only the server sequences frames, so any inbound `seq` is
// parsed and ignored rather than trusted.
type clientMsg struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// handleStream upgrades and runs one connection.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	ip := RemoteIP(r)
	if !s.auth.AllowHandshake(ip) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	if !s.hostAllowed(r) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}
	origin := r.Header.Get("Origin")
	if origin != "" && !s.originAllowed(r, origin) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}

	// Reject BEFORE the upgrade. An unauthenticated peer never gets a socket.
	token := BearerFrom(r)
	var who Identity
	if s.local.bypassAuth(r) {
		// Local mode: the loopback listener is the grant (SPEC F1). Origin and
		// Host pinning above have already run and are what actually defend
		// this; a browser upgrade with no Origin is refused here.
		if origin == "" && looksLikeBrowser(r) {
			http.Error(w, "origin required", http.StatusForbidden)
			return
		}
		who = Identity{Kind: "local", Name: "loopback"}
		if token != "" {
			if id, err := s.auth.Authenticate(token, ip, r.UserAgent()); err == nil {
				who = id
			}
		}
	} else {
		var err error
		who, err = s.auth.Authenticate(token, ip, r.UserAgent())
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	var respHeader http.Header
	if p := token; p != "" && r.Header.Get("Sec-WebSocket-Protocol") != "" {
		respHeader = http.Header{"Sec-WebSocket-Protocol": {"bearer." + p}}
	}
	c, err := s.upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		s.log.Debug("ws: upgrade failed", "err", err)
		return
	}
	_ = c.SetCompressionLevel(CompressionLevel)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	conn := &wsConn{
		c:          c,
		log:        s.log,
		id:         randToken(6),
		who:        who,
		met:        s.hub.Metrics(),
		agentState: map[string]string{},
		first:      true,
		out:        make(chan outMsg, SendQueueDepth),
		done:       make(chan struct{}),
	}
	conn.sess = s.hub.NewSession(ctx, conn)
	go conn.writeLoop()
	defer func() {
		conn.sess.Close()
		conn.shutdown()
	}()

	version, protocol := s.hub.Store().HerdrVersion()
	conn.SendJSON("welcome", map[string]any{
		// `protocol` is the wire contract version the client negotiates on;
		// `herdr_protocol` is upstream's, and they are unrelated numbers.
		"protocol":       "v" + APIVersion,
		"api":            APIVersion,
		"session":        conn.id,
		"server_version": s.Version,
		"herdr_version":  version,
		"herdr_protocol": protocol,
		"connection_id":  conn.id,
		"identity":       map[string]string{"kind": who.Kind, "name": who.Name},
		"binary": map[string]any{
			"server_header": "type:u8 seq:u64be tlen:u16be target:ascii payload:raw",
			"client_header": "type:u8 tlen:u16be target:ascii payload:raw",
			"types": map[string]int{
				"frame": int(core.TypeFrame), "snapshot": int(core.TypeSnapshot),
				"gap": int(core.TypeGap), "input": int(core.TypeInput),
			},
		},
		"limits": map[string]any{
			"min_cols": core.MinCols, "min_rows": core.MinRows,
			"max_write_bytes": core.MaxWriteBytes,
		},
		// Targets are session-qualified on this wire. Spelled out here so a
		// hand-written client does not have to infer it from the tree.
		"targets": map[string]any{
			"format":          "<session>/<pane_id>",
			"separator":       core.TargetSep,
			"multi_session":   true,
			"default_session": s.hub.Store().DefaultSession(),
		},
	})
	s.sendTree(ctx, conn)

	// Push tree updates for as long as the connection lives.
	treeCh, unwatch := s.hub.Store().Watch()
	defer unwatch()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-conn.done:
				return
			case <-treeCh:
				s.sendTree(ctx, conn)
			}
		}
	}()

	c.SetReadLimit(1 << 20)
	_ = c.SetReadDeadline(time.Now().Add(pongWait))
	c.SetPongHandler(func(string) error {
		return c.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		typ, data, err := c.ReadMessage()
		if err != nil {
			return
		}
		_ = c.SetReadDeadline(time.Now().Add(pongWait))
		switch typ {
		case websocket.BinaryMessage:
			s.handleBinary(conn, data)
		case websocket.TextMessage:
			s.handleControl(ctx, conn, data)
		}
	}
}

// handleBinary is the keystroke hot path: parse a 3-byte header, hand the raw
// bytes straight to the upstream stream. No JSON, no base64, no copy.
func (s *Server) handleBinary(conn *wsConn, data []byte) {
	t0 := time.Now()
	t, target, payload, err := core.DecodeClientFrame(data)
	if err != nil {
		return
	}
	if t != core.TypeInput {
		return
	}
	err = conn.sess.Input(target, payload)
	s.hub.Metrics().Input.Since(t0)
	if err != nil {
		conn.SendJSON("error", map[string]any{
			"target": target, "code": "input_failed", "detail": err.Error()})
	}
}

// sendTree pushes the nested tree, then the per-pane agent frames. The order
// matters: `agent` frames reference targets the client has just learned about,
// and an already-blocked pane must arrive with its detection text on the very
// first push or the Q&A view has nothing to render.
func (s *Server) sendTree(ctx context.Context, conn *wsConn) {
	t := s.hub.Store().Tree()
	view := buildTree(t, conn.sess)
	conn.SendJSON("tree", view)
	first := conn.first
	conn.first = false
	go s.emitAgents(ctx, conn, view, first)
}

func (s *Server) handleControl(ctx context.Context, conn *wsConn, data []byte) {
	var m clientMsg
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	switch m.Type {
	case "hello":
		// data: {protocol:"v1", client:{...}, session?:"..."} — informational.
		s.sendTree(ctx, conn)

	case "ping":
		// Echo the client's `t` VERBATIM: RTT is then a pure client-side
		// subtraction and needs no clock synchronisation.
		var d struct {
			T json.RawMessage `json:"t"`
		}
		_ = json.Unmarshal(m.Data, &d)
		conn.SendJSON("pong", map[string]any{"t": d.T, "server_t": time.Now().UnixMilli()})

	case "resize":
		var d struct {
			Target string `json:"target"`
			Cols   int    `json:"cols"`
			Rows   int    `json:"rows"`
		}
		if err := json.Unmarshal(m.Data, &d); err != nil || d.Target == "" {
			return
		}
		conn.sess.SetGeometry(d.Target, d.Cols, d.Rows)

	case "viewport", "subscribe":
		var d struct {
			Targets map[string]string `json:"targets"`
		}
		if err := json.Unmarshal(m.Data, &d); err != nil {
			return
		}
		conn.sess.SetViewport(d.Targets)

	case "unsubscribe":
		var d struct {
			Targets []string `json:"targets"`
		}
		if err := json.Unmarshal(m.Data, &d); err != nil {
			return
		}
		cur := map[string]string{}
		for _, t := range d.Targets {
			cur[t] = "none"
		}
		conn.sess.SetViewport(cur)

	case "scroll":
		var d struct {
			Target string `json:"target"`
			Delta  int    `json:"delta"`
		}
		if err := json.Unmarshal(m.Data, &d); err != nil {
			return
		}
		// Backpressure policy: shed scrolling, never typing.
		if conn.backlogged() {
			return
		}
		if err := conn.sess.Scroll(d.Target, d.Delta); err != nil {
			conn.SendJSON("error", map[string]any{
				"target": d.Target, "code": "scroll_failed", "detail": err.Error()})
		}

	case "seen":
		var d struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(m.Data, &d); err != nil {
			return
		}
		conn.sess.Seen.MarkSeen(d.Target, s.hub.Store().Tree().DoneSeq[d.Target])
		s.sendTree(ctx, conn)

	case "command":
		var d struct {
			ID      string          `json:"id"`
			Session string          `json:"session"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(m.Data, &d); err != nil {
			return
		}
		go s.runCommand(ctx, conn, d.ID, d.Session, d.Method, d.Params)
	}
}

// runCommand passes a method straight through to ONE session's Herdr socket.
// Method names are deliberately not enumerated: whatever Herdr exposes, clients
// can call.
//
// Session selection, in order: the frame's explicit `session`; the session
// prefix on a target-ish param; then the session of the target this connection
// is currently rendering live; then the store's default. Herdr itself knows
// nothing about our namespacing, so any `<session>/` prefix is stripped off the
// params before they go upstream.
func (s *Server) runCommand(ctx context.Context, conn *wsConn, id, session, method string, params json.RawMessage) {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	p := map[string]any{}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			// Not an object (Herdr takes objects, but never assume): pass it
			// through untouched against the resolved session.
			var raw any
			_ = json.Unmarshal(params, &raw)
			s.dispatch(cctx, conn, id, s.resolveCommandSession(conn, session, nil), method, raw)
			return
		}
	}
	sessionName := s.resolveCommandSession(conn, session, p)
	for _, k := range targetParamKeys {
		if v, ok := p[k].(string); ok {
			p[k] = core.StripSession(sessionName, v)
		}
	}
	s.dispatch(cctx, conn, id, sessionName, method, p)
}

// targetParamKeys are the params that carry a Herdr id and therefore may arrive
// session-qualified from a client that only ever sees qualified ids.
var targetParamKeys = []string{"target", "pane_id", "tab_id", "workspace_id",
	"from", "to", "source_pane_id", "target_pane_id"}

func (s *Server) resolveCommandSession(conn *wsConn, explicit string, params map[string]any) string {
	if explicit != "" {
		return explicit
	}
	known := map[string]bool{}
	for _, n := range s.hub.Store().Sessions() {
		known[n] = true
	}
	for _, k := range targetParamKeys {
		if v, ok := params[k].(string); ok {
			if name := core.SessionOf(v); name != "" && known[name] {
				return name
			}
		}
	}
	// The session of whatever this connection is looking at.
	return conn.sess.FocusedSession()
}

func (s *Server) dispatch(ctx context.Context, conn *wsConn, id, session, method string, params any) {
	c := s.hub.Store().Client(session)
	if c == nil {
		conn.SendJSON("result", map[string]any{
			"id": id, "session": session, "ok": false,
			"error": "unknown or disconnected herdr session: " + session})
		return
	}
	res, err := c.Call(ctx, method, params)
	if err != nil {
		conn.SendJSON("result", map[string]any{
			"id": id, "session": session, "ok": false, "error": err.Error()})
		return
	}
	conn.SendJSON("result", map[string]any{
		"id": id, "session": session, "ok": true, "result": json.RawMessage(res)})
}
