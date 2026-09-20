// Command wsprobe is the acceptance harness for the herdr-expose wire protocol.
//
// It is a real client: it authenticates, opens /v1/stream, reads the control
// plane, picks panes out of the tree, declares them TRANSCRIPT (the product's
// default view) and LIVE, and then measures what actually arrives — transcript
// and geometry frames on the control plane, snapshots and frames on the binary
// data plane.
//
// It sends NO geometry unless asked (-resize). AMENDMENTS 14 / K4 withdrew B2's
// "resize before the first frame" requirement, so the default run exercises the
// pane-matched attach and asserts that herdr reported the PANE'S own size back.
//
// A run that measured nothing FAILS. Every declared target must have produced
// data on the plane it was declared for; "RESULT ok" on an empty measurement is
// how a broken build passes.
//
// It is READ-ONLY against Herdr by default. It never sends input to a pane and
// never asks for a control stream; observe streams are harmless.
//
//	wsprobe -addr 127.0.0.1:21118 -token <server-token> [-target w2:p1] [-secs 8]
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// treeFrame mirrors the `tree` control frame (api 2):
// sessions -> workspaces -> tabs -> panes, with every id session-qualified.
type treeFrame struct {
	Rev            uint64         `json:"rev"`
	Connected      bool           `json:"connected"`
	HerdrVersion   string         `json:"herdr_version"`
	FocusedSession string         `json:"focused_session"`
	FocusedPane    string         `json:"focused_pane"`
	Sessions       []sessionFrame `json:"sessions"`
}

type sessionFrame struct {
	ID          string `json:"id"`
	Running     bool   `json:"running"`
	Connected   bool   `json:"connected"`
	Focused     bool   `json:"focused"`
	Origin      bool   `json:"origin"`
	Error       string `json:"error"`
	FocusedPane string `json:"focused_pane"`
	Workspaces  []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
		Tabs  []struct {
			ID    string      `json:"id"`
			Panes []paneFrame `json:"panes"`
		} `json:"tabs"`
	} `json:"workspaces"`
}

type paneFrame struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Agent *struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		State string `json:"state"`
	} `json:"agent"`
}

func (sf sessionFrame) panes() []paneFrame {
	var out []paneFrame
	for _, ws := range sf.Workspaces {
		for _, tab := range ws.Tabs {
			out = append(out, tab.Panes...)
		}
	}
	return out
}

func (t treeFrame) allPanes() []paneFrame {
	var out []paneFrame
	for _, sf := range t.Sessions {
		out = append(out, sf.panes()...)
	}
	return out
}

