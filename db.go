package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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
	TS        string `json:"ts"`
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
	  strftime('%Y-%m-%dT%H:%M:%SZ', timestamp, 'unixepoch') AS ts
	  FROM messages WHERE session_id = ` + lit + ` AND id > ` + strconv.FormatInt(sinceID, 10) + `
	  ORDER BY id ASC LIMIT ` + strconv.Itoa(limit)
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		e := EventRow{
			ID:        int64(asFloat(r["id"])),
			Role:      asString(r["role"]),
			ToolName:  asString(r["tool_name"]),
			ToolCalls: clip(asString(r["tool_calls"]), maxChars),
			TS:        asString(r["ts"]),
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

// roleFilter is the validated value of the /messages `role` parameter:
// "" (= all roles) or a single accepted role string.
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
// order (oldest first): selects the newest limit rows by id DESC, then
// reverses in Go. role selects which rows: "" = user+assistant (default),
// otherwise exactly one of the accepted roles; other roles (tool/system) are
// always skipped. role must come from parseRoleFilter, never from raw input.
func LoadMessages(dbPath, sid string, role roleFilter, limit int) ([]MsgRow, error) {
	out := []MsgRow{}
	lit := idLiteral(sid)
	if lit == "" {
		return out, nil
	}
	q := `SELECT id AS id, role AS role, content AS content,
	  strftime('%Y-%m-%dT%H:%M:%SZ', timestamp, 'unixepoch') AS ts
	  FROM messages WHERE ` + role.rolePredicate() + ` AND session_id = ` + lit + `
	  ORDER BY id DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out = append(out, MsgRow{
			Role:      asString(r["role"]),
			Content:   asString(r["content"]),
			Timestamp: asString(r["ts"]),
		})
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
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
