package main

// rhermes support: rhermes (see ./rhermes in this repo) runs a stock
// `hermes --tui` whose node TUI attaches to a runtime that rhermes owns, and
// opens a unix control socket /tmp/rhermes.<pid> where any local process can
// watch every wire frame and inject turns. This file is tmux-control's client
// for that socket: it discovers the instances, keeps one follower connection
// per instance, buffers the frames it sees, and speaks the line-delimited ctl
// protocol — so a console can drive a session without pushing keystrokes into
// the pane at all.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// RhermesInstance is one running rhermes shim, discovered from /tmp.
type RhermesInstance struct {
	PID  int    `json:"pid"`
	Path string `json:"socket"` // /tmp/rhermes.<pid>
}

// RhermesStatus is the shim's own status frame, plus what we know about the
// follower connection.
type RhermesStatus struct {
	ActiveSession string `json:"active_session,omitempty"`
	TUIAttached   bool   `json:"tui_attached"`
	RuntimeChild  int    `json:"runtime_child,omitempty"`
	TuiPID        int    `json:"tui_pid,omitempty"`
	FramesUp      int64  `json:"frames_up"`
	FramesDown    int64  `json:"frames_down"`
	UptimeS       int    `json:"uptime_s"`
	// follower bookkeeping (tmux-control's own, not the shim's)
	Followed bool   `json:"followed"`
	Note     string `json:"note,omitempty"` // last follower error, if any
}

// discoverRhermes lists the live control sockets in /tmp: a socket named by a
// dead pid is a leftover and is skipped, so callers never see stale entries.
func discoverRhermes() []RhermesInstance {
	matches, err := filepath.Glob("/tmp/rhermes.*")
	if err != nil {
		return nil
	}
	var out []RhermesInstance
	for _, path := range matches {
		tail := filepath.Base(path)
		tail = strings.TrimPrefix(tail, "rhermes.")
		pid := atoiSafe(tail)
		if pid <= 0 || !pidAlive(pid) {
			continue
		}
		out = append(out, RhermesInstance{PID: pid, Path: path})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// rhermesTty asks ps which controlling terminal a pid sits on. Empty when the
// process has none (or is gone) — either way it cannot belong to a pane.
func rhermesTty(pid int) string {
	out, err := runCommand("ps", "-o", "tty=", "-p", fmt.Sprint(pid))
	if err != nil {
		return ""
	}
	tty := strings.TrimSpace(out)
	if tty == "??" || tty == "" {
		return ""
	}
	return "/dev/" + tty
}

// rhermesClient speaks the ctl protocol on one socket. Every call opens its
// own connection: the protocol is line JSON, and a prompt's ack travels on the
// same connection as its later response frames.
type rhermesClient struct {
	path string
}

type ctlError struct{ msg string }

func (e ctlError) Error() string { return e.msg }

// unwrapTap strips a tap envelope: tap lines travel as
// {"tap":dir,"line":"<raw frame>"}, and a caller matching ids must look inside.
func unwrapTap(line string) (dir, inner string) {
	var env struct {
		Tap  string `json:"tap"`
		Line string `json:"line"`
	}
	if json.Unmarshal([]byte(line), &env) == nil && env.Tap != "" && env.Line != "" {
		return env.Tap, env.Line
	}
	return "", line
}

// doneCtl matches the shim's own single-line replies: {"ctl":"<verb>",...}.
func doneCtl(verb string) func(string) bool {
	return func(line string) bool {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			return false
		}
		v, _ := m["ctl"].(string)
		return v == verb
	}
}

// doneInj matches the end of an injected call: the response frame carrying an
// inj- id with a result or an error, or the shim refusing the request outright
// ({"ctl":...,"error":...}). Without a done predicate every call would burn its
// whole read window waiting for lines that never come — a status poll is not a
// stream, and the scan loop polls.
func doneInj(verb string) func(string) bool {
	return func(line string) bool {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			return false
		}
		if id, _ := m["id"].(string); strings.HasPrefix(id, "inj-") {
			_, hasResult := m["result"]
			_, hasError := m["error"]
			return hasResult || hasError
		}
		if v, _ := m["ctl"].(string); v == verb {
			_, hasError := m["error"]
			return hasError
		}
		return false
	}
}