type envelope struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:21118", "server address")
	token := flag.String("token", os.Getenv("HERDR_EXPOSE_TOKEN"), "bearer token")
	target := flag.String("target", "", "comma-separated session-qualified pane ids, e.g. "+
		"herdr-plugins/w2:p1,herdr-plugins/w2:p2 (default: one pane per connected session)")
	secs := flag.Int("secs", 25, "how long to stream")
	silentFail := flag.Int("silent-secs", 12, "FAIL a LIVE target that delivers nothing for this "+
		"many seconds AFTER the pane is observed (via pane.read) to have produced output")
	verify := flag.Bool("verify", true, "poll pane.read per LIVE target so an idle pane can be told "+
		"apart from a DEAD upstream stream; this is what makes silence a failure and not a shrug")
	// GEOMETRY IS OPT-IN, and that reversal is the point of this flag.
	//
	// wsprobe used to send `resize` before every `viewport`, because SPEC B2
	// said "a terminal without a geometry message does not work". AMENDMENTS 14
	// / K4 WITHDREW B2: a LIVE attach now passes no --cols/--rows and herdr
	// reports the pane's own size back as a `geometry` frame. Sending resize
	// unconditionally marked every target this harness touched as `explicit`,
	// so the flagship pane-matched path — the one the whole product now runs on
	// — was the one path with no protocol-level coverage at all.
	resize := flag.Bool("resize", false, "send an explicit `resize` before the viewport. "+
		"This MUTATES the pane for everyone attached to it (K4), so it is off by default; "+
		"leaving it off is what exercises the pane-matched, no-geometry attach")
	view := flag.String("view", "mixed", "live | transcript | mixed. `mixed` declares alternating "+
		"targets transcript and live, so the DEFAULT view (transcript) and the terminal plane "+
		"are both covered in one run")
	resizeAfter := flag.Int("resize-after-ms", 0, "re-send a slightly different `resize` this many "+
		"milliseconds AFTER the viewport (what a real xterm client does when it mounts and fits). "+
		"Implies -resize semantics for the targets it touches")
	cols := flag.Int("cols", 100, "terminal columns")
	echo := flag.Int("echo", 0, "measure keystroke->echo latency with N probe keystrokes "+
		"(DEFAULT 0 = read-only; this TAKES CONTROL of the pane and types into it)")
	echoKey := flag.String("echo-key", "\x1b", "bytes to send as the probe keystroke; "+
		"ESC is the default because it is inert in most TUIs")
	rows := flag.Int("rows", 30, "terminal rows")
	cmdMethod := flag.String("cmd", "", "also run one passthrough `command` (READ-ONLY methods only)")
	cmdParams := flag.String("cmd-params", "{}", "JSON params for -cmd")
	cmdSession := flag.String("cmd-session", "", "session for -cmd (default: inferred from the params, "+
		"else the session of the pane being rendered live)")
	flag.Parse()

	// An empty token is valid: in local mode (loopback listener, pinned Host and
	// Origin) no device token is required — see SPEC F1.

	// 1. /healthz, unauthenticated.
	resp, err := http.Get("http://" + *addr + "/healthz")
	if err != nil {
		fail("healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		fail("healthz: HTTP %d", resp.StatusCode)
	}
	fmt.Printf("PASS healthz 200 %s\n", body)

	// 2. WebSocket handshake with a bearer token.
	d := websocket.Dialer{EnableCompression: true, HandshakeTimeout: 10 * time.Second}
	hdr := http.Header{}
	if *token != "" {
		hdr.Set("Authorization", "Bearer "+*token)
	}
	c, hresp, err := d.Dial("ws://"+*addr+"/v1/stream", hdr)
	if err != nil {
		if hresp != nil {
			fail("dial: %v (HTTP %d)", err, hresp.StatusCode)
		}
		fail("dial: %v", err)
	}
	defer c.Close()
	fmt.Println("PASS websocket upgraded")

	// 3. Host pinning must reject a rebound DNS name before the upgrade.
	rebind := http.Header{"Host": {"evil.example.com"}}
	if _, r2, err := d.Dial("ws://"+*addr+"/v1/stream", rebind); err == nil {
		fmt.Println("WARN a forged Host was accepted (Go may have overridden it)")
	} else if r2 != nil && r2.StatusCode == http.StatusForbidden {
		fmt.Println("PASS forged Host rejected with 403 before upgrade")
	} else {
		fmt.Printf("INFO forged Host rejected (%v)\n", err)
	}
	// A browser-shaped upgrade from a hostile page must be refused on Origin.
	evil := http.Header{
		"Origin":     {"http://evil.example.com"},
		"User-Agent": {"Mozilla/5.0"},
	}
	if _, r3, err := d.Dial("ws://"+*addr+"/v1/stream", evil); err == nil {
		fail("FAIL a hostile Origin was accepted")
	} else if r3 != nil && r3.StatusCode == http.StatusForbidden {
		fmt.Println("PASS hostile Origin rejected with 403 before upgrade")
	} else {
		fmt.Printf("WARN hostile Origin rejected, but not with 403 (%v)\n", err)
	}

	var (
		gotWelcome bool
		gotTree    bool
		targets    []string
		// liveTargets/trTargets partition `targets` by the view each one was
		// declared in. A LIVE target is judged on the binary data plane; a
		// TRANSCRIPT target is judged on the `transcript` control frame, which
		// is the product's DEFAULT view and had no coverage here at all.
		liveTargets  []string
		trTargets    []string
		isTranscript = map[string]bool{}
		trCount      = map[string]int{}
		trBytes      int
		geomSource   = map[string]string{}
		geomSize     = map[string]string{}
		resizeSent   int
		perTarget    = map[string]int{}
		// Liveness bookkeeping, touched only by the read loop.
		lastData      = map[string]time.Time{}
		dataSincePoll = map[string]bool{}
		unexplained   = map[string]time.Time{}
		paneHash      = map[string]string{}
		changedEver   = map[string]bool{}
		pane          = *target
		frames        int
		snapshots     int
		gaps          int
		dataBytes     int
		firstFrame    time.Time
		pingSent      time.Time
		rtt           time.Duration

		echoMu      sync.Mutex
		echoPending time.Time
		echoSamples []time.Duration
		echoLeft    = *echo
	)

	deadline := time.Now().Add(time.Duration(*secs) * time.Second)
	_ = c.SetReadDeadline(deadline.Add(2 * time.Second))

	// sendInput writes a binary client frame: [type=16][tlen u16][target][raw].
	sendInput := func(target string, raw []byte) {
		buf := make([]byte, 0, 3+len(target)+len(raw))
		buf = append(buf, 16, byte(len(target)>>8), byte(len(target)))
		buf = append(buf, target...)
		buf = append(buf, raw...)
		echoMu.Lock()
		echoPending = time.Now()
		echoMu.Unlock()
		if err := c.WriteMessage(websocket.BinaryMessage, buf); err != nil {
			fail("write input: %v", err)
		}
	}

	send := func(typ string, data any) {
		b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
		if err := c.WriteMessage(websocket.TextMessage, b); err != nil {
			fail("write %s: %v", typ, err)
		}
	}

	for time.Now().Before(deadline) {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			break
		}
		switch mt {
		case websocket.TextMessage:
			var e envelope
			if json.Unmarshal(msg, &e) != nil {
				continue
			}
			switch e.Type {
			case "welcome":
				gotWelcome = true
				fmt.Printf("PASS welcome %s\n", compact(e.Data))
			case "tree":
				if !gotTree {
					gotTree = true
					var t treeFrame
					_ = json.Unmarshal(e.Data, &t)
					all := t.allPanes()
					fmt.Printf("PASS tree upstream_connected=%v sessions=%d panes=%d focused_session=%s\n",
						t.Connected, len(t.Sessions), len(all), t.FocusedSession)
					for _, sf := range t.Sessions {
						flags := ""
						if sf.Origin {
							flags += " origin"
						}
						if sf.Focused {
							flags += " focused"
						}
						fmt.Printf("  session %-16s running=%-5v connected=%-5v workspaces=%d%s %s\n",
							sf.ID, sf.Running, sf.Connected, len(sf.Workspaces), flags, sf.Error)
						for _, p := range sf.panes() {
							state, kind := "-", "-"
							if p.Agent != nil {
								state, kind = p.Agent.State, p.Agent.Kind
							}
							fmt.Printf("     pane %-28s agent=%-8s state=%-8s %s\n",
								p.ID, kind, state, p.Title)
						}
					}
					// Namespacing check: every pane id must be <session>/<id>.
					for _, sf := range t.Sessions {
						for _, p := range sf.panes() {
							if !strings.HasPrefix(p.ID, sf.ID+"/") {
								fail("pane %q is not namespaced by session %q", p.ID, sf.ID)
							}
						}
					}
					fmt.Println("PASS every target is namespaced <session>/<pane_id>")

					if *target != "" {
						for _, tg := range strings.Split(*target, ",") {
							if tg = strings.TrimSpace(tg); tg != "" {
								targets = append(targets, tg)
							}
						}
					} else {
						// One pane per CONNECTED session: the point of api 2 is
						// that many sessions stream at once.
						for _, sf := range t.Sessions {
							if !sf.Connected {
								continue
							}
							pick := sf.FocusedPane
							if pick == "" {
								if ps := sf.panes(); len(ps) > 0 {
									pick = ps[0].ID
								}
							}
							if pick != "" {
								targets = append(targets, pick)
							}
						}
					}
					if len(targets) == 0 {
						fail("no pane to stream")
					}
					// Partition the targets across the two views. `mixed`
					// alternates, so one run covers both the transcript (the
					// DEFAULT) and the terminal plane.
					for i, tg := range targets {
						tr := *view == "transcript" || (*view == "mixed" && i%2 == 0)
						isTranscript[tg] = tr
						if tr {
							trTargets = append(trTargets, tg)
						} else {
							liveTargets = append(liveTargets, tg)
						}
					}
					if len(liveTargets) > 0 {
						pane = liveTargets[0]
					} else {
						pane = targets[0]
					}
					vp := map[string]string{}
					for _, tg := range targets {
						if isTranscript[tg] {
							fmt.Printf("     transcript %s (no geometry, never attaches)\n", tg)
							vp[tg] = "transcript"
							continue
						}
						if *resize || *resizeAfter > 0 {
							// EXPLICIT, and it really does resize somebody's
							// pane — which is why it is not the default.
							fmt.Printf("     streaming %s at an IMPOSED %dx%d\n", tg, *cols, *rows)
							send("resize", map[string]any{"target": tg, "cols": *cols, "rows": *rows})
							resizeSent++
						} else {
							fmt.Printf("     streaming %s at the PANE'S OWN size (no geometry sent)\n", tg)
						}
						vp[tg] = "live"
					}
					send("viewport", map[string]any{"targets": vp})
					started := time.Now()
					for _, tg := range targets {
						lastData[tg] = started
					}
					if *resizeAfter > 0 {
						tgs := append([]string(nil), liveTargets...)
						go func() {
							time.Sleep(time.Duration(*resizeAfter) * time.Millisecond)
							for _, tg := range tgs {
								fmt.Printf("     re-resize %s to %dx%d AFTER viewport\n", tg, *cols-1, *rows)
								send("resize", map[string]any{"target": tg, "cols": *cols - 1, "rows": *rows})
							}
						}()
					}
					if *verify {
						// Poll what the PANE actually holds, so "no frames" can
						// be judged. An idle pane is legitimately silent; a pane
						// whose content is changing while our LIVE stream says
						// nothing is a dead product, and must FAIL.
						tgs := append([]string(nil), liveTargets...)
						go func() {
							t := time.NewTicker(2 * time.Second)
							defer t.Stop()
							for range t.C {
								if time.Now().After(deadline) {
									return
								}
								for _, tg := range tgs {
									send("command", map[string]any{"id": "verify:" + tg,
										"method": "pane.read",
										"params": map[string]any{"pane_id": tg,
											"source": "visible", "format": "text", "strip_ansi": true}})
								}
							}
						}()
					}
					pingSent = time.Now()
					send("ping", map[string]any{"t": pingSent.UnixMilli()})
					if *cmdMethod != "" {
						var params any
						if err := json.Unmarshal([]byte(*cmdParams), &params); err != nil {
							fail("-cmd-params is not JSON: %v", err)
						}
						send("command", map[string]any{"id": "probe-cmd",
							"session": *cmdSession, "method": *cmdMethod, "params": params})
					}
					if *echo > 0 {
						fmt.Printf("     NOTE: -echo takes CONTROL of %s and types %q into it\n",
							pane, *echoKey)
						go func() {
							time.Sleep(400 * time.Millisecond)
							sendInput(pane, []byte(*echoKey))
						}()
					}
				}
			case "pong":
				if rtt == 0 {
					rtt = time.Since(pingSent)
					fmt.Printf("PASS pong control-plane RTT %.2fms\n", float64(rtt.Microseconds())/1000)
				}
			case "result":
				var rr struct {
					ID     string `json:"id"`
					OK     bool   `json:"ok"`
					Result struct {
						Read struct {
							Text string `json:"text"`
						} `json:"read"`
					} `json:"result"`
				}
				_ = json.Unmarshal(e.Data, &rr)
				if strings.HasPrefix(rr.ID, "verify:") {
					// The data plane is FASTER than this poll, so a change seen
					// here is normally already explained by a frame that landed
					// during the previous poll window. Only a change with NO
					// data in that window is unexplained, and only an
					// unexplained window that PERSISTS is a dead stream.
					tg := strings.TrimPrefix(rr.ID, "verify:")
					h := hash(rr.Result.Read.Text)
					if prev, seen := paneHash[tg]; seen && prev != h {
						changedEver[tg] = true
						if !dataSincePoll[tg] && unexplained[tg].IsZero() {
							unexplained[tg] = time.Now()
						}
					}
					if dataSincePoll[tg] {
						unexplained[tg] = time.Time{}
					}
					dataSincePoll[tg] = false
					paneHash[tg] = h
					continue
				}
				fmt.Printf("PASS result %s\n", compact(e.Data))
			case "geometry":
				// K4's flagship frame, and it had no handler here at all.
				// `source` is the whole product statement: "pane" means herdr
				// told us the pane's own size and nothing of the owner's moved;
				// "client" means somebody imposed one.
				var g struct {
					Target string `json:"target"`
					Cols   int    `json:"cols"`
					Rows   int    `json:"rows"`
					Source string `json:"source"`
				}
				if json.Unmarshal(e.Data, &g) == nil && g.Target != "" {
					if geomSource[g.Target] == "" {
						fmt.Printf("PASS geometry %s %dx%d source=%s\n",
							g.Target, g.Cols, g.Rows, g.Source)
					}
					geomSource[g.Target] = g.Source
					geomSize[g.Target] = fmt.Sprintf("%dx%d", g.Cols, g.Rows)
				}
			case "transcript":
				// The product's DEFAULT view. It is control-plane JSON with the
				// ANSI already stripped, so it never shows up on the binary
				// data plane — which is why a harness that only counted binary
				// frames could call a run with zero transcript coverage "ok".
				var tr struct {
					Target    string `json:"target"`
					Source    string `json:"source"`
					Text      string `json:"text"`
					Lines     int    `json:"lines"`
					Truncated bool   `json:"truncated"`
					Agent     bool   `json:"agent"`
					State     string `json:"state"`
				}
				if json.Unmarshal(e.Data, &tr) != nil || tr.Target == "" {
					fmt.Println("FAIL a transcript frame had no target")
					continue
				}
				if !isTranscript[tr.Target] {
					fmt.Printf("FAIL transcript frame for %s, which was never declared transcript\n",
						tr.Target)
				}
				if strings.ContainsRune(tr.Text, '\x1b') {
					fail("transcript for %s still contains ESC: ANSI must be stripped SERVER-side",
						tr.Target)
				}
				trCount[tr.Target]++
				trBytes += len(tr.Text)
				lastData[tr.Target] = time.Now()
				if trCount[tr.Target] == 1 {
					fmt.Printf("PASS transcript %s source=%s lines=%d agent=%v state=%q bytes=%d\n",
						tr.Target, tr.Source, tr.Lines, tr.Agent, tr.State, len(tr.Text))
				}
			case "agent":
				fmt.Printf("PASS agent %s\n", compact(e.Data))
			case "closed":
				fmt.Printf("INFO closed %s\n", compact(e.Data))
			case "error":
				fmt.Printf("WARN error %s\n", compact(e.Data))
			}

		case websocket.BinaryMessage:
			if len(msg) < 11 {
				fmt.Println("FAIL short binary frame")
				continue
			}
			typ := msg[0]
			seq := binary.BigEndian.Uint64(msg[1:9])
			tlen := int(binary.BigEndian.Uint16(msg[9:11]))
			tgt := string(msg[11 : 11+tlen])
			payload := msg[11+tlen:]
			switch typ {
			case 1:
				frames++
				perTarget[tgt]++
				lastData[tgt] = time.Now()
				dataSincePoll[tgt] = true
				unexplained[tgt] = time.Time{}
				dataBytes += len(payload)
				// keystroke -> echo: the first frame after a probe keystroke.
				echoMu.Lock()
				if !echoPending.IsZero() {
					echoSamples = append(echoSamples, time.Since(echoPending))
					echoPending = time.Time{}
					if echoLeft > 0 {
						echoLeft--
					}
				}
				echoMu.Unlock()
				if echoLeft > 0 && len(echoSamples) < *echo {
					go func() {
						time.Sleep(120 * time.Millisecond)
						sendInput(pane, []byte(*echoKey))
					}()
				}
				if firstFrame.IsZero() {
					firstFrame = time.Now()
					fmt.Printf("PASS first frame seq=%d target=%s bytes=%d\n", seq, tgt, len(payload))
					fmt.Printf("     head: %q\n", clip(payload, 72))
				}
			case 2:
				snapshots++
				perTarget[tgt]++
				lastData[tgt] = time.Now()
				dataSincePoll[tgt] = true
				unexplained[tgt] = time.Time{}
				dataBytes += len(payload)
				if snapshots == 1 {
					fmt.Printf("PASS snapshot seq=%d target=%s bytes=%d\n", seq, tgt, len(payload))
				}
			case 3:
				gaps++
				fmt.Printf("INFO gap target=%s dropped=%d\n", tgt, binary.BigEndian.Uint64(payload))
			}
		}
	}

	// 4. Server-side measured latency.
	req, _ := http.NewRequest("GET", "http://"+*addr+"/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer "+*token)
	if mr, err := http.DefaultClient.Do(req); err == nil {
		mb, _ := io.ReadAll(mr.Body)
		mr.Body.Close()
		var pretty map[string]any
		_ = json.Unmarshal(mb, &pretty)
		out, _ := json.MarshalIndent(pretty, "     ", "  ")
		fmt.Printf("\nMEASURED server-side latency (microseconds):\n     %s\n", out)
	}

	if len(echoSamples) > 0 {
		sort.Slice(echoSamples, func(i, j int) bool { return echoSamples[i] < echoSamples[j] })
		q := func(p float64) time.Duration {
			i := int(float64(len(echoSamples))*p) - 1
			if i < 0 {
				i = 0
			}
			return echoSamples[i]
		}
		fmt.Printf("\nMEASURED keystroke -> echo (n=%d): p50 %.2fms  p95 %.2fms  max %.2fms\n",
			len(echoSamples), ms(q(0.5)), ms(q(0.95)), ms(echoSamples[len(echoSamples)-1]))
	}

	fmt.Printf("\nPER-TARGET data frames:\n")
	silent := 0
	var dead []string
	var unmeasured []string
	for _, tg := range trTargets {
		fmt.Printf("     %-28s transcripts=%d (DEFAULT view)\n", tg, trCount[tg])
		// A transcript target that produced NOTHING is not "an idle pane": the
		// first frame is unconditional, because a subscriber that has just
		// arrived needs the current text whether or not it changed. Nothing
		// means the poll never reached it.
		if trCount[tg] == 0 {
			unmeasured = append(unmeasured,
				tg+" (declared transcript, received no `transcript` frame at all)")
		}
	}
	for _, tg := range liveTargets {
		fmt.Printf("     %-28s frames+snapshots=%d pane_changed=%v geometry=%s/%s\n",
			tg, perTarget[tg], changedEver[tg], geomSize[tg], geomSource[tg])
		// K4: with no explicit resize, every LIVE attach must come back with the
		// PANE'S own size. A `client` source here means something imposed a
		// geometry on somebody else's terminal.
		if resizeSent == 0 && geomSource[tg] == "client" {
			fail("%s reported geometry source=client on a run that sent no resize: "+
				"something imposed a size on the pane (K4)", tg)
		}
		if perTarget[tg] == 0 {
			silent++
			// An observe attach delivers a full screen immediately — that is
			// what attaching MEANS. Zero payloads is a target we did not
			// actually measure, and "ok" on an unmeasured target is how a
			// broken build passes.
			unmeasured = append(unmeasured,
				tg+" (declared live, received no snapshot and no frame)")
		}
		// THE acceptance rule. A LIVE target whose pane demonstrably produced
		// output, and which then stayed silent on the data plane for longer
		// than -silent-secs, is a DEAD STREAM. Saying "an idle pane can be
		// legitimately silent" about that is how a broken product passes.
		if since := unexplained[tg]; !since.IsZero() &&
			time.Since(since) > time.Duration(*silentFail)*time.Second {
			dead = append(dead, fmt.Sprintf("%s (pane has been producing output since %s — %s — "+
				"while the LIVE stream delivered nothing; last data %s)",
				tg, since.Format("15:04:05"), time.Since(since).Round(time.Second),
				lastData[tg].Format("15:04:05")))
		}
	}
	if len(dead) > 0 {
		for _, d := range dead {
			fmt.Printf("FAIL dead LIVE stream: %s\n", d)
		}
		fail("%d LIVE target(s) went silent while their pane was producing output", len(dead))
	}
	trFrames := 0
	for _, n := range trCount {
		trFrames += n
	}
	fmt.Printf("\nSUMMARY welcome=%v tree=%v targets=%d (live=%d transcript=%d) "+
		"snapshots=%d frames=%d transcripts=%d gaps=%d payload_bytes=%d transcript_bytes=%d "+
		"resize_sent=%d\n",
		gotWelcome, gotTree, len(targets), len(liveTargets), len(trTargets),
		snapshots, frames, trFrames, gaps, dataBytes, trBytes, resizeSent)
	if silent > 0 {
		fmt.Printf("INFO %d of %d LIVE targets produced no data\n", silent, len(liveTargets))
	}
	if !gotWelcome || !gotTree {
		fail("did not receive welcome + tree")
	}
	// A HARNESS THAT MEASURED NOTHING DID NOT PASS.
	//
	// This used to be `if snapshots+frames == 0`, which let a run through as
	// long as ANY one target said anything — so eleven dead targets out of
	// twelve printed "RESULT ok", and so did a run whose whole declared view
	// produced zero frames. Every target this run declared has to have been
	// measured on the plane it was declared for.
	if len(unmeasured) > 0 {
		for _, u := range unmeasured {
			fmt.Printf("FAIL unmeasured target: %s\n", u)
		}
		fail("%d of %d declared target(s) produced no data at all; this run measured nothing "+
			"for them and cannot be called ok", len(unmeasured), len(targets))
	}
	if snapshots+frames+trFrames == 0 {
		fail("no data of any kind arrived for %v", targets)
	}
	if len(liveTargets) > 0 && snapshots+frames == 0 {
		fail("%d LIVE target(s) declared and not one byte arrived on the data plane", len(liveTargets))
	}
	if len(trTargets) > 0 && trFrames == 0 {
		fail("%d TRANSCRIPT target(s) declared — the product's DEFAULT view — and not one "+
			"`transcript` frame arrived", len(trTargets))
	}
	fmt.Println("RESULT ok")
}

// hash is a cheap content fingerprint; pane.read text can be large.
func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum[:8])
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func compact(r json.RawMessage) string {
	if len(r) > 400 {
		return string(r[:400]) + "..."
	}
	return string(r)
}

func clip(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "FAIL "+f+"\n", a...)
	os.Exit(1)
}
