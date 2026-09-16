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
	"time"

	"github.com/gorilla/websocket"
)

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
	rows := flag.Int("rows", 30, "terminal rows")
	flag.Parse()

	if *token == "" {
		fail("no token: pass -token or set $HERDR_EXPOSE_TOKEN")
	}

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
	hdr := http.Header{"Authorization": {"Bearer " + *token}}
	c, hresp, err := d.Dial("ws://"+*addr+"/v1/stream", hdr)
	if err != nil {
		if hresp != nil {
			fail("dial: %v (HTTP %d)", err, hresp.StatusCode)
		}
		fail("dial: %v", err)
	}
	defer c.Close()
	fmt.Println("PASS websocket upgraded")

	// 3. Rejection before upgrade is part of the contract.
	if _, r2, err := d.Dial("ws://"+*addr+"/v1/stream", http.Header{"Authorization": {"Bearer wrong"}}); err == nil {
		fail("FAIL a bad token was accepted")
	} else if r2 == nil || r2.StatusCode != http.StatusUnauthorized {
		fmt.Println("WARN bad token rejected, but not with 401")
	} else {
		fmt.Println("PASS bad token rejected with 401 before upgrade")
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
	)

	deadline := time.Now().Add(time.Duration(*secs) * time.Second)
	_ = c.SetReadDeadline(deadline.Add(2 * time.Second))

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
					var t struct {
						Connected bool `json:"connected"`
						Snapshot  struct {
							FocusedPane string `json:"focused_pane_id"`
							Panes       []struct {
								PaneID string `json:"pane_id"`
								Agent  string `json:"agent"`
								Title  string `json:"terminal_title_stripped"`
							} `json:"panes"`
						} `json:"snapshot"`
					}
					_ = json.Unmarshal(e.Data, &t)
					fmt.Printf("PASS tree upstream_connected=%v panes=%d\n",
						t.Connected, len(t.Snapshot.Panes))
					for _, p := range t.Snapshot.Panes {
						fmt.Printf("     pane %-10s agent=%-8s %s\n", p.PaneID, p.Agent, p.Title)
					}
					if pane == "" {
						pane = t.Snapshot.FocusedPane
						if pane == "" && len(t.Snapshot.Panes) > 0 {
							pane = t.Snapshot.Panes[0].PaneID
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
				}
			case "pong":
				if rtt == 0 {
					rtt = time.Since(pingSent)
					fmt.Printf("PASS pong control-plane RTT %.2fms\n", float64(rtt.Microseconds())/1000)
				}
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
