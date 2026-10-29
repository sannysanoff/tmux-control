package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// gzipWriter wraps a ResponseWriter so a handler's writes are compressed.
type gzipWriter struct {
	http.ResponseWriter
	w *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.w.Write(b) }

// gzipIfAccepted compresses a response when the caller asks for it. Nothing below
// this layer compresses: the relay carries ciphertext and its transport never
// negotiates encoding, so a history page is only ever squeezed here.
func gzipIfAccepted(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
			next(w, req)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next(gzipWriter{ResponseWriter: w, w: gz}, req)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// registerRoutes builds the mux.
func (r *Registry) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /sessions", r.handleListSessions)
	// Autonomous (cron) runs: sessions no pane displays. /runs lists them,
	// /sessions/{id}/resume answers one as a background tmux window, and
	// /sessions/{id}/kill ends that window.
	m.HandleFunc("GET /runs", r.handleRunList)
	m.HandleFunc("GET /sessions/{id}/output", r.handleRunOutput)
	m.HandleFunc("POST /sessions/{id}/resume", r.handleResume)
	m.HandleFunc("POST /sessions/{id}/kill", r.handleKill)
	m.HandleFunc("POST /sessions/{id}/dismiss", r.handleDismiss)
	m.HandleFunc("GET /sessions/{id}", r.handleGetSession)
	m.HandleFunc("GET /sessions/{id}/pane", r.handlePane)
	m.HandleFunc("GET /sessions/{id}/messages", r.handleMessages)
	m.HandleFunc("GET /sessions/{id}/events", gzipIfAccepted(r.handleEvents))
	m.HandleFunc("GET /sessions/{id}/tree", r.handleTree)
	m.HandleFunc("GET /sessions/{id}/child-events", gzipIfAccepted(r.handleChildEvents))
	m.HandleFunc("GET /sessions/{id}/exec", gzipIfAccepted(r.handleExec))
	m.HandleFunc("POST /sessions/{id}/fs/stat", r.handleFSStat)
	m.HandleFunc("GET /sessions/{id}/fs/list", r.handleFSList)
	m.HandleFunc("GET /sessions/{id}/fs/file", r.handleFSFile)
	m.HandleFunc("GET /sessions/{id}/fs/raw", r.handleFSRaw)
	m.HandleFunc("POST /sessions/{id}/break", r.handleBreak)
	m.HandleFunc("POST /sessions/{id}/send", r.handleSend)
	m.HandleFunc("POST /sessions/{id}/paste", r.handlePaste)
	m.HandleFunc("POST /sessions/{id}/keys", r.handleKeys)
	m.HandleFunc("GET /sessions/{id}/rhermes/status", r.handleRhermesStatus)
	m.HandleFunc("GET /sessions/{id}/rhermes/frames", r.handleRhermesFrames)
	m.HandleFunc("POST /sessions/{id}/rhermes/prompt", r.handleRhermesPrompt)
	m.HandleFunc("POST /sessions/{id}/rhermes/send", r.handleRhermesSend)
	m.HandleFunc("POST /sessions/{id}/rhermes/stop", r.handleRhermesStop)
	m.HandleFunc("GET /rhermes", r.handleRhermesList)
	m.HandleFunc("GET /sessions/pane/{pid}/pane", r.handlePaneByID)
	m.HandleFunc("POST /sessions/pane/{pid}/keys", r.handlePaneByIDKeys)
	m.HandleFunc("GET /health", r.handleHealth)
	return m
}

func (r *Registry) handleHealth(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"version": version,
		"now":     time.Now().UTC().Format(time.RFC3339),
	})
}

// findPaneBySession resolves a session id to a pane.
func (r *Registry) findPaneBySession(sid string) (PaneID, *PaneStatus, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var hits []PaneID
	for id, ps := range r.panes {
		if ps.SessionID == sid {
			hits = append(hits, id)
		}
	}
	if len(hits) == 0 {
		return "", nil, fmt.Errorf("session %s not associated with any pane", sid)
	}
	sortPanes(hits)
	pid := hits[0]
	ps := r.panes[pid]
	return pid, &ps, nil
}

