package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testSID = "20261006_200628_6e4365"

// newFixtureDB builds a minimal hermes-shaped messages table. Only the columns
// LoadEvents reads are present, which is the point: the query must not depend on
// anything else in the real schema.
func newFixtureDB(t *testing.T) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "state.db")
	script := `
CREATE TABLE messages(
  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, role TEXT, content TEXT,
  tool_call_id TEXT, tool_calls TEXT, tool_name TEXT, timestamp REAL);
INSERT INTO messages(session_id,role,content,tool_calls,tool_name,timestamp) VALUES
 ('` + testSID + `','user','hello there','','',1000000000),
 ('` + testSID + `','assistant','','[{"type":"function","function":{"name":"patch","arguments":"{\"path\":\"/a/b.go\"}"}}]','',1000000060),
 ('` + testSID + `','tool','OUTPUT-LONG-AND-LONGER','','patch',1000000120),
 ('20261006_000000_aaaaaa','user','other session','','',1000000120);
`
	cmd := exec.Command("sqlite3", "-batch", db)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	return db
}

func TestLoadEventsClipsAndPages(t *testing.T) {
	db := newFixtureDB(t)

	rows, err := LoadEvents(db, testSID, 0, 10, 8)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3 (the other session must not leak in)", len(rows))
	}
	if rows[0].Role != "user" || rows[0].Content != "hello there" {
		t.Fatalf("first row = %+v", rows[0])
	}
	// user/assistant text is not clipped at maxChars...
	if rows[1].Role != "assistant" || rows[1].Content != "" {
		t.Fatalf("assistant row = %+v", rows[1])
	}
	if !strings.HasPrefix(rows[1].ToolCalls, "[{\"type\"") {
		t.Fatalf("tool_calls = %q", rows[1].ToolCalls)
	}
	if !strings.HasSuffix(rows[1].ToolCalls, "…") {
		t.Fatalf("tool_calls should be clipped at 8 bytes: %q", rows[1].ToolCalls)
	}
	// ...but a tool row's output is.
	if rows[2].ToolName != "patch" {
		t.Fatalf("tool_name = %q", rows[2].ToolName)
	}
	if rows[2].Content != "OUTPUT-L…" {
		t.Fatalf("tool content = %q, want the 8-byte clip", rows[2].Content)
	}
	if rows[2].TS == "" {
		t.Fatal("ts was not rendered")
	}
	// ids ascending, so a caller can take the last one as its cursor
	if !(rows[0].ID < rows[1].ID && rows[1].ID < rows[2].ID) {
		t.Fatalf("ids not ascending: %d %d %d", rows[0].ID, rows[1].ID, rows[2].ID)
	}
}

func TestLoadEventsIncremental(t *testing.T) {
	db := newFixtureDB(t)
	all, err := LoadEvents(db, testSID, 0, 10, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	// Cursor at the first row: exactly the other two come back, in order.
	rest, err := LoadEvents(db, testSID, all[0].ID, 10, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(rest) != 2 || rest[0].ID != all[1].ID || rest[1].ID != all[2].ID {
		t.Fatalf("incremental page = %+v", rest)
	}
	// Caught up: an empty page, not an error.
	none, err := LoadEvents(db, testSID, all[len(all)-1].ID, 10, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("caught-up page = %+v", none)
	}
	// A bad session id is a refusal to query, not a query.
	if rows, err := LoadEvents(db, "../../etc/passwd", 0, 10, 0); err != nil || len(rows) != 0 {
		t.Fatalf("bad session id: rows=%v err=%v", rows, err)
	}
}

func TestLoadEventsLimit(t *testing.T) {
	db := newFixtureDB(t)
	rows, err := LoadEvents(db, testSID, 0, 2, 0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit ignored: %d rows", len(rows))
	}
}

func TestClip(t *testing.T) {
	if got := clip("abcdef", 0); got != "abcdef" {
		t.Fatalf("n<=0 must not clip: %q", got)
	}
	if got := clip("abcdef", 3); got != "abc…" {
		t.Fatalf("clip = %q", got)
	}
	// a cut inside a multi-byte rune backs off to a boundary: 3 bytes lands
	// inside "р", so the clip keeps only "п"
	if got := clip("привет", 3); got != "п…" {
		t.Fatalf("clip on a rune boundary = %q", got)
	}
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("sqlite3"); err != nil {
		os.Exit(0) // no sqlite3 CLI: nothing in this file can be exercised
	}
	os.Exit(m.Run())
}