// call sends one ctl line and reads lines until done says the reply is complete
// (or the deadline passes). It returns every line seen, in order, tap envelopes
// unwrapped; callers pick what they need out of them.
func (c rhermesClient) call(obj map[string]any, read time.Duration, done func(string) bool) ([]string, error) {
	conn, err := dialUnix(c.path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	line, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(read))
	var lines []string
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		_, inner := unwrapTap(sc.Text())
		lines = append(lines, inner)
		if done != nil && done(inner) {
			return lines, nil
		}
	}
	if len(lines) == 0 && sc.Err() != nil {
		return nil, ctlError{sc.Err().Error()}
	}
	return lines, nil
}

// status fetches the shim's status frame.
func (c rhermesClient) status() (RhermesStatus, error) {
	lines, err := c.call(map[string]any{"ctl": "status"}, 5*time.Second, doneCtl("status"))
	if err != nil {
		return RhermesStatus{}, err
	}
	var st RhermesStatus
	for _, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if m["ctl"] == "status" {
			b, _ := json.Marshal(m)
			_ = json.Unmarshal(b, &st)
		}
	}
	return st, nil
}

// prompt injects a user turn: it sends the ctl line, then keeps reading until
// the injected frame's result comes back (the shim reports streaming at once,
// and the runtime finishes the turn on its own).
func (c rhermesClient) prompt(text string, wait time.Duration) (map[string]any, error) {
	lines, err := c.call(map[string]any{"ctl": "prompt", "text": text}, wait, doneInj("prompt"))
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if id, _ := m["id"].(string); strings.HasPrefix(id, "inj-") {
			if _, ok := m["result"]; ok {
				return m, nil
			}
		}
		if m["ctl"] == "prompt" && m["ok"] == false {
			if e, _ := m["error"].(string); e != "" {
				return nil, ctlError{e}
			}
		}
	}
	return map[string]any{"lines": lines}, nil
}

// send runs an arbitrary JSON-RPC method through the shim.
func (c rhermesClient) send(method string, params any, wait time.Duration) (map[string]any, error) {
	lines, err := c.call(map[string]any{"ctl": "send", "method": method, "params": params}, wait, doneInj("send"))
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			continue
		}
		if id, _ := m["id"].(string); strings.HasPrefix(id, "inj-") {
			if _, ok := m["result"]; ok {
				return m, nil
			}
		}
	}
	return map[string]any{"lines": lines}, nil
}

// stop asks the shim to tear the whole session down.
func (c rhermesClient) stop() error {
	_, err := c.call(map[string]any{"ctl": "stop"}, 5*time.Second, doneCtl("stop"))
	return err
}

// ------------------------------------------------------------------ follower

// rhermesFrame is one wire frame as mirrored by a tap connection: direction
// plus the JSON the shim saw, clipped to what a console needs.
type rhermesFrame struct {
	Dir     string `json:"dir"` // up (node->runtime), down (runtime->node), inj
	Line    string `json:"line"`
	At      string `json:"at"`
	Clipped bool   `json:"clipped,omitempty"`
}

// frameLimit caps one buffered frame's size, and rhermesFrames caps the ring.
const (
	frameLimit    = 8 * 1024
	rhermesFrames = 4000
)

// follower keeps one tap connection to one shim and appends everything it
// mirrors into a ring buffer. It reconnects on its own: the shim's socket
// survives follower churn, and a dead shim ends the loop quietly.
type follower struct {
	mu      sync.Mutex
	path    string
	buf     []rhermesFrame
	dropped int
	lastErr string
	// alive reports whether the shim behind the socket still exists; it is a
	// field so tests can pin it true without a real pid behind the path.
	alive func() bool
}

func newFollower(path string) *follower {
	return &follower{path: path, alive: func() bool { return pidAlive(pidOfPath(path)) }}
}

// add stores one frame, clipping its body like a tool row: a console wants the
// gist, not the payload.
func (f *follower) add(dir, line string) {
	clipped := false
	if len(line) > frameLimit {
		line = line[:frameLimit]
		clipped = true
	}
	f.mu.Lock()
	if len(f.buf) >= rhermesFrames {
		f.buf = f.buf[1:]
		f.dropped++
	}
	f.buf = append(f.buf, rhermesFrame{
		Dir: dir, Line: line, At: time.Now().UTC().Format(time.RFC3339), Clipped: clipped,
	})
	f.mu.Unlock()
}