func sortPanes(ids []PaneID) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

func (r *Registry) handleListSessions(w http.ResponseWriter, req *http.Request) {
	// content=0 skips pane content entirely: no pane_content fields and no
	// tmux captures at all, so polling stays cheap.
	noContent := req.URL.Query().Get("content") == "0"

	r.mu.RLock()
	panes := make([]PaneStatus, 0, len(r.panes))
	for _, ps := range r.panes {
		panes = append(panes, ps)
	}
	r.mu.RUnlock()

	// enrich with session rows + last messages, from the database that holds them:
	// every profile keeps its own state.db, so ids are grouped by database.
	byDB := map[string][]string{}
	for _, ps := range panes {
		if ps.SessionID == "" {
			continue
		}
		db := r.dbFor(ps.Profile)
		byDB[db] = append(byDB[db], ps.SessionID)
	}
	sessions := map[string]SessionRow{}
	msgs := map[string]LastExchange{}
	for db, ids := range byDB {
		s, err := LoadSessions(db, ids)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for k, v := range s {
			sessions[k] = v
		}
		if m, err := LoadLastMessages(db, ids); err == nil {
			for k, v := range m {
				msgs[k] = v
			}
		}
	}
	r.mu.RLock()
	for i := range panes {
		ps := &panes[i]
		if ps.SessionID != "" {
			if s, ok := sessions[ps.SessionID]; ok {
				ps.Session = &s
			}
			if le, ok := msgs[ps.SessionID]; ok {
				ps.LastQuery = le.LastQuery
				ps.LastAnswer = le.LastAnswer
				ps.Steps = le.Steps
			}
			if ts, ok := r.turnState[ps.SessionID]; ok {
				tt := ts
				ps.Turn = &tt
				ps.ExchangeState = tt.State
			}
		}
	}
	r.mu.RUnlock()
	// pane content for hermes panes (on demand, sequential tmux captures)
	if !noContent {
		for i := range panes {
			if panes[i].HasHermes {
				if lines, err := capturePane(panes[i].Pane.ID, r.captureMax, false); err == nil {
					panes[i].PaneLines = lines
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": r.updated, "panes": panes})
}

func (r *Registry) handleGetSession(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	_, ps, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	db := r.dbFor(ps.Profile)
	if s, err2 := LoadSessions(db, []string{sid}); err2 == nil {
		if srow, ok := s[sid]; ok {
			ps.Session = &srow
		}
	}
	if le, err2 := LoadLastMessages(db, []string{sid}); err2 == nil {
		if x, ok := le[sid]; ok {
			ps.LastQuery = x.LastQuery
			ps.LastAnswer = x.LastAnswer
		}
	}
	r.mu.RLock()
	ts := r.turnState[sid]
	r.mu.RUnlock()
	ps.Turn = &ts
	writeJSON(w, http.StatusOK, ps)
}

func (r *Registry) handlePane(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	pid, _, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// color=1 answers with the capture carrying the pane's SGR attributes
	// (`capture-pane -e`), so a console can paint the screen in color; any
	// other value keeps the plain capture byte-exact as before. A client that
	// strips the codes back out (ESC + "[...m", then trim trailing spaces per
	// line) recovers the plain lines exactly. The cursor position rides with
	// both answers so the console can draw the block cursor; the capture is
	// still one per request, the cursor is one cheap display-message.
	cx, cy, ph := paneCursor(pid)
	if req.URL.Query().Get("color") == "1" {
		clines, err := capturePane(pid, r.captureMax, true)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "pane_id": string(pid), "lines": clines, "color": true, "cursor_x": cx, "cursor_y": cy, "pane_height": ph})
		return
	}
	lines, err := capturePane(pid, r.captureMax, false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "pane_id": string(pid), "lines": lines, "cursor_x": cx, "cursor_y": cy, "pane_height": ph})
}

// handlePaneByID serves GET /sessions/pane/{pid}/pane: the live screen of a
// pane addressed BY PANE ID, for a pane whose hermes has ended — the screen
// must keep working when the session is gone (the pane is the stable thing).
// Color rides as on the session route.
func (r *Registry) handlePaneByID(w http.ResponseWriter, req *http.Request) {
	pid := PaneID(req.PathValue("pid"))
	r.mu.RLock()
	_, ok := r.panes[pid]
	r.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no such pane "+string(pid))
		return
	}
	cx, cy, ph := paneCursor(pid)
	if req.URL.Query().Get("color") == "1" {
		clines, err := capturePane(pid, r.captureMax, true)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pane_id": string(pid), "lines": clines, "color": true, "cursor_x": cx, "cursor_y": cy, "pane_height": ph})
		return
	}
	lines, err := capturePane(pid, r.captureMax, false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pane_id": string(pid), "lines": lines, "cursor_x": cx, "cursor_y": cy, "pane_height": ph})
}

// handlePaneByIDKeys serves POST /sessions/pane/{pid}/keys {text?, keys?}: raw
// keystrokes into a pane addressed by pane id — the fg/exit case exactly. Same
// body, same validation, same no-focus-check as the session-keyed /keys.
func (r *Registry) handlePaneByIDKeys(w http.ResponseWriter, req *http.Request) {
	pid := PaneID(req.PathValue("pid"))
	r.mu.RLock()
	_, ok := r.panes[pid]
	r.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no such pane "+string(pid))
		return
	}
	var body struct {
		Text string   `json:"text"`
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Text == "" && len(body.Keys) == 0 {
		writeErr(w, http.StatusBadRequest, "text or keys is required")
		return
	}
	res, err := sendKeys(string(pid), body.Text, body.Keys)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res["pane_id"] = string(pid)
	writeJSON(w, http.StatusOK, res)
}

// handleKeys serves POST /sessions/{id}/keys {text?, keys?}: raw keystrokes
// into the pane, with NO input focus check — a one-key permission dialog is
// exactly the case /send and /paste refuse. rhermes panes included: this verb
// deliberately always means terminal keys, never a socket prompt.
func (r *Registry) handleKeys(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	var body struct {
		Text string   `json:"text"`
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Text == "" && len(body.Keys) == 0 {
		writeErr(w, http.StatusBadRequest, "text or keys is required")
		return
	}
	pid, _, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// No Running check, unlike /send and /paste: the pane lives whether or not
	// a hermes holds it, and a shell prompt at "hermes just ended" is exactly
	// when typing `fg` or `exit` into it is the point. Raw keystrokes cannot
	// disturb a turn that is not there.
	res, err := sendKeys(string(pid), body.Text, body.Keys)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	res["session_id"] = sid
	writeJSON(w, http.StatusOK, res)
}

// handleMessages serves GET /sessions/{id}/messages?limit=N&role=R: recent
// conversation history, chronological (oldest first, newest last). role
// selects user or assistant rows only; the default (all) keeps today's
// user+assistant behaviour.
func (r *Registry) handleMessages(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	// A pane is the fast path (it names the profile directly), but a session
	// that no pane displays — every finished cron run — is still readable: its
	// history is a database view, not a screen.
	db := ""
	if _, ps, err := r.findPaneBySession(sid); err == nil {
		db = r.dbFor(ps.Profile)
	} else {
		db = r.dbForSession(sid)
	}
	if db == "" {
		writeErr(w, http.StatusNotFound, "session "+sid+" not associated with any pane")
		return
	}
	limit := 20
	if req.URL.Query().Has("limit") {
		n, err := strconv.Atoi(req.URL.Query().Get("limit"))
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}
	// before_id pages BACKWARD (rows older than the bound — the reading view
	// crawls to the session's first prompt), since_id pages FORWARD (cheap
	// top-ups between polls). Both are exclusive bounds, mirroring /events'
	// since_id.
	beforeID, sinceID := int64(0), int64(0)
	if v := req.URL.Query().Get("before_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid before_id: must be a positive integer")
			return
		}
		beforeID = n
	}
	if v := req.URL.Query().Get("since_id"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid since_id: must be a non-negative integer")
			return
		}
		sinceID = n
	}
	role, ok := parseRoleFilter(req.URL.Query().Get("role"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid role: must be user, assistant or all")
		return
	}
	msgs, err := LoadMessages(db, sid, role, limit, beforeID, sinceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "count": len(msgs), "messages": msgs})
}

