package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// PaneSource tells how a pane got associated with a hermes session.
type PaneSource int

const (
	SourceNone PaneSource = iota
	SourceLease
	SourceMarker
	SourceProc
)

func (s PaneSource) String() string {
	switch s {
	case SourceLease:
		return "lease"
	case SourceMarker:
		return "marker"
	case SourceProc:
		return "proc"
	}
	return "none"
}

// PaneAssoc is one hermes process attached to a pane's tty.
type PaneAssoc struct {
	PID        int    `json:"pid"`
	Running    bool   `json:"running"`
	Suspended  bool   `json:"suspended"`
	SessionID  string `json:"session_id,omitempty"`
	Profile    string `json:"profile,omitempty"`
	Source     string `json:"source"`
	LeaseStale bool   `json:"lease_stale,omitempty"`
	Cmdline    string `json:"cmdline,omitempty"`
}

// PaneStatus describes one tmux pane and its hermes association.
type PaneStatus struct {
	Pane       Pane        `json:"pane"`
	HasHermes  bool        `json:"has_hermes"`
	Assocs     []PaneAssoc `json:"associations"`
	Source     string      `json:"source,omitempty"`
	Running    bool        `json:"running"`
	Status     string      `json:"status"`
	SessionID  string      `json:"session_id,omitempty"`
	Profile    string      `json:"profile,omitempty"`
	Session    *SessionRow `json:"session,omitempty"`
	LastQuery  *MsgRow     `json:"last_query,omitempty"`
	LastAnswer *MsgRow     `json:"last_answer,omitempty"`
	// Steps is how many tool calls the agent has made since the last human
	// question: the turn's progress as a number, for a console that wants to show
	// activity without reading the database.
	Steps         int            `json:"steps_since_query,omitempty"`
	ExchangeState string         `json:"exchange_state,omitempty"`
	Turn          *TurnState     `json:"turn,omitempty"`
	PaneLines     []string       `json:"pane_content,omitempty"`
	Rhermes       *RhermesStatus `json:"rhermes,omitempty"`
	RhermesPID    int            `json:"rhermes_pid,omitempty"`
	RhermesPath   string         `json:"rhermes_socket,omitempty"`
}

var sessionIDRe = regexp.MustCompile(`\b\d{8}_[0-9]{6}_[0-9a-f]{6,}\b`)
var hermesProcRe = regexp.MustCompile(`\.hermes/hermes-agent/`)

// Registry holds mutable state and scans the system periodically.
type Registry struct {
	mu         sync.RWMutex
	hermesHome string
	dbPath     string
	turnWindow time.Duration
	enterDelay time.Duration
	captureMax int
	// rhermesAccess selects how a rhermes-backed session is driven: keystrokes
	// pushes into the pane as before, socket talks to the shim's control socket,
	// auto uses the socket when the pane has a shim and keystrokes otherwise.
	rhermesAccess string
	logs          map[string]*logTail  // agent.log path -> tail state, one per profile
	turnState     map[string]TurnState // session id -> last turn state
	panes         map[PaneID]PaneStatus
	updated       time.Time
}

// logTail remembers how far one profile's agent.log has been read.
type logTail struct {
	mod    time.Time
	offset int64
}

// TurnState is the outcome of the last observed turn for a session.
type TurnState struct {
	State   string    `json:"state"` // running / answered / interrupted / none
	Since   time.Time `json:"since"`
	LogLine int64     `json:"log_line"`
	Detail  string    `json:"detail,omitempty"`
}

// NewRegistry builds a Registry.
func NewRegistry(hermesHome string, dbPath string, turnWindow time.Duration, captureMax int, rhermesAccess string) *Registry {
	return &Registry{
		hermesHome:    hermesHome,
		dbPath:        dbPath,
		turnWindow:    turnWindow,
		captureMax:    captureMax,
		rhermesAccess: rhermesAccess,
		logs:          map[string]*logTail{},
		turnState:     map[string]TurnState{},
		panes:         map[PaneID]PaneStatus{},
	}
}

