package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Session search: grepping what sessions SAID and DID, over the profile
// state.db of the box this daemon runs on.
//
// The corpus is not the rail. The rail is panes, live and few; a search has to
// reach every session the profile ever ran, most of which sit on no pane at all.
// So this route reads the database directly, per profile, and it is the daemon
// and not the console that reads it: each box owns its own state.db, and the
// console has no filesystem that holds it.
//
// Three kinds of text, and the spec picks which is searchable:
//   - user text      (role='user', content)            — always
//   - model text     (role='assistant', content)       — always, thinking excluded
//   - tool calls     (role='assistant', tool_calls)    — only with ?tools=1
// Tool RESULTS (role='tool', content) are never searched: "tool calls, not
// responses" is exactly that split.

// SearchHit is one matching session. Everything a result row needs to stand on
// its own is here, because the console merges rows from several endpoints and
// must not go back for more: the score, the timestamps, the step count, and the
// session's own opening question.
type SearchHit struct {
	SessionID string `json:"session_id"`
	// Profile is which profile's state.db the row was found in. The sessions
	// table has no profile column, so this is the daemon's own knowledge of
	// which database it just queried.
	Profile string `json:"profile,omitempty"`
	Title   string `json:"title,omitempty"`
	// FirstQuery is the session's first human message: what the session was
	// opened to do. The result row shows it, so a reader can recognise the
	// session without opening it.
	FirstQuery string `json:"first_query,omitempty"`
	// Score is how many occurrences the terms had in this session, summed over
	// both matched texts and tool calls when they were searched.
	Score int `json:"score"`
	// Hits is the number of matching messages, which is what a "12 messages"
	// count in the row means.
	Hits int `json:"hits"`
	// Steps is the session's total tool-call count (sessions.tool_call_count),
	// not steps-since-the-last-question: a search reports the whole session.
	Steps int `json:"steps"`
	// FirstTS and LastTS bound the session's own activity, from its messages.
	// LastTS is when the session was last touched, which is what orders the
	// results within an endpoint.
	FirstTS string `json:"first_ts,omitempty"`
	LastTS  string `json:"last_ts,omitempty"`
	// Matched is the message ids that contained a term, newest first, so the
	// console can jump straight to the interesting part of a long session.
	Matched []int64 `json:"matched,omitempty"`
}

// SearchTerms is a parsed query: a set of literals that must ALL match, plus the
// same set for highlighting. Parsing lives on the daemon because the daemon
// needs it for the SQL; the console re-derives nothing and paints from the terms
// the answer carries, so the two views cannot disagree about what matched.
type SearchTerms struct {
	// Terms are lowercase literals, deduplicated, empty ones dropped.
	Terms []string `json:"terms"`
	// Phrases are the quoted parts, kept whole: a phrase is one literal, not a
	// bag of words, and it is quoted again on the wire so the client can tell
	// them apart when highlighting.
	Phrases []string `json:"phrases,omitempty"`
}

