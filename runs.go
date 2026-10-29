package main

// cron-run rows: sessions hermes created on its own (a scheduled job, or a
// resumed job run) that no pane is currently displaying. They are read from
// state.db, never from tmux, because a finished cron run lives nowhere else.
//
// The console's rules ride on three facts this file exposes per row:
//
//	ended_at NULL + pane attached -> background running (ack forbidden)
//	ended_at NULL + no pane       -> running inside the gateway (uninterruptible)
//	ended_at set  + readable text -> the run's output is ready to read
//
// "Readable" excludes [SILENT]: hermes writes a run-output file for EVERY run,
// including the silent ones, so "a file exists" alone would turn every watchdog
// run into a notification.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RunStatus is one cron/background session row, as the console needs it.
type RunStatus struct {
	SessionID  string      `json:"session_id"`
	JobID      string      `json:"job_id"`
	Profile    string      `json:"profile"`
	Session    *SessionRow `json:"session,omitempty"`
	LastQuery  *MsgRow     `json:"last_query,omitempty"`
	LastAnswer *MsgRow     `json:"last_answer,omitempty"`
	Steps      int         `json:"steps_since_query,omitempty"`
	Turn       *TurnState  `json:"turn,omitempty"`
	// Ended is the raw state.db fact: finished (true) or still running (false).
	Ended bool `json:"ended"`
	// PaneAttached says a tmux pane currently displays this session, so the
	// pane row already carries it and this row is redundant.
	PaneAttached bool `json:"pane_attached"`
	// OutputReady is ended && the last answer is real text (not empty, not
	// [SILENT]). Only such a row may be acknowledged.
	OutputReady bool `json:"output_ready"`
	// OutputPath is the run's own .md under cron/output/<job>/, chosen by
	// nearest mtime to the session's end stamp (the writer stamps the file a
	// few milliseconds after ended_at). Empty when no file matched.
	OutputPath string `json:"output_path,omitempty"`
	// StartedAt is the session's start (unix seconds), for ordering.
	StartedAt float64 `json:"started_at"`
}

// cronJobIDOf splits a cron session id into its job id: cron_<jobid>_<date>_<time>.
func cronJobIDOf(sid string) string {
	rest, ok := strings.CutPrefix(sid, "cron_")
	if !ok {
		return ""
	}
	// The job id is the segment before the YYYYMMDD date part.
	parts := strings.Split(rest, "_")
	if len(parts) < 3 {
		return ""
	}
	return parts[0]
}

// silentResponse is hermes' "nothing to report" sentinel, written verbatim as
// the assistant's whole answer.
func silentResponse(text string) bool {
	return strings.TrimSpace(text) == "[SILENT]"
}

// profileDirs lists every hermes home a state.db can live in: the default home
// first, then every profile subdirectory.
func (r *Registry) profileDirs() []struct{ Name, Home string } {
	out := []struct{ Name, Home string }{{"", r.hermesHome}}
	entries, err := os.ReadDir(filepath.Join(r.hermesHome, "profiles"))
	if err != nil {
		return out
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		out = append(out, struct{ Name, Home string }{n, filepath.Join(r.hermesHome, "profiles", n)})
	}
	return out
}

