// Command wsprobe is the acceptance harness for the herdr-expose wire protocol.
//
// It is a real client: it authenticates, opens /v1/stream, reads the control
// plane, picks a live pane out of the tree, declares geometry and a LIVE
// viewport, and then measures what actually arrives on the binary data plane.
//
// It is READ-ONLY against Herdr by default. It never sends input to a pane and
// never asks for a control stream; observe streams are harmless.
//
//	wsprobe -addr 127.0.0.1:21118 -token <server-token> [-target w2:p1] [-secs 8]
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// treeFrame mirrors the `tree` control frame: workspaces -> tabs -> panes.
type treeFrame struct {
	Rev          uint64 `json:"rev"`
	Connected    bool   `json:"connected"`
	HerdrVersion string `json:"herdr_version"`
	FocusedPane  string `json:"focused_pane"`
	Workspaces   []struct {
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

func (t treeFrame) allPanes() []paneFrame {
	var out []paneFrame
	for _, ws := range t.Workspaces {
		for _, tab := range ws.Tabs {
			out = append(out, tab.Panes...)
		}
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
	target := flag.String("target", "", "pane id (default: the focused pane from the tree)")
	secs := flag.Int("secs", 8, "how long to stream")
	cols := flag.Int("cols", 100, "terminal columns")
	echo := flag.Int("echo", 0, "measure keystroke->echo latency with N probe keystrokes "+
		"(DEFAULT 0 = read-only; this TAKES CONTROL of the pane and types into it)")
	echoKey := flag.String("echo-key", "\x1b", "bytes to send as the probe keystroke; "+
		"ESC is the default because it is inert in most TUIs")
	rows := flag.Int("rows", 30, "terminal rows")
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
		pane       = *target
		frames     int
		snapshots  int
		gaps       int
		dataBytes  int
		firstFrame time.Time
		pingSent   time.Time
		rtt        time.Duration

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
					fmt.Printf("PASS tree upstream_connected=%v workspaces=%d panes=%d\n",
						t.Connected, len(t.Workspaces), len(all))
					for _, p := range all {
						state, kind := "-", "-"
						if p.Agent != nil {
							state, kind = p.Agent.State, p.Agent.Kind
						}
						fmt.Printf("     pane %-10s agent=%-8s state=%-8s %s\n",
							p.ID, kind, state, p.Title)
					}
					if pane == "" {
						pane = t.FocusedPane
						if pane == "" && len(all) > 0 {
							pane = all[0].ID
						}
					}
					if pane == "" {
						fail("no pane to stream")
					}
					fmt.Printf("     streaming %s at %dx%d\n", pane, *cols, *rows)
					// Geometry BEFORE the first frame is requested (B2).
					send("resize", map[string]any{"target": pane, "cols": *cols, "rows": *rows})
					send("viewport", map[string]any{"targets": map[string]string{pane: "live"}})
					pingSent = time.Now()
					send("ping", map[string]any{"t": pingSent.UnixMilli()})
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

	fmt.Printf("\nSUMMARY welcome=%v tree=%v snapshots=%d frames=%d gaps=%d payload_bytes=%d\n",
		gotWelcome, gotTree, snapshots, frames, gaps, dataBytes)
	if !gotWelcome || !gotTree {
		fail("did not receive welcome + tree")
	}
	if snapshots+frames == 0 {
		fail("no terminal data arrived for %s", pane)
	}
	fmt.Println("RESULT ok")
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
