package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadMarkersDetail covers the skip log: a good marker loads, and every
// kind of bad one — stale, wrong name shape, unparsable — is skipped with a
// reason instead of vanishing silently.
func TestLoadMarkersDetail(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "terminal-sessions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write := func(name, payload string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(payload), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	marker := func(sid string, age time.Duration) string {
		b, _ := json.Marshal(map[string]any{
			"session_id": sid, "cwd": "/tmp", "ts": float64(time.Now().Add(-age).Unix()),
		})
		return string(b)
	}
	write("tty-dev-pts-3", marker("20261014_104731_3064ef", 0))            // loads
	write("tty-dev-pts-4", marker("20261001_104731_3064ef", 48*time.Hour)) // stale, no owner
	write("tmux_pane--0", marker("20261014_104731_3064ef", 0))             // name not a tty
	write("tty-dev-pts-5", "not json{")                                    // unparsable
	write("tty-dev-pts-6", marker("20261022_082437_b37694", 26*time.Hour)) // stale but owned

	owner := func(tty string) (time.Time, bool) {
		if tty == "/dev/pts/6" {
			// the process on this tty started before the marker was written
			return time.Now().Add(-72 * time.Hour), true
		}
		if tty == "/dev/pts/4" {
			// the tty changed hands: this process is newer than the marker
			return time.Now().Add(-1 * time.Hour), true
		}
		return time.Time{}, false
	}

	markers, skipped, err := LoadMarkersDetail(home, owner)
	if err != nil {
		t.Fatalf("LoadMarkersDetail: %v", err)
	}
	if m, ok := markers["/dev/pts/3"]; !ok || m.SessionID != "20261014_104731_3064ef" {
		t.Fatalf("fresh marker missing: %+v", markers)
	}
	// an old marker whose writer still holds the tty is believed
	if m, ok := markers["/dev/pts/6"]; !ok || m.SessionID != "20261022_082437_b37694" {
		t.Fatalf("owned stale marker dropped: %+v", markers)
	}
	// ...and one whose tty was taken over by a newer process is not
	if _, ok := markers["/dev/pts/4"]; ok {
		t.Fatalf("stale marker on a reused tty was believed: %+v", markers)
	}
	if len(skipped) != 3 {
		t.Fatalf("want 3 skips, got %d: %v", len(skipped), skipped)
	}
	for _, want := range []string{"older than", "not a tty path", "unparsable"} {
		found := false
		for _, s := range skipped {
			if strings.Contains(s, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("skip reason %q not in %v", want, skipped)
		}
	}
}

// TestHermesProcRe covers the two shapes a hermes command line takes: the
// installed tree (a path with a trailing slash) and the macOS launcher, whose
// -c snippet names the tree inside quotes with no slash. The launcher form was
// the one that read as "hermes procs: 0" on a host where panes clearly ran it.
func TestHermesProcRe(t *testing.T) {
	yes := []string{
		`/Users/san/.hermes/hermes-agent/venv/bin/python -m hermes_cli.main`,
		`python3 -I -c sys.path.insert(0, '/Users/san/.hermes/hermes-agent'); import hermes_cli`,
		`python3 -c sys.path.insert(0,"/home/ubuntu/.hermes/hermes-agent")`,
	}
	for _, cmd := range yes {
		if !hermesProcRe.MatchString(cmd) {
			t.Errorf("not matched as hermes: %s", cmd)
		}
	}
	no := []string{
		`/usr/bin/vim /Users/san/.hermes/notes`,
		`/usr/bin/grep hermes-agent /tmp/x`,
	}
	for _, cmd := range no {
		if hermesProcRe.MatchString(cmd) {
			t.Errorf("matched as hermes: %s", cmd)
		}
	}
}
