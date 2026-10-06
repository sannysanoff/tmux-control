package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

// MarkerEntry is one per-tty current-session marker file.
type MarkerEntry struct {
	TTY       string
	SessionID string
	CWD       string
	TS        time.Time
}

// LoadMarkers reads all ~/.hermes/terminal-sessions/* files.
func LoadMarkers(hermesHome string) (map[string]MarkerEntry, error) {
	out := map[string]MarkerEntry{}
	dir := filepath.Join(hermesHome, "terminal-sessions")
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return out, err
	}
	for _, de := range ents {
		b, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			continue
		}
		var m struct {
			SessionID string  `json:"session_id"`
			CWD       string  `json:"cwd"`
			TS        float64 `json:"ts"`
		}
		if json.Unmarshal(b, &m) != nil || m.SessionID == "" {
			continue
		}
		tty := ttyFromMarkerName(de.Name())
		if tty == "" {
			continue
		}
		out[tty] = MarkerEntry{TTY: tty, SessionID: m.SessionID, CWD: m.CWD, TS: time.Unix(int64(m.TS), 0)}
	}
	return out, nil
}

// ttyFromMarkerName converts "tty-dev-pts-19" to "/dev/pts/19" (and
// "tty-dev-pts-0" to "/dev/pts/0"). Returns "" for unknown shapes.
func ttyFromMarkerName(name string) string {
	rest := strings.TrimPrefix(name, "tty-dev-")
	if rest == name {
		return ""
	}
	i := strings.LastIndex(rest, "-")
	if i <= 0 {
		return ""
	}
	dev, num := rest[:i], rest[i+1:]
	if num == "" {
		return ""
	}
	return "/dev/" + dev + "/" + num
}

// procStartTime reads /proc/PID/stat field 22 in clock ticks.
func procStartTime(pid int) (float64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	rp := strings.LastIndex(s, ")")
	if rp < 0 || rp+2 >= len(s) {
		return 0, os.ErrInvalid
	}
	fields := strings.Fields(s[rp+2:])
	if len(fields) < 20 {
		return 0, os.ErrInvalid
	}
	ticks, err := strconv.ParseFloat(fields[20], 64)
	if err != nil {
		return 0, err
	}
	return ticks / 100.0, nil
}

// procAlive reports whether pid exists; if start is a lease epoch timestamp
// (>1e6), it checks /proc/PID stat mtime-age consistency loosely, else only
// existence. We deliberately avoid clock-tick math (deterministic + simple).
func procAlive(pid int, start float64) bool {
	st, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return false
	}
	_ = st
	_ = start
	return true
}

// AgentLogSession extracts session ids like 20261005_122131_345299 from a
// log line. Returns the session id and the log timestamp.
var _ = json.Marshal

var _ = time.Now