// Run polls until ctx is done.
func (r *Registry) Run(ctx context.Context, every time.Duration) {
	r.refreshLogState()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.scanOnce()
		r.refreshLogState()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// refreshLogState tails every profile's agent.log and updates per-session turn
// state. One log per profile: a pane running profile rtassist has its turns in
// profiles/rtassist/logs/agent.log, not in the default home's log.
func (r *Registry) refreshLogState() {
	for _, path := range r.logPaths() {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		st := r.logs[path]
		if st == nil {
			st = &logTail{}
			r.logs[path] = st
		}
		if !st.mod.IsZero() && !info.ModTime().After(st.mod) {
			continue
		}
		st.mod = info.ModTime()
		lines, total, err := tailLines(path, 4000)
		if err != nil {
			continue
		}
		for _, line := range lines {
			ev := parseTurnLine(line)
			if ev.Kind == TurnNone || ev.SessionID == "" {
				continue
			}
			if ev.Kind == TurnStarted {
				r.turnState[ev.SessionID] = TurnState{State: "running", Since: parseLogTS(ev.TS), Detail: ev.QueryHint}
			} else {
				st := "answered"
				if strings.Contains(ev.Reason, "interrupt") {
					st = "interrupted"
				}
				r.turnState[ev.SessionID] = TurnState{State: st, Since: parseLogTS(ev.TS), Detail: ev.Reason}
			}
		}
		st.offset = total
	}
}

// logPaths is the default profile's log plus one per profile seen in the panes.
func (r *Registry) logPaths() []string {
	out := []string{filepath.Join(r.hermesHome, "logs", "agent.log")}
	seen := map[string]bool{}
	r.mu.RLock()
	for _, ps := range r.panes {
		p := ps.Profile
		if p == "" || p == "default" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, filepath.Join(r.hermesHome, "profiles", p, "logs", "agent.log"))
	}
	r.mu.RUnlock()
	return out
}

// parseLogTS parses "2026-10-05 12:56:09,473" into UTC time.
func parseLogTS(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05", strings.SplitN(s, ",", 2)[0])
	if err != nil {
		return time.Time{}
	}
	return t
}

