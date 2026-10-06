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
	Source     string `json:"source"`
	LeaseStale bool   `json:"lease_stale,omitempty"`
	Cmdline    string `json:"cmdline,omitempty"`
}

// PaneStatus describes one tmux pane and its hermes association.
type PaneStatus struct {
	Pane          Pane        `json:"pane"`
	HasHermes     bool        `json:"has_hermes"`
	Assocs        []PaneAssoc `json:"associations"`
	Source        string      `json:"source,omitempty"`
	Running       bool        `json:"running"`
	Status        string      `json:"status"`
	SessionID     string      `json:"session_id,omitempty"`
	Session       *SessionRow `json:"session,omitempty"`
	LastQuery     *MsgRow     `json:"last_query,omitempty"`
	LastAnswer    *MsgRow     `json:"last_answer,omitempty"`
	ExchangeState string      `json:"exchange_state,omitempty"`
	Turn          *TurnState  `json:"turn,omitempty"`
	PaneLines     []string    `json:"pane_content,omitempty"`
}

var sessionIDRe = regexp.MustCompile(`\b\d{8}_[0-9]{6}_[0-9a-f]{6,}\b`)
var hermesProcRe = regexp.MustCompile(`\.hermes/hermes-agent/`)

// Registry holds mutable state and scans the system periodically.
type Registry struct {
	mu           sync.RWMutex
	hermesHome   string
	dbPath       string
	turnWindow   time.Duration
	enterDelay   time.Duration
	captureMax   int
	logOffsets   map[string]int64 // inode -> read offset for agent.log tracking
	logInode     uint64
	lastAgentMod time.Time
	turnState    map[string]TurnState // session id -> last turn state
	panes        map[PaneID]PaneStatus
	updated      time.Time
}

// TurnState is the outcome of the last observed turn for a session.
type TurnState struct {
	State   string    `json:"state"` // running / answered / interrupted / none
	Since   time.Time `json:"since"`
	LogLine int64     `json:"log_line"`
	Detail  string    `json:"detail,omitempty"`
}

// NewRegistry builds a Registry.
func NewRegistry(hermesHome string, dbPath string, turnWindow time.Duration, captureMax int) *Registry {
	return &Registry{
		hermesHome: hermesHome,
		dbPath:     dbPath,
		turnWindow: turnWindow,
		captureMax: captureMax,
		logOffsets: map[string]int64{},
		turnState:  map[string]TurnState{},
		panes:      map[PaneID]PaneStatus{},
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

// refreshLogState tails agent.log and updates per-session turn state.
func (r *Registry) refreshLogState() {
	path := filepath.Join(r.hermesHome, "logs", "agent.log")
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.ModTime().Before(r.lastAgentMod) && !r.lastAgentMod.IsZero() {
		return
	}
	r.lastAgentMod = info.ModTime()
	lines, total, err := tailLines(path, 4000)
	if err != nil {
		return
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
	r.logOffsets["agent.log"] = total
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
	leases, err := LoadLeases(r.hermesHome)
	if err != nil {
		log.Printf("scan: leases: %v", err)
	}
	markers, err := LoadMarkers(r.hermesHome)
	if err != nil {
		log.Printf("scan: markers: %v", err)
	}

	// tty -> hermes processes
	ttyProcs := map[string][]PaneAssoc{}
	for _, pr := range procs {
		a := PaneAssoc{PID: pr.pid, Running: !pr.suspended, Suspended: pr.suspended, Cmdline: pr.cmdline}
		if le, ok := leases[pr.pid]; ok && procAlive(pr.pid, le.ProcessStartTime) {
			a.SessionID = le.LiveSessionID
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
			if m, ok := markers[p.TTY]; ok {
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
// scan (proc_linux.go reads /proc, proc_darwin.go shells out to ps).
type hermesProc struct {
	pid       int
	tty       string
	suspended bool
	cmdline   string
}
