package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SessionRow mirrors a row of the hermes sessions table (read-only).
type SessionRow struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Profile string `json:"profile"`
	CWD     string `json:"cwd"`
	Ended   bool   `json:"ended"`
	EndedAt string `json:"ended_at"`
}

// MsgRow is one message from the messages table.
type MsgRow struct {
	ID        int64  `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	Timestamp string `json:"ts"`
}

// LastExchange holds the most recent human query and agent answer, plus how many
// tool calls the agent has made since that query — the step count a console can
// show without reading the database itself.
type LastExchange struct {
	LastQuery  *MsgRow `json:"last_query,omitempty"`
	LastAnswer *MsgRow `json:"last_answer,omitempty"`
	// Steps is the number of tool rows after the last human question. It is what
	// "it is working" looks like as a number, and it survives the query/answer pair
	// not changing while the agent works through a long turn.
	Steps int `json:"steps_since_query,omitempty"`
}

// EventRow is one message of ANY role, unlike MsgRow: the console keeps a local
// archive of the whole session, so tool rows and the tool calls on assistant
// rows are the point, not noise. Long fields are clipped before they go on the
// wire — a tool's output is not worth its bytes to an archive that only needs to
// know which files were touched.
type EventRow struct {
	ID        int64  `json:"id"`
	Role      string `json:"role"`
	Content   string `json:"content,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	ToolCalls string `json:"tool_calls,omitempty"`
	// ToolCallID pairs a tool row with the call on the assistant row that asked
	// for it; a renderer can then show the call's arguments beside the output.
	ToolCallID string `json:"tool_call_id,omitempty"`
	TS         string `json:"ts"`
}

// hardContentCap bounds a text row even when no clipping is asked for: a
// pathological answer must not turn one event page into a hundred megabytes.
const hardContentCap = 200_000

// clip truncates s to at most n bytes on a rune boundary, marking the cut. n <= 0
// means "no clipping at all".
func clip(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// LoadEvents returns message rows for one session in id order (oldest first),
// starting after sinceID. It is the incremental feed the console archives: the
// caller stores the last id it received and asks for everything newer next time,
// so a session is pulled once and only its new rows ever travel again.
//
// sinceID 0 means the whole history from the beginning, paged by limit. maxChars
// clips a tool row's output and every row's tool_calls; user and assistant text
// is kept whole up to hardContentCap. Returns rows ascending; the caller detects
// "there is more" simply by getting a full page.
func LoadEvents(dbPath, sid string, sinceID int64, limit, maxChars int) ([]EventRow, error) {
	out := []EventRow{}
	lit := idLiteral(sid)
	if lit == "" {
		return out, nil
	}
	q := `SELECT id AS id, role AS role, coalesce(content,'') AS content,
	  coalesce(reasoning_content,'') AS reasoning,
	  coalesce(tool_name,'') AS tool_name, coalesce(tool_calls,'') AS tool_calls,
	  coalesce(tool_call_id,'') AS tool_call_id,
	  strftime('%Y-%m-%dT%H:%M:%SZ', timestamp, 'unixepoch') AS ts
	  FROM messages WHERE session_id = ` + lit + ` AND id > ` + strconv.FormatInt(sinceID, 10) + `
	  ORDER BY id ASC LIMIT ` + strconv.Itoa(limit)
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		e := EventRow{
			ID:         int64(asFloat(r["id"])),
			Role:       asString(r["role"]),
			ToolName:   asString(r["tool_name"]),
			ToolCalls:  clip(asString(r["tool_calls"]), maxChars),
			ToolCallID: asString(r["tool_call_id"]),
			TS:         asString(r["ts"]),
		}
		// The agent's own thinking is clipped like a tool row rather than kept
		// whole: it is the biggest column in the table by far, and a reader that
		// wants to know what the turn is doing needs its gist, not its length.
		if e.Role == "assistant" {
			e.Reasoning = clip(asString(r["reasoning"]), maxChars)
		}
		switch e.Role {
		case "tool", "system":
			e.Content = clip(asString(r["content"]), maxChars)
		default:
			e.Content = clip(asString(r["content"]), hardContentCap)
		}
		out = append(out, e)
	}
	return out, nil
}

