package main

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeShim is a control socket that answers status and echoes prompts the way
// the real shim does: an ack line, then the mirrored frame, then the inj-
// result. Enough protocol to keep the client honest, no hermes involved.
type fakeShim struct {
	dir  string
	path string
	ln   net.Listener
}

func newFakeShim(t *testing.T, pid int) *fakeShim {
	t.Helper()
	// macOS temp dirs run past the 104-byte unix-socket path limit, so the fake
	// shim lives in a short /tmp directory of its own; tests run one at a time.
	dir, err := os.MkdirTemp("/tmp", "rhmtest")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "rhermes."+itoa(pid))
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fs := &fakeShim{dir: dir, path: path, ln: ln}
	go fs.serve()
	t.Cleanup(func() { ln.Close() })
	return fs
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (fs *fakeShim) serve() {
	for {
		conn, err := fs.ln.Accept()
		if err != nil {
			return
		}
		go fs.handle(conn)
	}
}

func (fs *fakeShim) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		var obj map[string]any
		if json.Unmarshal(sc.Bytes(), &obj) != nil {
			continue
		}
		switch obj["ctl"] {
		case "status":
			b, _ := json.Marshal(map[string]any{
				"ctl": "status", "active_session": "20260101_000000_abcdef",
				"tui_attached": true, "frames_up": 3, "frames_down": 4, "uptime_s": 9,
			})
			conn.Write(append(b, '\n'))
		case "follow":
			conn.Write([]byte("{\"ctl\":\"follow\",\"ok\":true}\n"))
			conn.Write([]byte("{\"tap\":\"down\",\"line\":\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"f\\\":1}\"}\n"))
			conn.Write([]byte("{\"tap\":\"up\",\"line\":\"{\\\"jsonrpc\\\":\\\"2.0\\\",\\\"f\\\":2}\"}\n"))
		case "prompt":
			b, _ := json.Marshal(map[string]any{"ctl": "prompt", "ok": true, "id": "inj-1", "session_id": "s"})
			conn.Write(append(b, '\n'))
			inner, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "inj-1", "result": map[string]any{"status": "streaming"}})
			env, _ := json.Marshal(map[string]any{"tap": "down", "line": string(inner)})
			conn.Write(append(env, '\n'))
		case "send":
			b, _ := json.Marshal(map[string]any{"ctl": "send", "ok": true, "id": "inj-2"})
			conn.Write(append(b, '\n'))
			inner, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "inj-2", "result": map[string]any{"session_id": "s"}})
			env, _ := json.Marshal(map[string]any{"tap": "down", "line": string(inner)})
			conn.Write(append(env, '\n'))
		case "stop":
			b, _ := json.Marshal(map[string]any{"ctl": "stop", "ok": true})
			conn.Write(append(b, '\n'))
		}
	}
}

// A socket named by a live pid is found; one named by a dead pid is skipped —
// discovery must not hand a caller a leftover.
func TestDiscoverRhermesSkipsDeadPids(t *testing.T) {
	live := os.Getpid()
	// the glob only sees /tmp/rhermes.*, so the live fake binds exactly there
	ln, err := net.Listen("unix", "/tmp/rhermes."+itoa(live))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	// a socket whose pid is this test process's pid+1000000 (dead): the glob
	// sees it, discovery must not
	dead := "/tmp/rhermes." + itoa(live+1000000)
	if err := os.WriteFile(dead, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(dead)
	got := discoverRhermes()
	found := false
	for _, inst := range got {
		if inst.Path == "/tmp/rhermes."+itoa(live) {
			found = true
		}
		if inst.Path == dead {
			t.Fatalf("discovery returned a socket named by a dead pid: %s", dead)
		}
	}
	if !found {
		t.Fatalf("discovery missed the live socket (got %v)", got)
	}
}

// The client reads the status frame out of the wire noise.
func TestRhermesClientStatus(t *testing.T) {
	fs := newFakeShim(t, os.Getpid()+7)
	st, err := (rhermesClient{path: fs.path}).status()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.TUIAttached || st.ActiveSession != "20260101_000000_abcdef" || st.FramesUp != 3 {
		t.Fatalf("status = %+v", st)
	}
}

// A prompt returns once the inj- result is on the wire, and a send resolves its
// own result; both tolerate the ack line coming first.
func TestRhermesClientPromptAndSend(t *testing.T) {
	fs := newFakeShim(t, os.Getpid()+8)
	c := rhermesClient{path: fs.path}
	res, err := c.prompt("hi", 5*time.Second)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if id, _ := res["id"].(string); id != "inj-1" {
		t.Fatalf("prompt result id = %v", res["id"])
	}
	res, err = c.send("session.most_recent", map[string]any{}, 5*time.Second)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if id, _ := res["id"].(string); id != "inj-2" {
		t.Fatalf("send result id = %v", res["id"])
	}
}

// The follower buffers mirrored frames; a drain is non-blocking and empties.
func TestFollowerDrains(t *testing.T) {
	fs := newFakeShim(t, os.Getpid()+9)
	f := newFollower(fs.path)
	f.alive = func() bool { return true }
	stop := make(chan struct{})
	go f.run(stop)
	defer close(stop)
	// wait until both frames arrived
	deadline := time.Now().Add(5 * time.Second)
	for {
		frames, _ := f.drain(0)
		if len(frames) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("follower never saw the frames (lastErr=%s)", f.lastErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	frames, dropped := f.drain(0)
	if len(frames) != 0 || dropped != 0 {
		t.Fatalf("second drain should be empty: %d %d", len(frames), dropped)
	}
}

// add clips oversized frames rather than buffering a payload whole.
func TestFollowerClips(t *testing.T) {
	f := newFollower("/nonexistent")
	f.add("down", strings.Repeat("x", frameLimit*3))
	frames, _ := f.drain(0)
	if len(frames) != 1 || len(frames[0].Line) != frameLimit || !frames[0].Clipped {
		t.Fatalf("clip failed: %d %v", len(frames), frames)
	}
}
