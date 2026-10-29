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

// Session ids are hermes' own YYYYMMDD_HHMMSS_hexhex, and the scheduler's
// cron_<jobid>_<ts> for a scheduled run. The cron form is accepted everywhere
// a session id is matched (log tail, db lookups) because a cron session is a
// real session: refusing its id is what kept every autonomous run off the rail.
// Two shapes: the CLI/gateway one (YYYYMMDD_HHMMSS_hexhex) and the scheduler's
// cron_<jobid>_YYYYMMDD_HHMMSS, which carries NO trailing hex segment — an
// optional-prefix regex silently dropped every cron id.
var sessionIDRe = regexp.MustCompile(`\b(?:cron_[0-9a-f]{6,}_\d{8}_[0-9]{6}|\d{8}_[0-9]{6}_[0-9a-f]{6,})\b`)

// The launcher's -c snippet names the tree as a quoted string with no trailing
// slash ('/Users/x/.hermes/hermes-agent'), while an installed hermes runs a path
// inside it (…/hermes-agent/venv/bin/python). Both forms are hermes.
var hermesProcRe = regexp.MustCompile(`\.hermes/hermes-agent(/|'|"|\s|$)`)

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

// Run polls until ctx is done. The first scan is followed by a startup dump:
// the raw evidence behind that scan lands in the log, so an "unattached" pane
// can be traced to its source without re-deriving it by hand.
func (r *Registry) Run(ctx context.Context, every time.Duration) {
	first := true
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.scanOnce()
		if first {
			first = false
			r.dumpStartup()
		}
		r.refreshLogState()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// dumpStartup logs the evidence behind the first scan: which tmux socket and
// panes the daemon saw, what the proc scan found, which leases and markers
// hermes left (markers with their skip reasons), which rhermes shims exist, and
// how the scan joined all of it into pane statuses. A pane that reads as
// unattached shows here next to the raw line that put it there.
func (r *Registry) dumpStartup() {
	// tmux side: the socket the daemon's tmux commands follow
	if sock := os.Getenv("TMUX"); sock != "" {
		log.Printf("startup: daemon TMUX=%s (tmux commands follow this socket, not the default)", sock)
	} else {
		log.Printf("startup: daemon TMUX empty (tmux commands follow the default server socket)")
	}
	if out, err := runTmux("list-sessions", "-F", "#{session_name}|#{session_attached}"); err != nil {
		log.Printf("startup: tmux list-sessions: %v", err)
	} else {
		log.Printf("startup: tmux list-sessions: %s", strings.TrimSpace(out))
	}
	raw, err := runTmux("list-panes", "-a", "-F", paneFormat)
	if err != nil {
		log.Printf("startup: tmux list-panes: %v", err)
	} else {
		lines := []string{}
		for _, ln := range strings.Split(raw, "\n") {
			if ln != "" {
				lines = append(lines, ln)
			}
		}
		log.Printf("startup: tmux list-panes: %d panes", len(lines))
		for _, ln := range lines {
			log.Printf("startup: tmux pane: %s", ln)
		}
	}

	// proc side: every hermes process, with the tty it sits on
	procs := scanHermesProcs()
	log.Printf("startup: hermes procs: %d", len(procs))
	for _, pr := range procs {
		log.Printf("startup: hermes proc pid=%d tty=%s suspended=%v profile=%q cmd=%q", pr.pid, pr.tty, pr.suspended, pr.profile, clip(pr.cmdline, 120))
	}

	// hermes side: leases and markers for every profile the procs name
	profiles := map[string]bool{"": true}
	for _, pr := range procs {
		profiles[pr.profile] = true
	}
	// The same tty->start map the scan uses, rebuilt here so the dump reports the
	// markers the scan will actually believe.
	startedOn := map[string]time.Time{}
	leasesByPID := map[int]LeaseEntry{}
	for p := range profiles {
		if lf, err := LoadLeases(r.profileHome(p)); err == nil {
			for pid, le := range lf {
				leasesByPID[pid] = le
			}
		}
	}
	for _, pr := range procs {
		if pr.tty == "" {
			continue
		}
		if le, ok := leasesByPID[pr.pid]; ok && le.ProcessStartTime > 0 {
			startedOn[pr.tty] = time.Unix(int64(le.ProcessStartTime), 0)
			continue
		}
		if st, err := procStartTime(pr.pid); err == nil && st > 0 {
			startedOn[pr.tty] = time.Unix(int64(st), 0)
		}
	}
	markerOwner := func(tty string) (time.Time, bool) {
		t, ok := startedOn[tty]
		return t, ok
	}
	for p := range profiles {
		home := r.profileHome(p)
		if leases, err := LoadLeases(home); err != nil {
			log.Printf("startup: leases profile=%q home=%s: %v", p, home, err)
		} else {
			for pid, le := range leases {
				log.Printf("startup: lease profile=%q pid=%d session=%s live=%s", p, pid, le.SessionID, le.LiveSessionID)
			}
		}
		if markers, skipped, err := LoadMarkersDetail(home, markerOwner); err != nil {
			log.Printf("startup: markers profile=%q home=%s: %v", p, home, err)
		} else {
			for tty, m := range markers {
				log.Printf("startup: marker profile=%q tty=%s session=%s ts=%s", p, tty, m.SessionID, m.TS.Format(time.RFC3339))
			}
			for _, s := range skipped {
				log.Printf("startup: marker skip profile=%q %s", p, s)
			}
		}
	}

	// rhermes shims: a TUI session attaches through one, never through a lease
	if insts := discoverRhermes(); len(insts) == 0 {
		log.Printf("startup: rhermes instances: none")
	} else {
		for _, inst := range insts {
			log.Printf("startup: rhermes instance pid=%d socket=%s", inst.PID, inst.Path)
		}
	}

	// the join: what the first scan concluded for every pane
	r.mu.RLock()
	attached := 0
	log.Printf("startup: panes after first scan: %d", len(r.panes))
	for id, ps := range r.panes {
		if ps.HasHermes {
			attached++
		}
		log.Printf("startup: pane %s tty=%s status=%s source=%s session=%s hermes=%v assocs=%d", id, ps.Pane.TTY, ps.Status, ps.Source, ps.SessionID, ps.HasHermes, len(ps.Assocs))
	}
	r.mu.RUnlock()
	log.Printf("startup: summary: panes=%d hermes-attached=%d", len(r.panes), attached)
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
	for p := range profiles {
		home := r.profileHome(p)
		if lf, err := LoadLeases(home); err != nil {
			log.Printf("scan: leases %s: %v", home, err)
		} else {
			for pid, le := range lf {
				leases[pid] = le
			}
		}
	}

	// Which hermes started on which tty, so a marker older than the age bound can
	// still be believed when the process that wrote it is still in the pane. The
	// lease's process_start_time is the kernel's start stamp and is exact;
	// procStartTime is the fallback for a process with no lease entry.
	startedOn := map[string]time.Time{}
	for _, pr := range procs {
		if pr.tty == "" {
			continue
		}
		if le, ok := leases[pr.pid]; ok && le.ProcessStartTime > 0 {
			startedOn[pr.tty] = time.Unix(int64(le.ProcessStartTime), 0)
			continue
		}
		if st, err := procStartTime(pr.pid); err == nil && st > 0 {
			startedOn[pr.tty] = time.Unix(int64(st), 0)
		}
	}
	markerOwner := func(tty string) (time.Time, bool) {
		t, ok := startedOn[tty]
		return t, ok
	}

	markers := map[string]MarkerEntry{} // "<profile>|<tty>" -> marker
	for p := range profiles {
		home := r.profileHome(p)
		if mm, err := LoadMarkers(home, markerOwner); err != nil {
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

// dbForSession finds the state.db that holds a session row, without a pane:
// the profile db of every known profile is probed in turn. It is the fallback
// for routes that serve a session's history rather than its pane — the tree
// routes — because a subagent's parent is usually a session that has already
// ended and therefore sits on no pane at all. "" when nowhere.
func (r *Registry) dbForSession(sid string) string {
	lit := idLiteral(sid)
	if lit == "" {
		return ""
	}
	probe := func(db string) bool {
		rows, err := sqliteJSON(db, "SELECT count(*) AS n FROM sessions WHERE id = "+lit, nil)
		return err == nil && len(rows) > 0 && asFloat(rows[0]["n"]) > 0
	}
	profiles := []string{""}
	if entries, err := os.ReadDir(filepath.Join(r.hermesHome, "profiles")); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				profiles = append(profiles, e.Name())
			}
		}
	}
	for _, p := range profiles {
		db := r.dbFor(p)
		if db == "" {
			continue
		}
		if _, err := os.Stat(db); err != nil {
			continue
		}
		if probe(db) {
			return db
		}
	}
	return ""
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