// sqliteJSON runs a read-only query via the sqlite3 CLI and returns JSON rows.
func sqliteJSON(dbPath, query string, args []string) ([]map[string]any, error) {
	// A WAL database can answer "unable to open database file" while a writer
	// checkpoint it — a transient that has nothing to do with the query. It is
	// retried here, at the one choke point every reader goes through, because a
	// single failed read used to blank a whole rail (state.db is read several
	// times per poll while hermes writes to it). Anything else fails at once: a
	// missing db or a bad statement must not be retried into a slow answer.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 150 * time.Millisecond)
		}
		rows, err := sqliteJSONOnce(dbPath, query, args)
		if err == nil {
			return rows, nil
		}
		lastErr = err
		if !strings.Contains(err.Error(), "unable to open database file") {
			return nil, err
		}
	}
	return nil, lastErr
}

func sqliteJSONOnce(dbPath, query string, args []string) ([]map[string]any, error) {
	cmdArgs := []string{"-readonly", "-json", dbPath, query}
	cmdArgs = append(cmdArgs, args...)
	out, err := exec.Command("sqlite3", cmdArgs...).Output()
	if err != nil {
		detail := ""
		if ee, ok := err.(*exec.ExitError); ok {
			detail = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("sqlite3: %v: %s", err, tailStr(detail, 300))
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil, nil
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(s), &rows); err != nil {
		return nil, fmt.Errorf("parse sqlite json: %w", err)
	}
	return rows, nil
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func asBool(v any) bool { return asString(v) == "1" }

// asInt64 reads a row id that sqlite3 -json hands back as a JSON number (or,
// on very large ids, a string).
func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	case int64:
		return n
	}
	return 0
}

// asFloat reads a number that sqlite3 -json may hand back as a JSON number.
func asFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case string:
		f, _ := strconv.ParseFloat(n, 64)
		return f
	}
	return 0
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// idLiteral validates a session id (strict charset) and renders it as a
// single-quoted SQL literal. Returns "" if invalid.
func idLiteral(id string) string {
	if !sessionIDRe.MatchString(id) {
		return ""
	}
	return "'" + id + "'"
}

// idList renders validated ids as a comma-joined SQL IN list.
func idList(ids []string) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		if l := idLiteral(id); l != "" {
			parts = append(parts, l)
		}
	}
	return strings.Join(parts, ",")
}

// LoadSessions fetches session rows for the given ids.
func LoadSessions(dbPath string, ids []string) (map[string]SessionRow, error) {
	res := map[string]SessionRow{}
	in := idList(ids)
	if in == "" {
		return res, nil
	}
	q := `SELECT id AS id, coalesce(title,'') AS title,
		coalesce(profile_name,'default') AS profile, coalesce(cwd,'') AS cwd,
		(ended_at IS NOT NULL AND ended_at != 0) AS ended,
		coalesce(strftime('%Y-%m-%dT%H:%M:%SZ', ended_at, 'unixepoch'),'') AS ended_ts
		FROM sessions WHERE id IN (` + in + `)`
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		s := SessionRow{
			ID:      asString(r["id"]),
			Title:   asString(r["title"]),
			Profile: asString(r["profile"]),
			CWD:     asString(r["cwd"]),
			Ended:   asBool(r["ended"]),
			EndedAt: asString(r["ended_ts"]),
		}
		res[s.ID] = s
	}
	return res, nil
}