// scanOnce performs one full scan of tmux + proc + hermes files.
func (r *Registry) scanOnce() {
	panes, err := listPanes()
	if err != nil {
		log.Printf("scan: listPanes: %v", err)
		return
	}
	procs := scanHermesProcs()

	// rhermes shims are matched to panes by controlling terminal, the same way
	// hermes processes are: the shim sits in the pane (it runs the stock TUI as
	// its child), so its tty is the pane's tty. A shim on an unknown tty is kept
	// out of the panes but still followed, so its frames are available through
	// the API even when tmux knows nothing about it.
	rhByTty := map[string]RhermesInstance{}
	for _, inst := range discoverRhermes() {
		// follow every instance from birth: frames accumulate whether or not
		// anyone is watching yet, and a drain is then never blind
		followerFor(inst.Path)
		if tty := rhermesTty(inst.PID); tty != "" {
			rhByTty[tty] = inst
		}
	}

	// Leases and markers live in each profile's own home, and nowhere else: the
	// default home's files say nothing about a pane running profile rtassist.
	profiles := map[string]bool{"": true}
	for _, pr := range procs {
		profiles[pr.profile] = true
	}
	leases := map[int]LeaseEntry{}
	markers := map[string]MarkerEntry{} // "<profile>|<tty>" -> marker
	for p := range profiles {
		home := r.profileHome(p)
		if lf, err := LoadLeases(home); err != nil {
			log.Printf("scan: leases %s: %v", home, err)
		} else {
			for pid, le := range lf {
				leases[pid] = le
			}
		}
		if mm, err := LoadMarkers(home); err != nil {
			log.Printf("scan: markers %s: %v", home, err)
		} else {
			for tty, m := range mm {
				markers[p+"|"+tty] = m
			}
		}
	}

	// tty -> hermes processes
	ttyProcs := map[string][]PaneAssoc{}
	for _, pr := range procs {
		a := PaneAssoc{
			PID:       pr.pid,
			Running:   !pr.suspended,
			Suspended: pr.suspended,
			Profile:   pr.profile,
			Cmdline:   pr.cmdline,
		}
		if le, ok := leases[pr.pid]; ok && procAlive(pr.pid, le.ProcessStartTime) {
			// live_session_id is the session being served right now; it can be
			// empty, in which case the lease's own session id is the answer.
			a.SessionID = le.LiveSessionID
			if a.SessionID == "" {
				a.SessionID = le.SessionID
			}
			a.Source = SourceLease.String()
		} else {
			// found by the process scan alone: no lease names its session
			a.Source = SourceProc.String()
		}
		ttyProcs[pr.tty] = append(ttyProcs[pr.tty], a)
	}

	out := map[PaneID]PaneStatus{}
	for id, p := range panes {
		ps := PaneStatus{Pane: p}
		assocs := ttyProcs[p.TTY]
		ps.Assocs = assocs
		ps.Profile = ps.profileOf()
		if inst, ok := rhByTty[p.TTY]; ok {
			ps.RhermesPID = inst.PID
			ps.RhermesPath = inst.Path
			if st, err := (rhermesClient{path: inst.Path}).status(); err == nil {
				st.Followed = true
				ps.Rhermes = &st
				// the shim's runtime serves the session; the pane has hermes
				// because the stock TUI runs inside it
				ps.HasHermes = true
				ps.Running = true
				if ps.Status == "" || ps.Status == "no-hermes" {
					ps.Status = "running"
				}
				if ps.SessionID == "" {
					ps.SessionID = st.ActiveSession
				}
			} else {
				ps.Rhermes = &RhermesStatus{Note: err.Error()}
			}
		}
		for _, a := range assocs {
			if a.Running {
				ps.HasHermes = true
				ps.Running = true
			} else {
				ps.HasHermes = true
			}
		}
		switch {
		case p.Dead:
			ps.Status = "dead-pane"
		case ps.Running:
			ps.Status = "running"
		case ps.HasHermes:
			ps.Status = "suspended-only"
		default:
			ps.Status = "no-hermes"
		}
		// session id: prefer per-tty marker (current), else lease, else none
		if ps.HasHermes {
			sid := ""
			if m, ok := markers[ps.Profile+"|"+p.TTY]; ok {
				sid = m.SessionID
			}
			if sid == "" {
				for _, a := range assocs {
					if a.SessionID != "" {
						sid = a.SessionID
						break
					}
				}
			}
			ps.SessionID = sid
			if a := ps.runningAssoc(); a != nil {
				ps.Source = a.Source
			}
		}
		out[id] = ps
	}

	r.mu.Lock()
	r.panes = out
	r.updated = time.Now()
	r.mu.Unlock()
}

// runningAssoc returns the first running association, if any.
func (ps *PaneStatus) runningAssoc() *PaneAssoc {
	for i := range ps.Assocs {
		if ps.Assocs[i].Running {
			return &ps.Assocs[i]
		}
	}
	return nil
}

// hermesProc is one detected hermes process, as found by the platform-specific
// scan (proc_linux.go reads /proc, proc_darwin.go shells out to ps). profile is
// the hermes profile it runs under, empty for the default one.
type hermesProc struct {
	pid       int
	tty       string
	suspended bool
	cmdline   string
	profile   string
}

// profileHome is the hermes home a profile keeps its state in: the registry's own
// home for the default profile, <home>/profiles/<name> otherwise. Leases, markers,
// logs and state.db all live under it, so nothing may be read from the wrong home.
func (r *Registry) profileHome(profile string) string {
	if profile == "" || profile == "default" {
		return r.hermesHome
	}
	return filepath.Join(r.hermesHome, "profiles", profile)
}

// dbFor is the state.db that holds a pane's session.
func (r *Registry) dbFor(profile string) string {
	if profile == "" || profile == "default" {
		return r.dbPath
	}
	return filepath.Join(r.profileHome(profile), "state.db")
}

// profileOf reports the profile of the pane's hermes process, preferring the one
// that is running.
func (ps *PaneStatus) profileOf() string {
	if a := ps.runningAssoc(); a != nil && a.Profile != "" {
		return a.Profile
	}
	for i := range ps.Assocs {
		if ps.Assocs[i].Profile != "" {
			return ps.Assocs[i].Profile
		}
	}
	return ""
}