// ParseSearchTerms turns the raw query into the set of literals to match.
//
// The grammar is deliberately small: whitespace separates terms, and a double
// quote groups its contents into ONE term ("header was rendered in compact
// manner"). Everything is case-insensitive. An unterminated quote is treated as
// if it had been closed at the end, because a half-typed query must still search
// for what the person meant rather than returning nothing.
func ParseSearchTerms(raw string) SearchTerms {
	var out SearchTerms
	seen := map[string]bool{}
	add := func(s string, phrase bool) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		low := strings.ToLower(s)
		if seen[low] {
			return
		}
		seen[low] = true
		out.Terms = append(out.Terms, low)
		if phrase {
			out.Phrases = append(out.Phrases, low)
		}
	}
	var cur strings.Builder
	inQuote := false
	for _, r := range raw {
		switch {
		case r == '"':
			if inQuote {
				add(cur.String(), true)
				cur.Reset()
				inQuote = false
			} else {
				add(cur.String(), false)
				cur.Reset()
				inQuote = true
			}
		case !inQuote && (r == ' ' || r == '\t' || r == '\n' || r == '\r'):
			add(cur.String(), false)
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	add(cur.String(), inQuote) // an unclosed quote still means one phrase
	return out
}

// sqlQuote renders a string as a single-quoted SQL literal, doubling embedded
// quotes. Terms come from a URL and are handed to a query, so this is the only
// place user text becomes SQL.
func sqlQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// matchExpr builds the SQL expression that counts a term's occurrences in the
// given columns. instr() rather than LIKE because a term may contain % or _ and
// must be matched literally.
func matchExpr(term string, cols []string) string {
	low := sqlQuote(strings.ToLower(term))
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		// instr(haystack, needle): both lowercased, so the comparison is
		// case-insensitive exactly as the UI's highlight is.
		parts = append(parts, "instr(lower(ifnull("+c+",'')), "+low+")")
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "max(" + strings.Join(parts, ", ") + ")"
}

// scoreExpr counts non-overlapping occurrences of term in one lowercased column.
// SQLite has no count-over-occurrences, so this is a recursive CTE-free
// approximation: the number of positions where the term starts. For scoring and
// ordering that is the right number; it is not used to decide WHETHER a row
// matches (instr does that, once).
func scoreExpr(term string, cols []string) string {
	low := sqlQuote(strings.ToLower(term))
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		parts = append(parts, "(length(lower(ifnull("+c+",''))) - length(replace(lower(ifnull("+c+",'')), "+low+", ''))) / length("+low+")")
	}
	return "(" + strings.Join(parts, " + ") + ")"
}

// SearchSessions runs one search over one state.db and returns the matching
// top-level sessions, newest activity first.
//
// limit is the caller's ceiling on the answer, not on the scan: every match is
// scored, then the newest `limit` are returned. Ordering by date rather than by
// score is deliberate — a search is a place to look through, not a leaderboard,
// and a stale session that mentions the term 40 times is not more useful than
// the one being worked on today. Score still rides on every row so the reader
// can see why a row is there.
func SearchSessions(dbPath, profile string, terms SearchTerms, withTools bool, limit int) ([]SearchHit, error) {
	if len(terms.Terms) == 0 || dbPath == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	textCols := []string{"content"}
	// Tool calls are a separate toggle because they are the expensive half: the
	// column is several times the size of all user+assistant text put together.
	callCols := []string{"tool_calls"}

	// The per-row predicate: every term must appear somewhere in the row's
	// searchable text (or its calls, when they are in scope).
	conds := make([]string, 0, len(terms.Terms))
	for _, t := range terms.Terms {
		cols := append([]string{}, textCols...)
		if withTools {
			cols = append(cols, callCols...)
		}
		conds = append(conds, "("+matchExpr(t, cols)+" > 0)")
	}
	scoreParts := make([]string, 0, len(terms.Terms))
	for _, t := range terms.Terms {
		cols := append([]string{}, textCols...)
		if withTools {
			cols = append(cols, callCols...)
		}
		scoreParts = append(scoreParts, scoreExpr(t, cols))
	}
	scoreSQL := "(" + strings.Join(scoreParts, " + ") + ")"

	// Top-level sessions only: a subagent child repeats its parent's text, so
	// including children doubles rows and inflates scores. parent_session_id is
	// NULL or empty for the sessions a human opened.
	q := `
WITH m AS (
  SELECT session_id AS sid,
         sum(` + scoreSQL + `) AS sc,
         count(*) AS hits,
         min(id) AS first_id,
         max(id) AS last_id,
         min(timestamp) AS first_ts,
         max(timestamp) AS last_ts
    FROM messages
   WHERE role IN ('user','assistant') AND (` + strings.Join(conds, " AND ") + `)
   GROUP BY session_id
)
SELECT m.sid AS sid, m.sc AS sc, m.hits AS hits,
       m.first_ts AS first_ts, m.last_ts AS last_ts,
       coalesce(s.title,'') AS title,
       coalesce(s.tool_call_count,0) AS steps
  FROM m JOIN sessions s ON s.id = m.sid
 WHERE (s.parent_session_id IS NULL OR s.parent_session_id = '')
 ORDER BY m.last_ts DESC
 LIMIT ` + strconv.Itoa(limit)

	rows, err := sqliteJSON(dbPath, q, nil)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(rows))
	for _, r := range rows {
		sid := asString(r["sid"])
		h := SearchHit{
			SessionID: sid,
			Profile:   profile,
			Title:     asString(r["title"]),
			Score:     int(asFloat(r["sc"])),
			Hits:      int(asFloat(r["hits"])),
			Steps:     int(asFloat(r["steps"])),
			FirstTS:   unixToISO(asFloat(r["first_ts"])),
			LastTS:    unixToISO(asFloat(r["last_ts"])),
		}
		out = append(out, h)
	}
	if len(out) == 0 {
		return out, nil
	}

	// The second pass: each hit's opening question and the ids of its matching
	// messages. One query for all of them rather than one per hit.
	ids := make([]string, 0, len(out))
	for _, h := range out {
		ids = append(ids, h.SessionID)
	}
	firsts, matched, err := searchDetails(dbPath, ids, terms, withTools)
	if err == nil {
		for i := range out {
			if q, ok := firsts[out[i].SessionID]; ok {
				out[i].FirstQuery = q
			}
			if mm, ok := matched[out[i].SessionID]; ok {
				out[i].Matched = mm
			}
		}
	}
	// Newest activity first; the sort is stable so equal stamps keep the
	// database's own order.
	sort.SliceStable(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	return out, nil
}