// LoadLastMessages returns the latest user and assistant messages per session.
func LoadLastMessages(dbPath string, ids []string) (map[string]LastExchange, error) {
	res := map[string]LastExchange{}
	in := idList(ids)
	if in == "" {
		return res, nil
	}
	q := `WITH want AS (
		  SELECT session_id, role, max(id) AS maxid FROM messages
		  WHERE role IN ('user','assistant') AND session_id IN (` + in + `)
		  GROUP BY session_id, role
		)
		SELECT m.session_id AS session_id, m.role AS role, m.content AS content,
		  strftime('%Y-%m-%dT%H:%M:%SZ', m.timestamp, 'unixepoch') AS ts
		FROM messages m JOIN want w
		  ON w.session_id = m.session_id AND w.role = m.role AND w.maxid = m.id`
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		sid := asString(r["session_id"])
		le := res[sid]
		m := MsgRow{Role: asString(r["role"]), Content: asString(r["content"]), Timestamp: asString(r["ts"])}
		if m.Role == roleUser {
			le.LastQuery = &m
		} else {
			le.LastAnswer = &m
		}
		res[sid] = le
	}
	// One more query for the step counts: a second statement against the same
	// database is a second connection, and folding it into the query above would
	// mean a correlated subquery per message row.
	steps, err := LoadStepsSinceQuery(dbPath, ids)
	if err != nil {
		return res, nil // the counts are a nicety; the queries and answers are not
	}
	for sid, n := range steps {
		le := res[sid]
		le.Steps = n
		res[sid] = le
	}
	return res, nil
}

