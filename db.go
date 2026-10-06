package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
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

// LastExchange holds the most recent human query and agent answer.
type LastExchange struct {
	LastQuery  *MsgRow `json:"last_query,omitempty"`
	LastAnswer *MsgRow `json:"last_answer,omitempty"`
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
