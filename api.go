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
	m.HandleFunc("GET /sessions/{id}", r.handleGetSession)
	m.HandleFunc("GET /sessions/{id}/pane", r.handlePane)
	m.HandleFunc("GET /sessions/{id}/messages", r.handleMessages)
	m.HandleFunc("GET /sessions/{id}/events", gzipIfAccepted(r.handleEvents))
	m.HandleFunc("POST /sessions/{id}/break", r.handleBreak)
	m.HandleFunc("POST /sessions/{id}/send", r.handleSend)
	m.HandleFunc("POST /sessions/{id}/paste", r.handlePaste)
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
				if lines, err := capturePane(panes[i].Pane.ID, r.captureMax); err == nil {
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
	lines, err := capturePane(pid, r.captureMax)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "pane_id": string(pid), "lines": lines})
}

// handleMessages serves GET /sessions/{id}/messages?limit=N&role=R: recent
// conversation history, chronological (oldest first, newest last). role
// selects user or assistant rows only; the default (all) keeps today's
// user+assistant behaviour.
func (r *Registry) handleMessages(w http.ResponseWriter, req *http.Request) {
	sid := req.PathValue("id")
	_, ps, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
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
	role, ok := parseRoleFilter(req.URL.Query().Get("role"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid role: must be user, assistant or all")
		return
	}
	msgs, err := LoadMessages(r.dbFor(ps.Profile), sid, role, limit)
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
	_, ps, err := r.findPaneBySession(sid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
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

	events, err := LoadEvents(r.dbFor(ps.Profile), sid, sinceID, limit, maxChars)
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
	res, err := sendText(string(pid), body.Text, enter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
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
	res, err := pasteText(string(pid), body.Text, enter)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