// LoadStepsSinceQuery counts, per session, the tool rows that follow the last human
// question: a turn's progress as a number the console can put beside the clock.
// A session whose last question is gone from the table counts all of its tools,
// which is honest for "since the last question" — there is none.
func LoadStepsSinceQuery(dbPath string, ids []string) (map[string]int, error) {
	res := map[string]int{}
	in := idList(ids)
	if in == "" {
		return res, nil
	}
	q := `WITH lastuser AS (
		  SELECT session_id, max(id) AS uid FROM messages
		  WHERE role = 'user' AND session_id IN (` + in + `)
		  GROUP BY session_id
		)
		SELECT m.session_id AS session_id, count(*) AS steps
		FROM messages m LEFT JOIN lastuser u ON u.session_id = m.session_id
		WHERE m.role = 'tool' AND m.session_id IN (` + in + `)
		  AND m.id > coalesce(u.uid, 0)
		GROUP BY m.session_id`
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		res[asString(r["session_id"])] = int(asFloat(r["steps"]))
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// session tree (subagents)
//
// A delegate_task child is a real session row of source='subagent', carrying
// parent_session_id and a _delegate_from marker in model_config. The link the
// schema does NOT store is which delegate_task tool call spawned which child:
// the call's tool_call_id is never written to the child row, and the dispatch
// response only names a delegation_id. What IS stored on both sides is the
// goal text — the call's arguments carry tasks[].goal, and the child's first
// user row (the kickoff) repeats that goal verbatim — plus the delegation_id
// in the dispatch response. Children are therefore attributed to their calls
// by goal text; children whose goal matches no call in the window (an older
// daemon, a rewritten goal) keep the session-level parent link and no call.
// ---------------------------------------------------------------------------

// ChildCall is one delegate_task call found in the window: the tool_call_id of
// the assistant call (a tool row's tool_call_id is the same string), the
// compact task list, and the delegation_id its dispatch response returned.
type ChildCall struct {
	ToolCallID   string          `json:"tool_call_id"`
	ToolCallUID  string          `json:"tool_call_uid,omitempty"` // second-generation id, when the row carries one
	DelegationID string          `json:"delegation_id,omitempty"`
	Tasks        []ChildCallTask `json:"tasks,omitempty"`
}

type ChildCallTask struct {
	Index int    `json:"index"`
	Goal  string `json:"goal"`
}

// ChildSession is one subagent session under a parent.
type ChildSession struct {
	ID        string `json:"id"`
	Model     string `json:"model,omitempty"`
	StartedTS string `json:"started_ts,omitempty"`
	Ended     bool   `json:"ended"`
	// TaskIndex is the position of this child within its call's tasks list,
	// filled when the goal text matched the call's own task entry.
	TaskIndex    *int   `json:"task_index,omitempty"`
	Goal         string `json:"goal,omitempty"`         // the call's goal it matched (may be clipped)
	Kickoff      string `json:"kickoff,omitempty"`      // its own first user row, clipped
	ToolCallID   string `json:"tool_call_id,omitempty"` // the delegate_task call that spawned it
	DelegationID string `json:"delegation_id,omitempty"`
	MessageCount int    `json:"message_count"`
}

// TreeRow is the response of the tree loader: the parent's identity plus every
// subagent child under it and every delegate_task call found.
type TreeRow struct {
	Parent   SessionRow     `json:"parent"`
	Calls    []ChildCall    `json:"calls"`
	Children []ChildSession `json:"children"`
}

// delegateWindow is how far back delegate_task calls are looked for: children
// usually arrive within a minute of the call, and a window anchored on the
// child's own start keeps the scan bounded without missing a slow dispatch.
const delegateWindow = 30 * 24 * time.Hour

// subagentChildrenSQL lists the subagent session rows for one parent. The
// source column is the original label; the _delegate_from marker is checked as
// well so a child that predates the marker still matches (source survives
// compression lineage rewrites).
const subagentChildrenSQL = `
SELECT c.id AS id, coalesce(c.model,'') AS model,
  coalesce(strftime('%%Y-%%m-%%dT%%H:%%M:%%SZ', c.started_at, 'unixepoch'),'') AS started_ts,
  (c.ended_at IS NOT NULL AND c.ended_at != 0) AS ended,
  coalesce(c.message_count,0) AS message_count,
  json_extract(c.model_config,'$._delegate_from') AS df
FROM sessions c
WHERE c.parent_session_id = %s
  AND (LOWER(c.source) = 'subagent' OR json_extract(c.model_config,'$._delegate_from') = %s)
ORDER BY c.started_at ASC`

// LoadSessionTree returns the parent's row, every subagent child under it, and
// every delegate_task call in the window, with children matched to calls by
// goal text. The whole thing is three queries against the session's own
// profile database; nothing here needs a pane.
func LoadSessionTree(dbPath, parentID string) (TreeRow, error) {
	out := TreeRow{}
	lit := idLiteral(parentID)
	if lit == "" {
		return out, fmt.Errorf("invalid session id")
	}
	// the parent's own row first: the tree names the session it is about
	prow, err := sqliteJSON(dbPath, `SELECT id AS id, coalesce(title,'') AS title,
	  coalesce(profile_name,'default') AS profile, coalesce(cwd,'') AS cwd,
	  (ended_at IS NOT NULL AND ended_at != 0) AS ended,
	  coalesce(strftime('%Y-%m-%dT%H:%M:%SZ', ended_at, 'unixepoch'),'') AS ended_ts
	  FROM sessions WHERE id = `+lit, nil)
	if err != nil {
		return out, err
	}
	if len(prow) == 0 {
		return out, fmt.Errorf("session %s not found", parentID)
	}
	r0 := prow[0]
	out.Parent = SessionRow{
		ID:      asString(r0["id"]),
		Title:   asString(r0["title"]),
		Profile: asString(r0["profile"]),
		CWD:     asString(r0["cwd"]),
		Ended:   asBool(r0["ended"]),
		EndedAt: asString(r0["ended_ts"]),
	}
	rows, err := sqliteJSON(dbPath, fmt.Sprintf(subagentChildrenSQL, lit, lit), nil)
	if err != nil {
		return out, err
	}
	children := make([]ChildSession, 0, len(rows))
	for _, r := range rows {
		children = append(children, ChildSession{
			ID:           asString(r["id"]),
			Model:        asString(r["model"]),
			StartedTS:    asString(r["started_ts"]),
			Ended:        asBool(r["ended"]),
			MessageCount: int(asFloat(r["message_count"])),
		})
	}
	out.Children = children
	if len(children) == 0 {
		return out, nil // no children: no calls worth scanning for
	}

	// The calls: delegate_task assistant rows in the window plus their tool
	// rows (the dispatch responses carrying the delegation_id). The window is
	// anchored on the earliest child's start, so the scan is bounded.
	w, err := callWindow(dbPath, parentID)
	if err != nil {
		return out, err
	}
	calls, byID := loadDelegateCalls(dbPath, parentID, w)
	out.Calls = calls
	delegByID := loadDelegationIDs(dbPath, parentID, w)

	// kickoffs: each child's first user row, the goal verbatim.
	kickoffs := loadKickoffs(dbPath, children)

	for i := range out.Children {
		c := &out.Children[i]
		ko := kickoffs[c.ID]
		if ko == "" {
			continue
		}
		c.Kickoff = clip(ko, 4000)
		callID, idx, best := matchCall(byID, ko)
		if callID == "" {
			continue
		}
		c.ToolCallID = callID
		c.TaskIndex = &idx
		c.Goal = clip(best, 4000)
		if d, ok := delegByID[callID]; ok {
			c.DelegationID = d
		}
	}
	return out, nil
}

// callWindow bounds the delegate-call scan: from delegateWindow before the
// earliest child's start (or the parent's own start when no child has a
// timestamp) to "now".
func callWindow(dbPath, parentID string) (*windowSpec, error) {
	var since float64
	row, err := sqliteJSON(dbPath,
		"SELECT coalesce(min(started_at),0) AS s FROM sessions WHERE parent_session_id = "+idLiteral(parentID), nil)
	if err != nil {
		return nil, err
	}
	if len(row) > 0 {
		since = asFloat(row[0]["s"])
	}
	if since <= 0 {
		row, err = sqliteJSON(dbPath,
			"SELECT started_at AS s FROM sessions WHERE id = "+idLiteral(parentID), nil)
		if err != nil || len(row) == 0 {
			return nil, fmt.Errorf("parent row not found")
		}
		since = asFloat(row[0]["s"])
	}
	return &windowSpec{since: since - delegateWindow.Seconds()}, nil
}

type windowSpec struct{ since float64 }

// loadDelegateCalls reads the assistant rows carrying delegate_task calls plus
// the tool rows that answered them (the dispatch responses with a
// delegation_id), both bounded to the window, and parses them. The scan is a
// timestamp range over the session's own messages, so it is an index scan, not
// a table walk.
func loadDelegateCalls(dbPath string, parentID string, w *windowSpec) ([]ChildCall, map[string]*ChildCall) {
	lit := idLiteral(parentID)
	if lit == "" || w == nil {
		return nil, nil
	}
	q := fmt.Sprintf(`SELECT id, role, tool_call_id, tool_call_uid, tool_calls, content FROM messages
	  WHERE session_id = `+lit+` AND timestamp >= %f
	    AND ((role='assistant' AND tool_calls LIKE '%%delegate%%')
	      OR (role='tool' AND tool_name='delegate_task'))
	  ORDER BY id ASC`, w.since)
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, nil
	}
	calls := []ChildCall{}
	byID := map[string]*ChildCall{}
	get := func(id string) *ChildCall {
		if c, ok := byID[id]; ok {
			return c
		}
		c := &ChildCall{ToolCallID: id}
		byID[id] = c
		calls = append(calls, *c)
		return c
	}
	for _, r := range rows {
		switch asString(r["role"]) {
		case "assistant":
			tc := asString(r["tool_calls"])
			var list []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			if json.Unmarshal([]byte(tc), &list) != nil {
				continue
			}
			for _, call := range list {
				if call.Function.Name != "delegate_task" {
					continue
				}
				c := get(call.ID)
				var args struct {
					Tasks []struct {
						Goal string `json:"goal"`
					} `json:"tasks"`
				}
				if json.Unmarshal([]byte(call.Function.Arguments), &args) == nil {
					for i, t := range args.Tasks {
						c.Tasks = append(c.Tasks, ChildCallTask{Index: i, Goal: t.Goal})
					}
				}
			}
		case "tool":
			// the dispatch response: its delegation_id belongs to the call the
			// row answers (tool_call_id), and a uid may ride on the row itself.
			if id := asString(r["tool_call_id"]); id != "" {
				c := get(id)
				if c.ToolCallUID == "" {
					c.ToolCallUID = asString(r["tool_call_uid"])
				}
				var resp struct {
					DelegationID string   `json:"delegation_id"`
					SubagentIDs  []string `json:"subagent_ids"`
				}
				if json.Unmarshal([]byte(asString(r["content"])), &resp) == nil && resp.DelegationID != "" {
					c.DelegationID = resp.DelegationID
				}
			}
		}
	}
	return calls, byID
}

