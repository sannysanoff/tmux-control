package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// capturePane captures pane text as an array of lines. With escapes=true the
// capture carries the pane's rendered SGR attributes (`capture-pane -e`), so a
// console can paint the screen in color; trailing padding then stays as the
// terminal drew it (a full-width status bar keeps its background), and the
// caller strips the codes for any byte-identity work.
func capturePane(id PaneID, max int, escapes bool) ([]string, error) {
	args := []string{"capture-pane", "-p"}
	if escapes {
		args = append(args, "-e")
	}
	args = append(args, "-t", string(id), "-S", fmt.Sprintf("-%d", max))
	out, err := runTmux(args...)
	if err != nil {
		return nil, err
	}
	all := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	lines := make([]string, 0, len(all))
	for _, l := range all {
		if !escapes {
			l = strings.TrimRight(l, " 	")
		}
		lines = append(lines, l)
	}
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	return lines, nil
}

// captureViewport captures only the visible viewport of a pane (no history),
// so line indexes match tmux cursor_y.
func captureViewport(id string) ([]string, error) {
	out, err := runTmux("capture-pane", "-p", "-t", id)
	if err != nil {
		return nil, err
	}
	all := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	lines := make([]string, 0, len(all))
	for _, l := range all {
		lines = append(lines, strings.TrimRight(l, " \t"))
	}
	if len(lines) == 1 && lines[0] == "" {
		return []string{}, nil
	}
	return lines, nil
}

// paneCursor reads the pane's cursor position and pane height the way
// focusCheck does. Errors answer (-1, -1, -1): the console treats that as "no
// cursor known" and draws nothing, so an old tmux or a race with pane death
// never breaks the poll. pane_height rides along because cursor_y is
// viewport-relative while a capture-pane answer includes history: the console
// maps cursor_y into the capture as len(lines) - pane_height + cursor_y (the
// capture's last line is always the viewport's last line).
func paneCursor(id PaneID) (int, int, int) {
	dm, err := runTmux("display-message", "-p", "-t", string(id), "#{cursor_y}|#{cursor_x}|#{pane_height}")
	if err != nil {
		return -1, -1, -1
	}
	f := strings.Split(strings.TrimSpace(dm), "|")
	if len(f) != 3 {
		return -1, -1, -1
	}
	cy, _ := strconv.Atoi(f[0])
	cx, _ := strconv.Atoi(f[1])
	ph, _ := strconv.Atoi(f[2])
	if cy < 0 || cx < 0 || ph <= 0 {
		return -1, -1, -1
	}
	return cx, cy, ph
}

// sendText types text into the pane and optionally presses Enter.
// Focus check is deterministic geometry: the pane line at cursor_y must
// contain the hermes input glyph "❯" (the CLI input row).
// enterDelayMs pauses between typing and Enter (prompt_toolkit ingest);
// 0 means registry default (set by caller).
var enterDelayMs int64 = 500

// focusInfo is the outcome of the focus check shared by /send and /paste.
type focusInfo struct {
	X, Y    int
	Line    string
	Focused bool
}

// focusCheck is the deterministic focus check /send and /paste share: the
// pane line at tmux cursor_y must contain the hermes input glyph "❯" (the
// CLI input row). A non-nil error is a tmux/geometry failure;
// focused=false means the pane is not on its input row (streaming a
// response, or the user is scrolled into history).
func focusCheck(id string) (focusInfo, error) {
	dm, err := runTmux("display-message", "-p", "-t", id, "#{cursor_y}|#{cursor_x}")
	if err != nil {
		return focusInfo{}, err
	}
	f := strings.Split(strings.TrimSpace(dm), "|")
	if len(f) != 2 {
		return focusInfo{}, fmt.Errorf("bad display-message output: %q", dm)
	}
	var fi focusInfo
	fi.Y, _ = strconv.Atoi(f[0])
	fi.X, _ = strconv.Atoi(f[1])
	lines, err := captureViewport(id)
	if err != nil {
		return focusInfo{}, err
	}
	if fi.Y < 0 || fi.Y >= len(lines) {
		return focusInfo{}, fmt.Errorf("cursor_y=%d outside viewport (%d lines)", fi.Y, len(lines))
	}
	fi.Line = lines[fi.Y]
	fi.Focused = strings.Contains(fi.Line, "❯")
	return fi, nil
}