// drain hands out up to n buffered frames and empties the buffer — the
// non-blocking pull: whatever arrived since the last drain, right now.
func (f *follower) drain(n int) ([]rhermesFrame, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n <= 0 || n > len(f.buf) {
		n = len(f.buf)
	}
	out := make([]rhermesFrame, n)
	copy(out, f.buf[:n])
	f.buf = f.buf[n:]
	dropped := f.dropped
	f.dropped = 0
	return out, dropped
}

// run maintains the tap connection until ctx is done. Each reconnect re-sends
// {"ctl":"follow"}; the shim answers with an ack line (not mirrored) and then
// every frame, prefixed with a {"tap":...} envelope line.
func (f *follower) run(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}
		if f.alive != nil && !f.alive() {
			return
		}
		err := f.followOnce(stop)
		f.mu.Lock()
		f.lastErr = err.Error()
		f.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// followOnce holds one tap connection until it drops or stop fires.
func (f *follower) followOnce(stop <-chan struct{}) error {
	conn, err := dialUnix(f.path)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{\"ctl\":\"follow\"}\n")); err != nil {
		return err
	}
	conn.SetReadDeadline(time.Time{}) // no deadline: frames may be sparse
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	done := make(chan struct{})
	go func() {
		select {
		case <-stop:
			conn.Close()
		case <-done:
		}
	}()
	defer close(done)
	for sc.Scan() {
		dir, inner := unwrapTap(sc.Text())
		if dir == "" {
			// the follow ack and anything unprefixed; still worth keeping
			dir = "ctl"
			inner = sc.Text()
		}
		f.add(dir, inner)
	}
	return sc.Err()
}

// ------------------------------------------------------------------- per-daemon state

// rhermesHub owns the followers: one per discovered instance, created on
// demand, dropped when the shim dies.
type rhermesHub struct {
	mu    sync.Mutex
	modes map[string]*follower // socket path -> follower
}

var rhermesFollowers = &rhermesHub{modes: map[string]*follower{}}

// followFor returns the follower for one socket, starting it if this is the
// first ask. The loop runs for the daemon's lifetime; a dead shim ends it.
func (h *rhermesHub) followFor(path string) *follower {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.modes[path]
	if f == nil {
		f = newFollower(path)
		h.modes[path] = f
		go f.run(rhermesStop)
	}
	return f
}

// rhermesStop is closed when the whole daemon winds down; every follower loop
// watches it. main() wires it to the same context as the registry.
var rhermesStop = make(chan struct{})

// followerFor returns the follower for a socket path (starting one if needed),
// or nil when the path is empty.
func followerFor(path string) *follower {
	if path == "" {
		return nil
	}
	return rhermesFollowers.followFor(path)
}

// ------------------------------------------------------------------- helpers

// atoiSafe parses a decimal pid, zero on anything else.
func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
		if n > 1<<31 {
			return 0
		}
	}
	return n
}

// pidAlive is the portable signal-0 check: ESRCH means gone; EPERM means the
// pid exists but belongs to another user, which still counts as alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// pidOfPath extracts the pid from a /tmp/rhermes.<pid> socket path.
func pidOfPath(path string) int {
	base := filepath.Base(path)
	return atoiSafe(strings.TrimPrefix(base, "rhermes."))
}

// dialUnix opens a unix socket connection.
func dialUnix(path string) (netConn, error) {
	c, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return unixConn{c}, nil
}

// netConn is the subset of net.Conn the ctl client and follower need. It exists
// so the tests can stand in for real sockets without a filesystem.
type netConn interface {
	Read(b []byte) (int, error)
	Write(b []byte) (int, error)
	Close() error
	SetReadDeadline(t time.Time) error
}

// unixConn adapts net.Conn to netConn (identical, but keeps the interface honest
// for test doubles).
type unixConn struct{ net.Conn }

func (c unixConn) SetReadDeadline(t time.Time) error { return c.Conn.SetReadDeadline(t) }

// runCommand runs a command and returns its stdout.
func runCommand(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}