// searchDetails fetches, for the given sessions, each session's first human
// message and the ids of the messages that matched. Both are small strings and
// small id lists; the first query is bounded by the number of hits, and the
// second by the same ids.
func searchDetails(dbPath string, ids []string, terms SearchTerms, withTools bool) (map[string]string, map[string][]int64, error) {
	firsts := map[string]string{}
	matched := map[string][]int64{}
	in := idList(ids)
	if in == "" {
		return firsts, matched, nil
	}
	// First human message per session: the lowest id with role='user'.
	q1 := `SELECT session_id AS sid, content AS content FROM messages
	        WHERE role='user' AND session_id IN (` + in + `)
	          AND id IN (SELECT min(id) FROM messages WHERE role='user'
	                      AND session_id IN (` + in + `) GROUP BY session_id)`
	if rows, err := sqliteJSON(dbPath, q1, nil); err == nil {
		for _, r := range rows {
			firsts[asString(r["sid"])] = asString(r["content"])
		}
	}
	conds := make([]string, 0, len(terms.Terms))
	for _, t := range terms.Terms {
		cols := append([]string{}, "content")
		if withTools {
			cols = append(cols, "tool_calls")
		}
		conds = append(conds, "("+matchExpr(t, cols)+" > 0)")
	}
	q2 := `SELECT session_id AS sid, id AS id FROM messages
	        WHERE role IN ('user','assistant') AND ` + strings.Join(conds, " AND ") + `
	          AND session_id IN (` + in + `)
	        ORDER BY id DESC`
	if rows, err := sqliteJSON(dbPath, q2, nil); err == nil {
		for _, r := range rows {
			sid := asString(r["sid"])
			matched[sid] = append(matched[sid], asInt64(r["id"]))
		}
	}
	return firsts, matched, nil
}

// unixToISO renders a REAL unix timestamp the way every other route does.
func unixToISO(f float64) string {
	if f <= 0 {
		return ""
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec).UTC().Format(time.RFC3339)
}

// searchProfileDBs names every state.db this daemon may search: the default
// profile's own database plus one per named profile, skipping the ones that do
// not exist so a profile directory without a database is not an error.
func (r *Registry) searchProfileDBs() []struct{ Profile, Path string } {
	out := []struct{ Profile, Path string }{}
	if r.dbPath != "" {
		out = append(out, struct{ Profile, Path string }{"default", r.dbPath})
	}
	entries, err := os.ReadDir(filepath.Join(r.hermesHome, "profiles"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := r.dbFor(e.Name())
		if p == "" {
			continue
		}
		if _, statErr := os.Stat(p); statErr != nil {
			continue
		}
		out = append(out, struct{ Profile, Path string }{e.Name(), p})
	}
	return out
}

// formatSearchError keeps a failure attributable: a search fans out over every
// profile, and "one of them failed" is useless unless it says which.
func formatSearchError(db string, err error) string {
	return fmt.Sprintf("%s: %v", filepath.Base(filepath.Dir(db)), err)
}