// handleEvents serves GET /sessions/{id}/events?since_id=N&limit=M&max_chars=C:
// the whole session, every role, in id order — the incremental feed the console
// archives locally. since_id is the last id the caller stored (0 = from the
// start); a full page means there is more, so the caller pages until it is short.
func (r *Registry) handleEvents(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	// Same fallback as /messages: the archive of a finished cron run is a
	// database view, and a pane-less session must still export.
	db := ""
	if _, ps, err := r.findPaneBySession(sid); err == nil {
		db = r.dbFor(ps.Profile)
	} else {
		db = r.dbForSession(sid)
	}
	if db == "" {
		writeErr(w, http.StatusNotFound, "session "+sid+" not associated with any pane")
		return
	}
	q := req.URL.Query()

	var sinceID int64
	if q.Has("since_id") {
		n, err := strconv.ParseInt(q.Get("since_id"), 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid since_id: must be a non-negative integer")
			return
		}
		sinceID = n
	}

	limit := 500
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
			return
		}
		if n > 2000 {
			n = 2000
		}
		limit = n
	}

	maxChars := 2000
	if q.Has("max_chars") {
		n, err := strconv.Atoi(q.Get("max_chars"))
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid max_chars: must be a positive integer")
			return
		}
		if n > 200000 {
			n = 200000
		}
		maxChars = n
	}

	events, err := LoadEvents(db, sid, sinceID, limit, maxChars)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	last := sinceID
	if len(events) > 0 {
		last = events[len(events)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"count":      len(events),
		"last_id":    last,
		"has_more":   len(events) == limit,
		"events":     events,
	})
}