func sendText(id string, text string, enter bool) (map[string]any, error) {
	fi, err := focusCheck(id)
	if err != nil {
		return nil, err
	}
	if !fi.Focused {
		return nil, fmt.Errorf("pane not focused for input: cursor_y=%d line=%q", fi.Y, truncStr(fi.Line, 60))
	}
	if _, err := runTmux("send-keys", "-t", id, "-l", text); err != nil {
		return nil, err
	}
	time.Sleep(time.Duration(enterDelayMs) * time.Millisecond)
	if enter {
		if _, err := runTmux("send-keys", "-t", id, "Enter"); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"ok":        true,
		"action":    "send",
		"pane_id":   id,
		"focused":   fi.Focused,
		"input_row": fi.Y,
		"cursor":    fmt.Sprintf("%d,%d", fi.X, fi.Y),
	}, nil
}

// keyTokenRe accepts tmux key names (Enter, BSpace, C-c, F5, M-x, PPage) and
// single characters. /keys types raw keystrokes into a pane WITHOUT the input
// focus check — that is its whole point (a permission dialog is not the "❯"
// input row) — so it must not forward arbitrary option-shaped strings.
var keyTokenRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+-]{0,11}$`)

func validKeyToken(tok string) bool {
	if keyTokenRe.MatchString(tok) {
		return true
	}
	return utf8.RuneCountInString(tok) == 1
}

// sendKeys delivers raw keystrokes to the pane like a real terminal: literal
// text (sent first, via send-keys -l so it can never be reinterpreted as key
// names) followed by named keys in one send-keys call. There is deliberately
// NO focusCheck here: /send and /paste refuse when the cursor is not on the
// input row, and a one-key dialog ("1 allow, 2 deny") never is. No enter
// delay either — the caller presses Enter as its own key when it means it.
func sendKeys(id string, text string, keys []string) (map[string]any, error) {
	if len(text) > 4096 {
		return nil, fmt.Errorf("text too long (%d bytes, max 4096)", len(text))
	}
	if len(keys) > 64 {
		return nil, fmt.Errorf("too many keys (%d, max 64)", len(keys))
	}
	for _, k := range keys {
		if !validKeyToken(k) {
			return nil, fmt.Errorf("bad key token %q", k)
		}
	}
	if text != "" {
		if _, err := runTmux("send-keys", "-t", id, "-l", text); err != nil {
			return nil, err
		}
	}
	if len(keys) > 0 {
		args := append([]string{"send-keys", "-t", id}, keys...)
		if _, err := runTmux(args...); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"ok":      true,
		"action":  "keys",
		"pane_id": id,
		"bytes":   len(text),
		"keys":    len(keys),
	}, nil
}

// pasteReqID hands every paste request a unique tmux buffer name, so
// concurrent pastes cannot collide or leak each other's buffers.
var pasteReqID atomic.Int64

// pasteText pastes text into the pane as a bracketed paste, so embedded
// newlines are inserted literally instead of submitting a turn per line.
// The text is delivered on stdin to tmux load-buffer under a unique
// per-request buffer name (no argv limits, no quoting problems), applied
// with tmux paste-buffer -p, and the buffer is deleted afterwards. Enter
// is optionally pressed after the same enter-delay pause /send uses.
// Focus check is the same deterministic geometry /send performs.
func pasteText(id string, text string, enter bool) (map[string]any, error) {
	fi, err := focusCheck(id)
	if err != nil {
		return nil, err
	}
	if !fi.Focused {
		return nil, fmt.Errorf("pane not focused for input: cursor_y=%d line=%q", fi.Y, truncStr(fi.Line, 60))
	}
	buf := fmt.Sprintf("tmuxctl-paste-%d", pasteReqID.Add(1))
	if err := loadBufferStdin(buf, text); err != nil {
		return nil, err
	}
	if _, err := runTmux("paste-buffer", "-p", "-b", buf, "-t", id); err != nil {
		if _, delErr := runTmux("delete-buffer", "-b", buf); delErr != nil {
			return nil, fmt.Errorf("%v (buffer cleanup: %v)", err, delErr)
		}
		return nil, err
	}
	if _, err := runTmux("delete-buffer", "-b", buf); err != nil {
		return nil, fmt.Errorf("delete-buffer: %v", err)
	}
	time.Sleep(time.Duration(enterDelayMs) * time.Millisecond)
	if enter {
		if _, err := runTmux("send-keys", "-t", id, "Enter"); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"ok":        true,
		"action":    "paste",
		"pane_id":   id,
		"focused":   fi.Focused,
		"input_row": fi.Y,
		"cursor":    fmt.Sprintf("%d,%d", fi.X, fi.Y),
		"bytes":     len(text),
	}, nil
}