// loadDelegationIDs returns call id -> delegation_id from the dispatch
// responses in the window. Kept separate from the call parsing because a
// dispatch response can arrive after the assistant row was parsed (the call is
// written first, the response lands when the daemon answers).
func loadDelegationIDs(dbPath string, parentID string, w *windowSpec) map[string]string {
	lit := idLiteral(parentID)
	out := map[string]string{}
	if lit == "" || w == nil {
		return out
	}
	q := fmt.Sprintf(`SELECT tool_call_id, content FROM messages
	  WHERE session_id = `+lit+` AND role='tool' AND tool_name='delegate_task'
	    AND timestamp >= %f ORDER BY id ASC`, w.since)
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return out
	}
	for _, r := range rows {
		cid := asString(r["tool_call_id"])
		if cid == "" {
			continue
		}
		var resp struct {
			DelegationID string `json:"delegation_id"`
		}
		if json.Unmarshal([]byte(asString(r["content"])), &resp) == nil && resp.DelegationID != "" {
			out[cid] = resp.DelegationID
		}
	}
	return out
}

// loadKickoffs returns child session id -> its first user row (the kickoff,
// which repeats the call's goal verbatim).
func loadKickoffs(dbPath string, children []ChildSession) map[string]string {
	out := map[string]string{}
	for _, c := range children {
		rows, err := sqliteJSON(dbPath,
			`SELECT content FROM messages WHERE session_id = `+idLiteral(c.ID)+`
			  AND role='user' AND coalesce(content,'') != '' ORDER BY id ASC LIMIT 1`, nil)
		if err != nil || len(rows) == 0 {
			continue
		}
		out[c.ID] = asString(rows[0]["content"])
	}
	return out
}

