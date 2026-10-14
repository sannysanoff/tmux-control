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
	write("tty-dev-pts-4", marker("20261001_104731_3064ef", 48*time.Hour)) // stale
	write("tmux_pane--0", marker("20261014_104731_3064ef", 0))             // name not a tty
	write("tty-dev-pts-5", "not json{")                                    // unparsable

	markers, skipped, err := LoadMarkersDetail(home)
	if err != nil {
		t.Fatalf("LoadMarkersDetail: %v", err)
	}
	if m, ok := markers["/dev/pts/3"]; !ok || m.SessionID != "20261014_104731_3064ef" {
		t.Fatalf("fresh marker missing: %+v", markers)
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
