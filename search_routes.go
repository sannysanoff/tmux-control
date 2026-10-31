package main

import (
	"log"
	"net/http"
	"strconv"
)

// SearchAnswer is one endpoint's answer to a corpus search: the rows it found,
// the terms it actually matched on, and which kinds of text were in scope. The
// console merges one of these per endpoint and prints the union.
type SearchAnswer struct {
	Endpoint string `json:"endpoint,omitempty"`
	// Terms is the parsed query, echoed back so the console highlights exactly
	// what was matched — including the phrase boundaries, which a client cannot
	// recover from the raw string once it has been split.
	Terms  SearchTerms `json:"parsed"`
	Tools  bool        `json:"tools"`
	Hits   []SearchHit `json:"hits"`
	// Truncated says this endpoint found more than the limit and returned the
	// newest slice of it. It is what lets the console say "showing 50 of more"
	// instead of implying the list is complete.
	Truncated bool `json:"truncated,omitempty"`
	// Errors names the profile databases that could not be read. A search over
	// several profiles is partial evidence by nature, so a failure is reported
	// beside the rows rather than replacing them.
	Errors []string `json:"errors,omitempty"`
}

// handleSearch serves GET /search?q=...&tools=1&limit=50.
//
// q is the raw query (terms, quoting as described in ParseSearchTerms); tools=1
// adds assistant tool_calls to the searched text; limit caps the rows PER
// PROFILE DATABASE, defaulting to 50. Every profile database on the box is
// searched and their rows are merged, newest activity first.
func (r *Registry) handleSearch(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	terms := ParseSearchTerms(q.Get("q"))
	if len(terms.Terms) == 0 {
		writeErr(w, http.StatusBadRequest, "q is empty: give at least one term")
		return
	}
	withTools := q.Get("tools") == "1" || q.Get("tools") == "true"
	limit := 50
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "invalid limit: must be a positive integer")
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}

	ans := SearchAnswer{Terms: terms, Tools: withTools, Hits: []SearchHit{}}
	for _, p := range r.searchProfileDBs() {
		hits, err := SearchSessions(p.Path, p.Profile, terms, withTools, limit)
		if err != nil {
			// One unreadable profile must not hide another's rows.
			ans.Errors = append(ans.Errors, formatSearchError(p.Path, err))
			log.Printf("search: %s", formatSearchError(p.Path, err))
			continue
		}
		if len(hits) >= limit {
			ans.Truncated = true
		}
		ans.Hits = append(ans.Hits, hits...)
	}
	// The boxes hold several profiles; the merge keeps the newest first, which
	// is the order a reader scans a search in.
	sortHitsByRecency(ans.Hits)
	if len(ans.Hits) > limit {
		ans.Hits = ans.Hits[:limit]
		ans.Truncated = true
	}
	writeJSON(w, http.StatusOK, ans)
}

// sortHitsByRecency orders hits by last activity, newest first. Ties break by
// session id so the answer is stable between identical searches.
func sortHitsByRecency(hits []SearchHit) {
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0; j-- {
			a, b := hits[j-1], hits[j]
			if a.LastTS > b.LastTS || (a.LastTS == b.LastTS && a.SessionID <= b.SessionID) {
				break
			}
			hits[j-1], hits[j] = hits[j], hits[j-1]
		}
	}
}