// matchCall finds the call whose tasks[] goal equals the child's kickoff. The
// comparison is exact-prefix on the trimmed strings: hermes writes the kickoff
// verbatim today, and a fallback to longest common prefix keeps an
// almost-equal pair (a reformatting writer) matched to its call. A call whose
// goals all miss keeps the child unattributed — the session-level parent link
// is still there, so nothing is lost, only the call edge.
func matchCall(byID map[string]*ChildCall, kickoff string) (callID string, taskIndex int, goal string) {
	ko := strings.TrimSpace(kickoff)
	if ko == "" {
		return "", -1, ""
	}
	bestLen := 0
	for id, c := range byID {
		for _, t := range c.Tasks {
			g := strings.TrimSpace(t.Goal)
			if g == "" {
				continue
			}
			if g == ko || strings.HasPrefix(ko, g) || strings.HasPrefix(g, ko) {
				return id, t.Index, g
			}
			// partial overlap: track the longest shared prefix, use it only
			// when it covers most of both strings (a reformat, not a hash hit)
			p := commonPrefixLen(g, ko)
			if p > bestLen && p*4 >= len(g)*3 && p*4 >= len(ko)*3 {
				bestLen = p
				callID, taskIndex, goal = id, t.Index, g
			}
		}
	}
	return callID, taskIndex, goal
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	// avoid splitting a utf-8 rune
	for n > 0 && n < len(a) && (a[n]&0xC0) == 0x80 {
		n--
	}
	return n
}