// handleTree serves GET /sessions/{id}/tree: the session's subagent children
// and the delegate_task calls that spawned them, read from the session's own
// profile database. A session id that has no row answers 404 — the tree is a
// database view, not a pane view.
func (r *Registry) handleTree(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	// The tree is a database view, not a pane view: the parent is usually a
	// session that has already ended and sits on no pane, so the db is found
	// by probing the profile homes; the pane association, when there is one,
	// still wins (it names the profile directly).
	db := ""
	if _, ps, err := r.findPaneBySession(sid); err == nil {
		db = r.dbFor(ps.Profile)
	}
	if db == "" {
		db = r.dbForSession(sid)
	}
	if db == "" {
		writeErr(w, http.StatusNotFound, "session "+sid+" not found in any profile database")
		return
	}
	tree, err := LoadSessionTree(db, sid)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tree)
}

// handleChildEvents serves GET /sessions/{id}/child-events?child=ID&limit=N&max_chars=C:
// one subagent child's whole event stream, for the console's session-tree tab.
// The daemon gates it: only a real subagent child of {id} is ever served, so
// the route is not a general history export.
func (r *Registry) handleChildEvents(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	child := req.URL.Query().Get("child")
	if child == "" {
		writeErr(w, http.StatusBadRequest, "child is required")
		return
	}
	limit := 2000
	if req.URL.Query().Has("limit") {
		n, err := strconv.Atoi(req.URL.Query().Get("limit"))
		if err != nil || n <= 0 || n > 5000 {
			writeErr(w, http.StatusBadRequest, "invalid limit: 1..5000")
			return
		}
		limit = n
	}
	maxChars := 2000
	if req.URL.Query().Has("max_chars") {
		n, err := strconv.Atoi(req.URL.Query().Get("max_chars"))
		if err != nil || n <= 0 || n > 200000 {
			writeErr(w, http.StatusBadRequest, "invalid max_chars: 1..200000")
			return
		}
		maxChars = n
	}
	db := ""
	if _, ps, err := r.findPaneBySession(sid); err == nil {
		db = r.dbFor(ps.Profile)
	}
	if db == "" {
		db = r.dbForSession(sid)
	}
	if db == "" {
		writeErr(w, http.StatusNotFound, "session "+sid+" not found in any profile database")
		return
	}
	events, err := LoadChildEvents(db, sid, child, limit, maxChars)
	if err != nil {
		if strings.Contains(err.Error(), "is not a subagent") {
			writeErr(w, http.StatusForbidden, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "child_id": child, "count": len(events), "events": events,
	})
}

// handleExec serves GET /sessions/{id}/exec?since_id=&limit=&max_chars= — the
// parent's event stream plus the delegate_task calls in view and the subagent
// sessions each spawned. Same shape of query parameters as /events, so the
// console pages it the same way; the db is the pane's profile when the parent
// is on one, else the profile probe (a finished parent sits on no pane).
func (r *Registry) handleExec(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	db := ""
	if _, ps, err := r.findPaneBySession(sid); err == nil {
		db = r.dbFor(ps.Profile)
	}
	if db == "" {
		db = r.dbForSession(sid)
	}
	if db == "" {
		writeErr(w, http.StatusNotFound, "session "+sid+" not found in any profile database")
		return
	}
	q := req.URL.Query()
	var sinceID int64
	if q.Has("since_id") {
		n, err := strconv.ParseInt(q.Get("since_id"), 10, 64)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid since_id: must be a non-negative integer")
			return
		}
		sinceID = n
	}
	limit := 500
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
			return
		}
		if n > 2000 {
			n = 2000
		}
		limit = n
	}
	maxChars := 2000
	if q.Has("max_chars") {
		n, err := strconv.Atoi(q.Get("max_chars"))
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid max_chars: must be a positive integer")
			return
		}
		if n > 200000 {
			n = 200000
		}
		maxChars = n
	}
	events, names, err := LoadSessionExec(db, sid, sinceID, limit, maxChars)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	last := sinceID
	if len(events) > 0 {
		last = events[len(events)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid,
		"count":      len(events),
		"last_id":    last,
		"has_more":   len(events) == limit,
		"events":     events,
		"calls":      names.Calls,
		"kids":       names.Kids,
	})
}

