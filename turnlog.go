package main

import (
	"os"
	"strconv"
	"strings"
)

// TurnEventKind classifies one agent.log line relevant to turn state.
type TurnEventKind int

const (
	TurnNone TurnEventKind = iota
	TurnStarted
	TurnEnded
)

// TurnEvent is one parsed agent.log line.
type TurnEvent struct {
	Kind      TurnEventKind
	SessionID string
	TS        string
	Reason    string // for ended: text_response / interrupted_* / ...
	QueryHint string // truncated msg= payload from the turn-start line
}

var turnStartRe = `agent.turn_context: conversation turn: session=`
var turnEndRe = `agent.conversation_loop: Turn ended: reason=`

// parseTurnLine classifies a single log line.
func parseTurnLine(line string) TurnEvent {
	// format: 2026-10-05 12:56:09,473 INFO [session] logger: message
	istart := strings.Index(line, turnStartRe)
	iend := strings.Index(line, turnEndRe)
	if istart < 0 && iend < 0 {
		return TurnEvent{Kind: TurnNone}
	}
	// session id sits in square brackets after the level: "INFO [session]"
	lb := strings.Index(line, "] [")
	if lb < 0 {
		lb = strings.Index(line, "INFO [")
	}
	if lb < 0 {
		return TurnEvent{Kind: TurnNone}
	}
	lb = lb + len("INFO [")
	if lb > len(line) {
		return TurnEvent{Kind: TurnNone}
	}
	rest := line[lb:]
	brk := strings.Index(rest, "]")
	if brk < 0 {
		return TurnEvent{Kind: TurnNone}
	}
	sid := rest[:brk]
	if !sessionIDRe.MatchString(sid) {
		return TurnEvent{Kind: TurnNone}
	}
	ev := TurnEvent{SessionID: sid, TS: cutAt(line, 23)}
	if istart >= 0 && iend < 0 {
		ev.Kind = TurnStarted
		ev.QueryHint = extractMsg(line)
	} else if iend >= 0 && istart < 0 {
		ev.Kind = TurnEnded
		ev.Reason = extractReason(line)
	} else {
		// both: impossible in practice; treat as started
		ev.Kind = TurnStarted
		ev.QueryHint = extractMsg(line)
	}
	return ev
}

func cutAt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// extractMsg pulls the msg='...' or msg="..." payload (truncated in the log).
func extractMsg(line string) string {
	i := strings.Index(line, "msg=")
	if i < 0 {
		return ""
	}
	rest := line[i+4:]
	if rest == "" {
		return ""
	}
	q := rest[0]
	var end int
	for j := 1; j < len(rest); j++ {
		if rest[j] == q {
			end = j
			break
		}
		if rest[j] == '\n' {
			break
		}
	}
	if end == 0 {
		return ""
	}
	return rest[1:end]
}

// extractReason pulls reason=... from a turn-ended line.
func extractReason(line string) string {
	i := strings.Index(line, "reason=")
	if i < 0 {
		return ""
	}
	rest := line[i+len("reason="):]
	if j := strings.Index(rest, " model="); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}

// tailLines reads the last N lines of a file, returning them and the total
// line count (for offset tracking).
func tailLines(path string, n int) ([]string, int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	total := int64(len(lines))
	if int64(n) < total {
		lines = lines[total-int64(n):]
	}
	return lines, total, nil
}

var _ = strconv.Itoa