// LoadChildEvents returns a subagent child's whole event stream (any role) —
// the same shape /events gives a pane's session, so the console can render the
// child's work as a tree under the parent's exchange. It refuses any session
// that is not a subagent child of the named parent: the route exists for the
// tree, not as a general history export.
func LoadChildEvents(dbPath, parentID, childID string, limit, maxChars int) ([]EventRow, error) {
	out := []EventRow{}
	lit := idLiteral(childID)
	if lit == "" {
		return out, fmt.Errorf("invalid session id")
	}
	// the gate: one query, no row materialised unless the child belongs here
	gate := `SELECT count(*) AS n FROM sessions
	  WHERE id = ` + lit + ` AND parent_session_id = ` + idLiteral(parentID) + `
	    AND (LOWER(coalesce(source,'')) = 'subagent'
	      OR json_extract(model_config,'$._delegate_from') = ` + idLiteral(parentID) + `)`
	rows, err := sqliteJSON(dbPath, gate, nil)
	if err != nil {
		return out, err
	}
	if len(rows) == 0 || asFloat(rows[0]["n"]) < 1 {
		return out, fmt.Errorf("session %s is not a subagent of %s", childID, parentID)
	}
	q := `SELECT id AS id, role AS role, coalesce(content,'') AS content,
	  coalesce(reasoning_content,'') AS reasoning,
	  coalesce(tool_name,'') AS tool_name, coalesce(tool_calls,'') AS tool_calls,
	  coalesce(tool_call_id,'') AS tool_call_id,
	  strftime('%Y-%m-%dT%H:%M:%SZ', timestamp, 'unixepoch') AS ts
	  FROM messages WHERE session_id = ` + lit + `
	  ORDER BY id ASC LIMIT ` + strconv.Itoa(limit)
	rws, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return out, err
	}
	for _, r := range rws {
		e := EventRow{
			ID:         int64(asFloat(r["id"])),
			Role:       asString(r["role"]),
			ToolName:   asString(r["tool_name"]),
			ToolCalls:  clip(asString(r["tool_calls"]), maxChars),
			ToolCallID: asString(r["tool_call_id"]),
			TS:         asString(r["ts"]),
		}
		if e.Role == "assistant" {
			e.Reasoning = clip(asString(r["reasoning"]), maxChars)
		}
		switch e.Role {
		case "tool", "system":
			e.Content = clip(asString(r["content"]), maxChars)
		default:
			e.Content = clip(asString(r["content"]), hardContentCap)
		}
		out = append(out, e)
	}
	return out, nil
}

// ExecChildNames names the subagent sessions a delegate_task call produced, in
// dispatch order: the tree under the call's node.
type ExecChildNames struct {
	Calls []ChildCall               `json:"calls"` // delegate_task calls in the window
	Kids  map[string][]ChildSession `json:"kids"`  // tool_call_id -> its children, in start order
}

