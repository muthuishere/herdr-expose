// Command stress is the load harness for herdr-expose.
//
// It is a real protocol client (control plane JSON + binary data plane) that
// can be pointed at an instance with N concurrent connections, every pane or a
// chosen set, in live / transcript / summary mode, and can deliberately STALL
// one client to prove memory stays bounded under backpressure.
//
// It is READ-ONLY: it never sends `input`, never sends `resize` unless asked
// with -resize, and only ever issues read methods.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type envelope struct {
	Seq  uint64          `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type pane struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Agent *struct {
		Kind  string `json:"kind"`
		State string `json:"state"`
	} `json:"agent"`
}

type tree struct {
	Sessions []struct {
		ID         string `json:"id"`
		Connected  bool   `json:"connected"`
		Workspaces []struct {
			Tabs []struct {
				Panes []pane `json:"panes"`
			} `json:"tabs"`
		} `json:"workspaces"`
	} `json:"sessions"`
}

func (t tree) panes() []string {
	var out []string
	for _, s := range t.Sessions {
		if !s.Connected {
			continue
		}
		for _, w := range s.Workspaces {
			for _, tb := range w.Tabs {
				for _, p := range tb.Panes {
					out = append(out, p.ID)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

type stats struct {
	binFrames  atomic.Int64
	binBytes   atomic.Int64
	snapshots  atomic.Int64
	gaps       atomic.Int64
	transcript atomic.Int64
	trBytes    atomic.Int64
	trees      atomic.Int64
	treeBytes  atomic.Int64
	cycles     atomic.Int64
	errs       atomic.Int64

	mu   sync.Mutex
	rtts []time.Duration
	seen map[string]int
}

func main() {
	addr := flag.String("addr", "127.0.0.1:21118", "server address")
	token := flag.String("token", "", "bearer token (blank in local mode)")
	clients := flag.Int("clients", 1, "concurrent websocket connections")
	mode := flag.String("mode", "live", "live|transcript|summary|none")
	targetsFlag := flag.String("targets", "", "comma list, or 'all', or blank = first pane")
	limit := flag.Int("limit", 0, "cap the number of targets (0 = no cap)")
	secs := flag.Int("secs", 20, "run duration")
	stallAfter := flag.Int("stall-after", 0, "seconds after which client 0 stops reading (0 = never)")
	resize := flag.Bool("resize", false, "send an explicit resize (MUTATES the pane) before viewport")
	cols := flag.Int("cols", 100, "cols for -resize")
	rows := flag.Int("rows", 30, "rows for -resize")
	label := flag.String("label", "", "label for the report line")
	dump := flag.String("dump-trees", "", "write every `tree` frame to this file, newline separated")
	dumpTr := flag.String("dump-transcripts", "", "write every `transcript` frame (with arrival time) to this file")
	cycle := flag.Int("cycle-ms", 0, "reconnect storm: each client closes and redials every N ms")
	flag.Parse()

	base := "http://" + *addr
	// Discover the tree with one throwaway connection.
	all := discover(base, *addr, *token)
	var targets []string
	switch {
	case *targetsFlag == "all":
		targets = all
	case *targetsFlag != "":
		for _, t := range strings.Split(*targetsFlag, ",") {
			if t = strings.TrimSpace(t); t != "" {
				targets = append(targets, t)
			}
		}
	default:
		if len(all) > 0 {
			targets = all[:1]
		}
	}
	if *limit > 0 && len(targets) > *limit {
		targets = targets[:*limit]
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "no targets")
		os.Exit(1)
	}

	st := &stats{seen: map[string]int{}}
	start := time.Now()
	deadline := start.Add(time.Duration(*secs) * time.Second)

	var wg sync.WaitGroup
	for i := 0; i < *clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stall := time.Duration(0)
			if *stallAfter > 0 && i == 0 {
				stall = time.Duration(*stallAfter) * time.Second
			}
			if *cycle > 0 {
				for time.Now().Before(deadline) {
					d := time.Now().Add(time.Duration(*cycle) * time.Millisecond)
					if d.After(deadline) {
						d = deadline
					}
					runClient(i, *addr, *token, targets, *mode, d, st, 0, *resize, *cols, *rows, "", "")
					st.cycles.Add(1)
				}
				return
			}
			runClient(i, *addr, *token, targets, *mode, deadline, st, stall, *resize, *cols, *rows, *dump, *dumpTr)
		}(i)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()

	st.mu.Lock()
	rtts := append([]time.Duration(nil), st.rtts...)
	covered := len(st.seen)
	st.mu.Unlock()
	sort.Slice(rtts, func(a, b int) bool { return rtts[a] < rtts[b] })
	q := func(p float64) float64 {
		if len(rtts) == 0 {
			return 0
		}
		i := int(float64(len(rtts))*p) - 1
		if i < 0 {
			i = 0
		}
		return float64(rtts[i].Microseconds()) / 1000
	}

	out := map[string]any{
		"label": *label, "clients": *clients, "mode": *mode,
		"targets": len(targets), "targets_with_data": covered,
		"secs":        elapsed,
		"bin_frames":  st.binFrames.Load(),
		"bin_bytes":   st.binBytes.Load(),
		"snapshots":   st.snapshots.Load(),
		"gaps":        st.gaps.Load(),
		"transcripts": st.transcript.Load(),
		"tr_bytes":    st.trBytes.Load(),
		"trees":       st.trees.Load(),
		"tree_bytes":  st.treeBytes.Load(),
		"errors":      st.errs.Load(),
		"cycles":      st.cycles.Load(),
		"bytes_per_s": float64(st.binBytes.Load()+st.trBytes.Load()) / elapsed,
		"rtt_p50_ms":  q(0.5), "rtt_p95_ms": q(0.95), "rtt_n": len(rtts),
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
}

func discover(base, addr, token string) []string {
	c := dial(addr, token)
	if c == nil {
		return nil
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	send(c, "hello", map[string]any{"protocol": "v1"})
	for {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			return nil
		}
		if mt != websocket.TextMessage {
			continue
		}
		var e envelope
		if json.Unmarshal(msg, &e) != nil || e.Type != "tree" {
			continue
		}
		var t tree
		_ = json.Unmarshal(e.Data, &t)
		return t.panes()
	}
}

func dial(addr, token string) *websocket.Conn {
	d := websocket.Dialer{EnableCompression: true, HandshakeTimeout: 15 * time.Second,
		NetDial: func(n, a string) (net.Conn, error) { return net.DialTimeout(n, a, 10*time.Second) }}
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	c, resp, err := d.Dial("ws://"+addr+"/v1/stream", h)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Fprintf(os.Stderr, "dial: %v (HTTP %d) %s\n", err, code, strings.TrimSpace(string(b)))
		} else {
			fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		}
		return nil
	}
	return c
}

func send(c *websocket.Conn, typ string, data any) {
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	_ = c.WriteMessage(websocket.TextMessage, b)
}

func runClient(idx int, addr, token string, targets []string, mode string,
	deadline time.Time, st *stats, stall time.Duration, resize bool, cols, rows int, dump, dumpTr string) {

	var dumpF, dumpT *os.File
	if dump != "" && idx == 0 {
		dumpF, _ = os.Create(dump)
		if dumpF != nil {
			defer dumpF.Close()
		}
	}
	if dumpTr != "" && idx == 0 {
		dumpT, _ = os.Create(dumpTr)
		if dumpT != nil {
			defer dumpT.Close()
		}
	}

	c := dial(addr, token)
	if c == nil {
		st.errs.Add(1)
		return
	}
	defer c.Close()
	_ = c.SetReadDeadline(deadline.Add(5 * time.Second))
	send(c, "hello", map[string]any{"protocol": "v1"})

	view := map[string]string{}
	for _, t := range targets {
		if resize {
			send(c, "resize", map[string]any{"target": t, "cols": cols, "rows": rows})
		}
		view[t] = mode
	}
	send(c, "viewport", map[string]any{"targets": view})

	// Ping every 2s for control-plane RTT.
	stop := make(chan struct{})
	var pingMu sync.Mutex
	pingAt := map[int64]time.Time{}
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				now := time.Now()
				pingMu.Lock()
				pingAt[now.UnixNano()] = now
				pingMu.Unlock()
				send(c, "ping", map[string]any{"t": now.UnixNano()})
			}
		}
	}()
	defer close(stop)

	stallAt := time.Time{}
	if stall > 0 {
		stallAt = time.Now().Add(stall)
	}

	for time.Now().Before(deadline) {
		if !stallAt.IsZero() && time.Now().After(stallAt) {
			// STOP READING. The socket buffer fills, then the server's own
			// queue must bound itself. Hold the connection open, read nothing.
			time.Sleep(time.Until(deadline))
			return
		}
		mt, msg, err := c.ReadMessage()
		if err != nil {
			st.errs.Add(1)
			return
		}
		switch mt {
		case websocket.TextMessage:
			var e envelope
			if json.Unmarshal(msg, &e) != nil {
				continue
			}
			switch e.Type {
			case "tree":
				st.trees.Add(1)
				st.treeBytes.Add(int64(len(msg)))
				if dumpF != nil {
					dumpF.Write(msg)
					dumpF.Write([]byte("\n"))
				}
			case "transcript":
				st.transcript.Add(1)
				st.trBytes.Add(int64(len(msg)))
				var d struct {
					Target string `json:"target"`
				}
				_ = json.Unmarshal(e.Data, &d)
				st.mu.Lock()
				st.seen[d.Target]++
				st.mu.Unlock()
				if dumpT != nil {
					fmt.Fprintf(dumpT, "%d\t%s\t%d\n", time.Now().UnixMilli(), d.Target, len(msg))
				}
			case "pong":
				var d struct {
					T int64 `json:"t"`
				}
				if json.Unmarshal(e.Data, &d) == nil {
					pingMu.Lock()
					at, ok := pingAt[d.T]
					delete(pingAt, d.T)
					pingMu.Unlock()
					if ok {
						st.mu.Lock()
						st.rtts = append(st.rtts, time.Since(at))
						st.mu.Unlock()
					}
				}
			}
		case websocket.BinaryMessage:
			if len(msg) < 11 {
				continue
			}
			typ := msg[0]
			tlen := int(binary.BigEndian.Uint16(msg[9:11]))
			if 11+tlen > len(msg) {
				continue
			}
			tgt := string(msg[11 : 11+tlen])
			payload := len(msg) - 11 - tlen
			switch typ {
			case 1:
				st.binFrames.Add(1)
			case 2:
				st.snapshots.Add(1)
			case 3:
				st.gaps.Add(1)
			}
			st.binBytes.Add(int64(payload))
			st.mu.Lock()
			st.seen[tgt]++
			st.mu.Unlock()
		}
	}
}
