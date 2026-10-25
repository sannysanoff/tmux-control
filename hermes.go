package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LeaseEntry is one record of ~/.hermes/runtime/active_sessions.json.
type LeaseEntry struct {
	LeaseID          string  `json:"lease_id"`
	PID              int     `json:"pid"`
	ProcessStartTime float64 `json:"process_start_time"`
	SessionID        string  `json:"session_id"`
	LiveSessionID    string  `json:"live_session_id"`
	Surface          string  `json:"surface"`
}

type leaseFile struct {
	Entries []LeaseEntry `json:"entries"`
}

// LoadLeases parses the pid-keyed hermes lease file.
func LoadLeases(hermesHome string) (map[int]LeaseEntry, error) {
	out := map[int]LeaseEntry{}
	b, err := os.ReadFile(filepath.Join(hermesHome, "runtime", "active_sessions.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	var lf leaseFile
	if err := json.Unmarshal(b, &lf); err != nil {
		return out, err
	}
	for _, e := range lf.Entries {
		out[e.PID] = e
	}
	return out, nil
}

// MarkerEntry is one per-tty current-session marker file. The marker names the
// session that a terminal is currently running, so a stale one must not label a
// recycled tty with somebody else's conversation. Age alone is not enough to
// decide that: hermes rewrites the marker only on process start, /new, a
// compression rotation and CLI close, so a REPL that has been running for days
// keeps a marker older than any age bound while still owning its tty.
type MarkerEntry struct {
	TTY       string
	SessionID string
	CWD       string
	TS        time.Time
}

// markerMaxAge bounds how old a marker may be and still be believed on its own.
const markerMaxAge = 24 * time.Hour

// markerOwnerSlack absorbs the microseconds between a process being created and
// its marker being written: the lease records the kernel start time and the
// marker the moment hermes got around to the write.
const markerOwnerSlack = 5 * time.Minute

// MarkerOwner answers when the hermes process currently occupying a tty was
// created. The second result is false when nothing is known about that tty.
type MarkerOwner func(tty string) (time.Time, bool)

// LoadMarkers parses the terminal-sessions directory into tty-keyed markers.
func LoadMarkers(hermesHome string, owner MarkerOwner) (map[string]MarkerEntry, error) {
	out, _, err := LoadMarkersDetail(hermesHome, owner)
	return out, err
}

// LoadMarkersDetail is LoadMarkers plus the skip log: every marker file left
// out, with the reason (unreadable, unparsable, name not a tty, too old). The
// startup dump logs these, so a terminal that reads as unattached carries its
// why in the log.
//
// A marker older than markerMaxAge survives when the process on its tty started
// BEFORE the marker was written: then the writer is still there and the marker
// describes it, whatever its age. When the process started after the marker, the
// tty changed hands and the marker names a conversation that is gone — that is
// the case the age bound exists for. owner may be nil (no process knowledge),
// and the age bound then applies to every marker.
func LoadMarkersDetail(hermesHome string, owner MarkerOwner) (map[string]MarkerEntry, []string, error) {
	out := map[string]MarkerEntry{}
	skipped := []string{}
	dir := filepath.Join(hermesHome, "terminal-sessions")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, skipped, nil
		}
		return out, skipped, err
	}
	for _, de := range ents {
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			skipped = append(skipped, de.Name()+": read: "+err.Error())
			continue
		}
		var m struct {
			SessionID string  `json:"session_id"`
			CWD       string  `json:"cwd"`
			TS        float64 `json:"ts"`
		}
		if json.Unmarshal(b, &m) != nil || m.SessionID == "" {
			skipped = append(skipped, de.Name()+": unparsable or empty session_id")
			continue
		}
		tty := ttyFromMarkerName(de.Name())
		if tty == "" {
			skipped = append(skipped, de.Name()+": name is not a tty path")
			continue
		}
		ts := time.Unix(int64(m.TS), 0)
		if !ts.IsZero() && time.Since(ts) > markerMaxAge {
			// The age bound exists for a recycled tty: a marker left by a hermes
			// that has since exited would otherwise name somebody else's
			// conversation. If the hermes on this tty started BEFORE the marker
			// was written, the writer is still in the pane and the marker
			// describes it — believe it whatever its age.
			owned := false
			if owner != nil {
				if started, ok := owner(tty); ok && started.Before(ts.Add(markerOwnerSlack)) {
					owned = true
				}
			}
			if !owned {
				skipped = append(skipped, fmt.Sprintf("%s: ts %s older than %s and no older process holds the tty",
					de.Name(), ts.Format(time.RFC3339), markerMaxAge))
				continue
			}
		}
		out[tty] = MarkerEntry{TTY: tty, SessionID: m.SessionID, CWD: m.CWD, TS: ts}
	}
	return out, skipped, nil
}

// ttyFromMarkerName turns a marker file name into the device path tmux reports,
// on both platforms: "tty-dev-pts-19" -> "/dev/pts/19" (Linux) and
// "tty-dev-ttys009" -> "/dev/ttys009" (macOS). Returns "" for unknown shapes.
func ttyFromMarkerName(name string) string {
	rest := strings.TrimPrefix(name, "tty-dev-")
	if rest == name || rest == "" {
		return ""
	}
	// Linux: <dev>-<num>, where the device has no dash of its own.
	if i := strings.LastIndex(rest, "-"); i > 0 {
		dev, num := rest[:i], rest[i+1:]
		if !strings.Contains(dev, "-") && digitsOnly(num) {
			return "/dev/" + dev + "/" + num
		}
	}
	// macOS: the tty name has no number suffix split by a dash.
	if strings.HasPrefix(rest, "tty") {
		return "/dev/" + rest
	}
	return ""
}

func digitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// profileFor names the hermes profile a process runs under: the flag it was
// started with if the command line carries one, otherwise the profile that its
// HERMES_HOME points into. Empty means the default profile. The flag is preferred
// because it survives process listings that hide the environment, and because
// the shape there is a Python argv literal — ['hermes', '-p', 'rtassist'] — the
// command line is tokenised on quotes and commas rather than searched for " -p ".
func profileFor(pid int, cmdline string) string {
	if p := profileFromCmdline(cmdline); p != "" {
		return p
	}
	return profileFromHermesHome(procEnvHermesHome(pid))
}

func profileFromCmdline(cmdline string) string {
	toks := strings.FieldsFunc(cmdline, func(r rune) bool {
		switch r {
		case ' ', '	', '\n', '\'', '"', ',', '[', ']':
			return true
		}
		return false
	})
	for i, t := range toks {
		if t != "-p" && t != "--profile" {
			continue
		}
		if i+1 >= len(toks) {
			return ""
		}
		if name := strings.TrimSpace(toks[i+1]); name != "" && !strings.HasPrefix(name, "-") {
			return name
		}
	}
	return ""
}

// profileFromHermesHome reads the profile out of a HERMES_HOME value of the shape
// <home>/.hermes/profiles/<name>. Empty when it points at the default home.
func profileFromHermesHome(home string) string {
	home = strings.TrimSpace(home)
	if home == "" {
		return ""
	}
	parts := strings.Split(filepath.Clean(home), string(filepath.Separator))
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "profiles" {
			return parts[i+1]
		}
	}
	return ""
}
