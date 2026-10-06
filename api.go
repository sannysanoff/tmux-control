package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
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

	// enrich with session rows + last messages (small id set)
	idset := map[string]bool{}
	for _, ps := range panes {
		if ps.SessionID != "" {
			idset[ps.SessionID] = true
		}
	}
	ids := make([]string, 0, len(idset))
	for id := range idset {
		ids = append(ids, id)
	}
	sessions, err := LoadSessions(r.dbPath, ids)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msgs, _ := LoadLastMessages(r.dbPath, ids)
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
	if s, err2 := LoadSessions(r.dbPath, []string{sid}); err2 == nil {
		if srow, ok := s[sid]; ok {
			ps.Session = &srow
		}
	}
	if le, err2 := LoadLastMessages(r.dbPath, []string{sid}); err2 == nil {
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
	if _, _, err := r.findPaneBySession(sid); err != nil {
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
	msgs, err := LoadMessages(r.dbPath, sid, role, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": sid, "count": len(msgs), "messages": msgs})
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
