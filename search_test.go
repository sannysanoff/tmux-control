package main

import (
	"os"
	"testing"
)

func TestParseSearchTerms(t *testing.T) {
	cases := []struct {
		in      string
		terms   []string
		phrases []string
	}{
		{"SLE-34856", []string{"sle-34856"}, nil},
		{"tmux screen", []string{"tmux", "screen"}, nil},
		{`"header was rendered in compact manner"`, []string{"header was rendered in compact manner"}, []string{"header was rendered in compact manner"}},
		{`foo "bar baz" qux`, []string{"foo", "bar baz", "qux"}, []string{"bar baz"}},
		{`  `, nil, nil},
		{`"unterminated phrase`, []string{"unterminated phrase"}, []string{"unterminated phrase"}},
		{`a A a`, []string{"a"}, nil},
	}
	for _, c := range cases {
		got := ParseSearchTerms(c.in)
		if len(got.Terms) != len(c.terms) {
			t.Fatalf("%q: terms = %v, want %v", c.in, got.Terms, c.terms)
		}
		for i := range c.terms {
			if got.Terms[i] != c.terms[i] {
				t.Fatalf("%q: terms = %v, want %v", c.in, got.Terms, c.terms)
			}
		}
		if len(got.Phrases) != len(c.phrases) {
			t.Fatalf("%q: phrases = %v, want %v", c.in, got.Phrases, c.phrases)
		}
	}
}

func TestSQLQuoteEscapes(t *testing.T) {
	if got := sqlQuote("it's"); got != "'it''s'" {
		t.Fatalf("sqlQuote = %s", got)
	}
}

// TestSearchSessionsLive runs against the real profile database when it is
// present; it is a probe, not a fixture, and skips when the box has none.
func TestSearchSessionsLive(t *testing.T) {
	db := os.Getenv("SEARCH_TEST_DB")
	if db == "" {
		t.Skip("SEARCH_TEST_DB not set")
	}
	terms := ParseSearchTerms("tmux")
	hits, err := SearchSessions(db, "rtassist", terms, false, 50)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	t.Logf("%d hits, newest %s score %d hits %d steps %d", len(hits),
		hits[0].SessionID, hits[0].Score, hits[0].Hits, hits[0].Steps)
	if len(hits) == 0 {
		t.Fatalf("expected hits for tmux")
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].LastTS < hits[i].LastTS {
			t.Fatalf("not newest-first at %d", i)
		}
	}
	if hits[0].FirstQuery == "" {
		t.Fatalf("no first query on %s", hits[0].SessionID)
	}
}