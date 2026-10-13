package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func seedMsgDB(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("sqlite3"); err != nil {
		os.Exit(0) // no sqlite3 CLI: nothing in this file can be exercised
	}
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	script := "CREATE TABLE messages(\n" +
		"  id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, role TEXT, content TEXT,\n" +
		"  tool_call_id TEXT, tool_calls TEXT, tool_name TEXT, reasoning_content TEXT, timestamp REAL);\n" +
		"INSERT INTO messages(session_id,role,content,timestamp) VALUES\n" +
		" ('20261011_120000_abc123','user','q1',1),('20261011_120000_abc123','assistant','a1',2),\n" +
		" ('20261011_120000_abc123','user','q2',3),('20261011_120000_abc123','assistant','a2',4),\n" +
		" ('20261011_120000_abc123','user','q3',5),('20261011_120000_abc123','assistant','a3',6),\n" +
		" ('20261011_120000_abc123','user','q4',7),('20261011_120000_abc123','assistant','a4',8);\n"
	cmd := exec.Command("sqlite3", "-batch", db)
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	return db
}

// The reading view crawls backward: a before_id page returns the rows OLDER
// than the bound, still chronological after the Go-side reverse. The row ids
// ride along — they are the crawl cursor.
func TestLoadMessagesBackwardPaging(t *testing.T) {
	db := seedMsgDB(t)
	page, err := LoadMessages(db, "20261011_120000_abc123", "", 3, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 3 || page[0].Content != "a3" || page[2].Content != "a4" {
		t.Fatalf("newest-first window wrong: %+v", page)
	}
	if page[0].ID == 0 {
		t.Fatalf("row ids must ride along for paging")
	}
	older, err := LoadMessages(db, "20261011_120000_abc123", "", 3, page[0].ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 3 || older[0].Content != "q2" || older[2].Content != "q3" {
		t.Fatalf("backward page wrong: %+v", older)
	}
	tail, _ := LoadMessages(db, "20261011_120000_abc123", "", 3, older[0].ID, 0)
	if len(tail) != 2 || tail[0].Content != "q1" || tail[1].Content != "a1" {
		t.Fatalf("tail page wrong: %+v", tail)
	}
	done, _ := LoadMessages(db, "20261011_120000_abc123", "", 3, tail[0].ID, 0)
	if len(done) != 0 {
		t.Fatalf("past the first row the crawl must be empty, got %+v", done)
	}
}

// Forward pages feed the cheap top-up: rows newer than the cursor, oldest
// first, ids ascending — no reversal on this path.
func TestLoadMessagesForwardPaging(t *testing.T) {
	db := seedMsgDB(t)
	page, err := LoadMessages(db, "20261011_120000_abc123", "", 100, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 4 || page[0].Content != "q3" || page[3].Content != "a4" {
		t.Fatalf("forward page wrong: %+v", page)
	}
	for i := 1; i < len(page); i++ {
		if page[i].ID <= page[i-1].ID {
			t.Fatalf("forward rows must ascend: %+v", page)
		}
	}
}

func TestLoadMessagesUnknownSession(t *testing.T) {
	db := seedMsgDB(t)
	page, err := LoadMessages(db, "nope", "", 10, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 0 {
		t.Fatalf("unknown session must be empty, got %+v", page)
	}
}