// LoadSessionExec returns the parent's event stream plus, per delegate_task
// call, the subagent sessions that call spawned. The console renders the
// stream as the execution tree and hangs each call's subtree off its node.
// The parent may be on no pane (a finished session): the db is probed by the
// caller, the same way /tree resolves it.
func LoadSessionExec(dbPath, parentID string, sinceID int64, limit, maxChars int) ([]EventRow, ExecChildNames, error) {
	events, err := LoadEvents(dbPath, parentID, sinceID, limit, maxChars)
	if err != nil {
		return nil, ExecChildNames{}, err
	}
	out := ExecChildNames{Calls: []ChildCall{}, Kids: map[string][]ChildSession{}}
	if len(events) == 0 {
		return events, out, nil
	}
	// children + calls, the same machinery /tree uses: children of the parent
	// by session link, calls by goal-text matching. Kids are grouped by the
	// call's tool_call_id; an unmatched child is not keyed by a call and shows
	// under the parent itself.
	lit := idLiteral(parentID)
	if lit == "" {
		return events, out, fmt.Errorf("invalid session id")
	}
	rows, err := sqliteJSON(dbPath, fmt.Sprintf(subagentChildrenSQL, lit, lit), nil)
	if err != nil {
		return events, out, err
	}
	children := make([]ChildSession, 0, len(rows))
	for _, r := range rows {
		children = append(children, ChildSession{
			ID:           asString(r["id"]),
			Model:        asString(r["model"]),
			StartedTS:    asString(r["started_ts"]),
			Ended:        asBool(r["ended"]),
			MessageCount: int(asFloat(r["message_count"])),
		})
	}
	if len(children) == 0 {
		return events, out, nil
	}
	w, err := callWindow(dbPath, parentID)
	if err != nil {
		return events, out, nil // children without a window: names only, no edges
	}
	calls, byID := loadDelegateCalls(dbPath, parentID, w)
	out.Calls = calls
	delegByID := loadDelegationIDs(dbPath, parentID, w)
	kickoffs := loadKickoffs(dbPath, children)
	for i := range children {
		c := &children[i]
		ko := kickoffs[c.ID]
		if ko == "" {
			continue
		}
		c.Kickoff = clip(ko, 4000)
		callID, idx, best := matchCall(byID, ko)
		if callID == "" {
			continue // session-level link only: the console shows it unmatched
		}
		c.ToolCallID = callID
		c.TaskIndex = &idx
		c.Goal = clip(best, 4000)
		if d, ok := delegByID[callID]; ok {
			c.DelegationID = d
		}
		out.Kids[callID] = append(out.Kids[callID], *c)
	}
	return events, out, nil
}

type roleFilter string

// parseRoleFilter maps the raw `role` query value to a validated filter.
// Accepted: "user", "assistant", "all" (and empty = all). Anything else -> "".
func parseRoleFilter(raw string) (roleFilter, bool) {
	switch raw {
	case "", "all":
		return "", true
	case roleUser, roleAssistant:
		return roleFilter(raw), true
	default:
		return "", false
	}
}

// rolePredicate renders the validated role filter as a SQL predicate.
// It is only called with values produced by parseRoleFilter, so the
// interpolated literal is limited to the two accepted role strings.
func (f roleFilter) rolePredicate() string {
	if f == "" {
		return "role IN ('user','assistant')"
	}
	return "role = '" + string(f) + "'"
}

// LoadMessages returns up to limit messages for one session, chronological
// order (oldest first). It reads the WHOLE conversation: with beforeID it
// pages backwards (rows older than the bound, newest of them first, reversed
// in Go), with sinceID it pages forwards (rows newer than the cursor, oldest
// first) — the same incremental crawl /events serves for tool rows. With
// neither it selects the newest limit rows by id DESC and reverses. role
// selects which rows: "" = user+assistant (default), otherwise exactly one of
// the accepted roles; other roles (tool/system) are always skipped. role must
// come from parseRoleFilter, never from raw input. The row ids ride along:
// they are the paging cursor and the only tie-proof key (timestamps tie).
func LoadMessages(dbPath, sid string, role roleFilter, limit int, beforeID, sinceID int64) ([]MsgRow, error) {
	out := []MsgRow{}
	lit := idLiteral(sid)
	if lit == "" {
		return out, nil
	}
	extra := ""
	forward := false
	if beforeID > 0 {
		extra += " AND id < " + strconv.FormatInt(beforeID, 10)
	}
	if sinceID > 0 {
		extra += " AND id > " + strconv.FormatInt(sinceID, 10)
		forward = true
	}
	order := "DESC"
	if forward {
		order = "ASC"
	}
	q := `SELECT id AS id, role AS role, content AS content,
	  strftime('%Y-%m-%dT%H:%M:%SZ', timestamp, 'unixepoch') AS ts
	  FROM messages WHERE ` + role.rolePredicate() + ` AND session_id = ` + lit + extra + `
	  ORDER BY id ` + order + ` LIMIT ` + strconv.Itoa(limit)
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, MsgRow{
			ID:        asInt64(r["id"]),
			Role:      asString(r["role"]),
			Content:   asString(r["content"]),
			Timestamp: asString(r["ts"]),
		})
	}
	if !forward {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out, nil
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