func (r *Registry) handleBreak(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	pid, ps, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if ps == nil || !ps.Running {
		writeErr(w, http.StatusConflict, "session is not running in a pane")
		return
	}
	if _, err := runTmux("send-keys", "-t", string(pid), "C-c"); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "action": "break", "pane_id": string(pid), "session_id": sid})
}

func (r *Registry) handleSend(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	var body struct {
		Text  string `json:"text"`
		Enter *bool  `json:"enter"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	enter := body.Enter == nil || *body.Enter
	pid, ps, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if ps == nil || !ps.Running {
		writeErr(w, http.StatusConflict, "session is not running in a pane")
		return
	}
	if r.useSocket(ps) {
		r.rhermesPromptThroughSend(w, sid, ps, body.Text)
		return
	}
	res, err := sendText(string(pid), body.Text, enter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// rhermesPromptThroughSend answers a keystrokes-shaped request with the socket
// transport: the text becomes a prompt.submit, and the reply carries "via":
// "rhermes" so a caller can see which way it went. An error is 502, not 500 —
// the pane is fine; the shim refused.
func (r *Registry) rhermesPromptThroughSend(w http.ResponseWriter, sid string, ps *PaneStatus, text string) {
	res, err := (rhermesClient{path: ps.RhermesPath}).prompt(text, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "pid": ps.RhermesPID, "socket": ps.RhermesPath,
		"via": "rhermes", "result": res,
	})
}

func (r *Registry) handlePaste(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	var body struct {
		Text  string `json:"text"`
		Enter *bool  `json:"enter"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	enter := body.Enter == nil || *body.Enter
	pid, ps, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if ps == nil || !ps.Running {
		writeErr(w, http.StatusConflict, "session is not running in a pane")
		return
	}
	if r.useSocket(ps) {
		r.rhermesPromptThroughSend(w, sid, ps, body.Text)
		return
	}
	res, err := pasteText(string(pid), body.Text, enter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// rhermesOf resolves a session id to the pane's rhermes shim: its socket path,
// pid and the pane it sits in. A session without a shim answers "no rhermes".
func (r *Registry) rhermesOf(sid string) (PaneID, *PaneStatus, string, int, error) {
	pid, ps, err := r.findPaneBySession(sid)
	if err != nil {
		return "", nil, "", 0, err
	}
	if ps == nil || ps.RhermesPath == "" {
		return "", nil, "", 0, fmt.Errorf("session %s has no rhermes shim on its pane", sid)
	}
	return pid, ps, ps.RhermesPath, ps.RhermesPID, nil
}

// useSocket decides the access mode for one pane. "auto" means the socket when
// the pane actually has a shim — the default, so nothing changes for panes
// that run the TUI bare.
func (r *Registry) useSocket(ps *PaneStatus) bool {
	switch r.rhermesAccess {
	case "socket":
		return ps != nil && ps.RhermesPath != ""
	case "auto":
		return ps != nil && ps.RhermesPath != ""
	default: // keystrokes
		return false
	}
}

// handleRhermesList serves GET /rhermes: every live shim, followed or not.
func (r *Registry) handleRhermesList(w http.ResponseWriter, req *http.Request) {
	insts := discoverRhermes()
	out := make([]map[string]any, 0, len(insts))
	for _, inst := range insts {
		m := map[string]any{"pid": inst.PID, "socket": inst.Path}
		if st, err := (rhermesClient{path: inst.Path}).status(); err == nil {
			m["status"] = st
		} else {
			m["note"] = err.Error()
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": r.updated, "instances": out})
}

// handleRhermesStatus serves GET /sessions/{id}/rhermes/status.
func (r *Registry) handleRhermesStatus(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	_, _, path, rpid, err := r.rhermesOf(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	st, err := (rhermesClient{path: path}).status()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	st.Followed = followerFor(path) != nil
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "pid": rpid, "socket": path, "status": st,
	})
}

// handleRhermesFrames serves GET /sessions/{id}/rhermes/frames?limit=N: the
// non-blocking pull. Every frame the follower saw since the last drain, up to
// limit (default all), oldest first; the buffer empties as it serves.
func (r *Registry) handleRhermesFrames(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	_, _, path, rpid, err := r.rhermesOf(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	limit := 0
	if req.URL.Query().Has("limit") {
		n, err := strconv.Atoi(req.URL.Query().Get("limit"))
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit: must be a non-negative integer")
			return
		}
		limit = n
	}
	f := followerFor(path)
	if f == nil {
		writeErr(w, http.StatusBadGateway, "no follower for "+path)
		return
	}
	frames, dropped := f.drain(limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "pid": rpid, "socket": path,
		"count": len(frames), "dropped": dropped, "frames": frames,
	})
}

// handleRhermesPrompt serves POST /sessions/{id}/rhermes/prompt {text}: inject
// a user turn through the shim. It waits for the shim's streaming ack so the
// caller learns the turn started; the answer itself arrives as frames.
func (r *Registry) handleRhermesPrompt(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	_, _, path, rpid, err := r.rhermesOf(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	res, err := (rhermesClient{path: path}).prompt(body.Text, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "pid": rpid, "socket": path, "via": "rhermes", "result": res,
	})
}

// handleRhermesSend serves POST /sessions/{id}/rhermes/send {method, params}:
// an arbitrary JSON-RPC call through the shim.
func (r *Registry) handleRhermesSend(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	var body struct {
		Method string `json:"method"`
		Params any    `json:"params"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json: "+err.Error())
		return
	}
	if body.Method == "" {
		writeErr(w, http.StatusBadRequest, "method is required")
		return
	}
	_, _, path, rpid, err := r.rhermesOf(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	res, err := (rhermesClient{path: path}).send(body.Method, body.Params, 30*time.Second)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "pid": rpid, "socket": path, "via": "rhermes", "result": res,
	})
}

// handleRhermesStop serves POST /sessions/{id}/rhermes/stop: tear the whole
// rhermes instance down (TUI included). A destructive act, so it says so in the
// reply.
func (r *Registry) handleRhermesStop(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	_, _, path, rpid, err := r.rhermesOf(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err := (rhermesClient{path: path}).stop(); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": sid, "pid": rpid, "socket": path, "stopped": true,
	})
}