// LoadCronSessions reads recent autonomous sessions (source='cron') from one
// state.db, newest first.
func LoadCronSessions(dbPath string, limit int) ([]RunStatus, error) {
	q := `SELECT id AS id, coalesce(title,'') AS title, coalesce(cwd,'') AS cwd,
	  (ended_at IS NOT NULL AND ended_at != 0) AS ended,
	  coalesce(strftime('%Y-%m-%dT%H:%M:%SZ', ended_at, 'unixepoch'),'') AS ended_ts,
	  coalesce(started_at, 0) AS started_at
	  FROM sessions WHERE source = 'cron'
	  ORDER BY started_at DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	out := make([]RunStatus, 0, len(rows))
	for _, r := range rows {
		sid := asString(r["id"])
		if sid == "" {
			continue
		}
		out = append(out, RunStatus{
			SessionID: sid,
			JobID:     cronJobIDOf(sid),
			StartedAt: asFloat(r["started_at"]),
			Ended:     asBool(r["ended"]),
			Session: &SessionRow{
				ID:      sid,
				Title:   asString(r["title"]),
				CWD:     asString(r["cwd"]),
				Ended:   asBool(r["ended"]),
				EndedAt: asString(r["ended_ts"]),
			},
		})
	}
	return out, nil
}

// cronOutputFile finds the run-output file for a run: the newest *.md inside
// <home>/cron/output/<job>/, picked by nearest mtime to the run's end stamp.
// A run that has not ended has no usable file, and one that never wrote a file
// (a crash before delivery) answers "".
func cronOutputFile(home, jobID string, endedTS string) string {
	if jobID == "" {
		return ""
	}
	dir := filepath.Join(home, "cron", "output", jobID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var want float64
	if endedTS != "" {
		if t, err := time.Parse(time.RFC3339, endedTS); err == nil {
			want = float64(t.UnixNano()) / 1e9
		}
	}
	best := ""
	bestDelta := -1.0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := float64(info.ModTime().UnixNano()) / 1e9
		delta := 0.0
		if want > 0 {
			delta = mt - want
			if delta < 0 {
				delta = -delta
			}
		}
		if best == "" || delta < bestDelta {
			best, bestDelta = filepath.Join(dir, e.Name()), delta
		}
	}
	return best
}

// cronRuns assembles the run rows for every profile: state.db rows joined with
// their last exchange, their output file and whether a pane already shows them.
func (r *Registry) cronRuns(limit int) []RunStatus {
	r.mu.RLock()
	attached := map[string]bool{}
	for _, ps := range r.panes {
		if ps.SessionID != "" {
			attached[ps.SessionID] = true
		}
	}
	turns := make(map[string]TurnState, len(r.turnState))
	for k, v := range r.turnState {
		turns[k] = v
	}
	r.mu.RUnlock()

	var out []RunStatus
	for _, p := range r.profileDirs() {
		db := filepath.Join(p.Home, "state.db")
		if _, err := os.Stat(db); err != nil {
			continue
		}
		runs, err := LoadCronSessions(db, limit)
		if err != nil {
			continue // a locked/absent db must not blank the rail
		}
		ids := make([]string, 0, len(runs))
		for _, run := range runs {
			ids = append(ids, run.SessionID)
		}
		exch, _ := LoadLastMessages(db, ids)
		for i := range runs {
			run := &runs[i]
			run.Profile = p.Name
			if le, ok := exch[run.SessionID]; ok {
				run.LastQuery, run.LastAnswer, run.Steps = le.LastQuery, le.LastAnswer, le.Steps
			}
			if ts, ok := turns[run.SessionID]; ok {
				t := ts
				run.Turn = &t
			}
			run.PaneAttached = attached[run.SessionID]
			if run.Ended && run.LastAnswer != nil {
				body := strings.TrimSpace(run.LastAnswer.Content)
				run.OutputReady = body != "" && !silentResponse(body)
			}
			if run.Ended && run.Session != nil {
				run.OutputPath = cronOutputFile(p.Home, run.JobID, run.Session.EndedAt)
			}
		}
		out = append(out, runs...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---------------------------------------------------------------- bg tmux jobs
//
// A resumed cron session runs in its own window of a dedicated tmux session, so
// the work survives a tmux-control restart (the tmux server is a different
// process), can be introspected like any pane, and is killed by killing its
// window. Nothing about it is stored in the daemon.

const bgSessionName = "hermes-bg"

// bgPaneFor finds the pane currently running a resumed copy of sid.
func bgPaneFor(sid string) string {
	raw, err := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{"+"@hermes_bg"+"}")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(raw, "\n") {
		id, val, ok := strings.Cut(line, "|")
		if ok && strings.TrimSpace(val) == sid {
			return id
		}
	}
	return ""
}

// bgPanes lists every live background pane: session id -> pane id.
func bgPanes() map[string]string {
	out := map[string]string{}
	raw, err := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{"+"@hermes_bg"+"}")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(raw, "\n") {
		id, val, ok := strings.Cut(line, "|")
		if ok && strings.TrimSpace(val) != "" {
			out[strings.TrimSpace(val)] = id
		}
	}
	return out
}

// hermesBinary finds the launcher. The install tree's own `hermes` comes FIRST:
// on a box where hermes is not on PATH (a login shell only) the bare name fails,
// and a PATH hit could belong to a different install than the one whose
// state.db the daemon reads.
func hermesBinary() string {
	if home, err := os.UserHomeDir(); err == nil {
		tree := filepath.Join(home, ".hermes", "hermes-agent", "hermes")
		if _, err := os.Stat(tree); err == nil {
			return tree
		}
	}
	if p, err := exec.LookPath("hermes"); err == nil {
		return p
	}
	return "hermes"
}

// startBackgroundRun launches `hermes chat --resume <sid> --yolo --query-file`
// in its own tmux window and returns the pane id. The query travels through a
// file, never through argv: an answer may be arbitrarily long and contain shell
// metacharacters.
func (r *Registry) startBackgroundRun(sid, text, home, workdir string) (string, error) {
	if cur := bgPaneFor(sid); cur != "" {
		return "", fmt.Errorf("session %s is already running in pane %s", sid, cur)
	}
	if _, _, err := r.findPaneBySession(sid); err == nil {
		return "", fmt.Errorf("session %s is already displayed in a pane", sid)
	}
	qdir := filepath.Join(home, "bg-runs")
	if err := os.MkdirAll(qdir, 0o700); err != nil {
		return "", err
	}
	qfile := filepath.Join(qdir, sid+"."+strconv.FormatInt(time.Now().UnixNano(), 36)+".txt")
	if err := os.WriteFile(qfile, []byte(text), 0o600); err != nil {
		return "", err
	}
	if workdir == "" || !filepath.IsAbs(workdir) {
		if wd, err := os.UserHomeDir(); err == nil {
			workdir = wd
		} else {
			workdir = "/"
		}
	}
	if _, err := os.Stat(workdir); err != nil {
		workdir = "/"
	}
	// HERMES_HOME is the profile's own home, which is how the CLI selects a
	// profile: adding -p on top of it would name the profile twice.
	cmdline := fmt.Sprintf("exec env HERMES_HOME=%s %s chat --resume %s --yolo --query-file %s",
		shellQuote(home), shellQuote(hermesBinary()), shellQuote(sid), shellQuote(qfile))

	args := []string{"new-window", "-P", "-F", "#{pane_id}", "-d", "-n", bgWindowName(sid), "-c", workdir}
	if _, err := runTmux("has-session", "-t", bgSessionName); err != nil {
		args = append([]string{"new-session", "-P", "-F", "#{pane_id}", "-d", "-s", bgSessionName,
			"-n", bgWindowName(sid), "-c", workdir}, cmdline)
	} else {
		args = append(args, "-t", bgSessionName, cmdline)
	}
	out, err := runTmux(args...)
	if err != nil {
		os.Remove(qfile)
		return "", err
	}
	pane := strings.TrimSpace(out)
	if _, err := runTmux("set-option", "-p", "-t", pane, "@hermes_bg", sid); err != nil {
		return pane, err
	}
	return pane, nil
}

func bgWindowName(sid string) string {
	if len(sid) > 24 {
		return sid[:24]
	}
	return sid
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------------------------------------------------------- http routes

// handleRunList serves GET /runs: every recent autonomous session with the
// console's verdict fields.
func (r *Registry) handleRunList(w http.ResponseWriter, req *http.Request) {
	limit := 100
	if v := req.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}
	runs := r.cronRuns(limit)
	bg := bgPanes()
	for i := range runs {
		if p, ok := bg[runs[i].SessionID]; ok {
			runs[i].PaneAttached = true
			_ = p
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(runs), "runs": runs})
}

// runSession resolves a session id to (profile, profile home, db, session row).
// It answers for pane-less sessions too, which is the whole point: a finished
// cron run has no pane.
func (r *Registry) runSession(sid string) (string, string, string, *SessionRow, error) {
	if idLiteral(sid) == "" {
		return "", "", "", nil, fmt.Errorf("malformed session id")
	}
	for _, p := range r.profileDirs() {
		db := filepath.Join(p.Home, "state.db")
		if _, err := os.Stat(db); err != nil {
			continue
		}
		rows, err := sqliteJSON(db, "SELECT id AS id, coalesce(title,'') AS title, coalesce(cwd,'') AS cwd,"+
			" (ended_at IS NOT NULL AND ended_at != 0) AS ended,"+
			" coalesce(strftime('%Y-%m-%dT%H:%M:%SZ', ended_at, 'unixepoch'),'') AS ended_ts"+
			" FROM sessions WHERE id = "+idLiteral(sid), nil)
		if err != nil || len(rows) == 0 {
			continue
		}
		row := rows[0]
		return p.Name, p.Home, db, &SessionRow{
			ID:      asString(row["id"]),
			Title:   asString(row["title"]),
			CWD:     asString(row["cwd"]),
			Ended:   asBool(row["ended"]),
			EndedAt: asString(row["ended_ts"]),
		}, nil
	}
	return "", "", "", nil, fmt.Errorf("session %s not found in any profile database", sid)
}

// handleResume serves POST /sessions/{id}/resume {text}: the human's answer
// becomes the next turn of a session that is not running anywhere, launched as
// a background tmux window.
func (r *Registry) handleResume(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	profile, home, _, row, err := r.runSession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if _, _, perr := r.findPaneBySession(sid); perr == nil {
		writeErr(w, http.StatusConflict, "session is already displayed in a pane; answer it there")
		return
	}
	pane, err := r.startBackgroundRun(sid, text, home, row.CWD)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "action": "resume", "session_id": sid, "pane_id": pane,
		"tmux_session": bgSessionName, "profile": profile,
	})
}

// handleKill serves POST /sessions/{id}/kill: kill the background window that
// runs this session. A session with no background pane is a 404 — killing is
// never a synonym for interrupt (that is /break, which needs a pane too).
func (r *Registry) handleKill(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	pane := bgPaneFor(sid)
	if pane == "" {
		writeErr(w, http.StatusNotFound, "no background run for session "+sid)
		return
	}
	if _, err := runTmux("kill-window", "-t", pane); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "kill", "session_id": sid, "pane_id": pane})
}

// handleRunOutput serves GET /sessions/{id}/output: the run's own .md file.
func (r *Registry) handleRunOutput(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	_, home, _, row, err := r.runSession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	path := cronOutputFile(home, cronJobIDOf(sid), row.EndedAt)
	if path == "" {
		writeErr(w, http.StatusNotFound, "no run-output file for session "+sid)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	text := string(data)
	if len(text) > 512*1024 {
		text = clip(text, 512*1024)
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "path": path, "text": text})
}

// handleDismiss serves POST /sessions/{id}/dismiss: retire the run's own
// notification by moving its .md into <job>/acked/, and report what moved.
// The session row and its transcript stay in state.db — dismissing clears the
// notification, it does not delete history. Idempotent: a run already moved
// answers ok with moved:false.
func (r *Registry) handleDismiss(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	_, home, _, row, err := r.runSession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	path := cronOutputFile(home, cronJobIDOf(sid), row.EndedAt)
	if path == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "session_id": sid, "moved": false})
		return
	}
	dest := filepath.Join(filepath.Dir(path), "acked", filepath.Base(path))
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := os.Rename(path, dest); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A resumed run's REPL sits at its prompt long after the work is done, and
	// that idle pane is the only thing still holding the row in the rail —
	// dismissing must retire it for good, so the window goes with the file.
	// Never while a turn is in flight: that would be a kill disguised as an ack.
	killed := false
	if pane := bgPaneFor(sid); pane != "" {
		r.mu.RLock()
		ts := r.turnState[sid]
		r.mu.RUnlock()
		if ts.State != "running" {
			if _, err := runTmux("kill-window", "-t", pane); err == nil {
				killed = true
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "session_id": sid, "moved": true, "from": path, "to": dest, "killed": killed})
}
