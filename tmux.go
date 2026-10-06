package main

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// PaneID is tmux's unique pane id (%N).
type PaneID string

// Pane is one tmux pane as seen by tmux list-panes.
type Pane struct {
	ID        PaneID   `json:"pane_id"`
	TTY       string   `json:"tty"`
	Session   string   `json:"session"`
	Aliases   []string `json:"aliases,omitempty"` // all tmux session names sharing this pane (workspaces)
	WinIndex  string   `json:"win_index"`
	WinName   string   `json:"window_name"`
	PaneIndex int      `json:"pane_index"`
	Cmd       string   `json:"cmd"`
	Active    bool     `json:"active"`
	Dead      bool     `json:"dead"`
}

// runTmux runs tmux and returns stdout with stderr detail on error.
func runTmux(args ...string) (string, error) {
	cmd := exec.Command("tmux", args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("tmux %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// loadBufferStdin creates tmux buffer buf with the given content, delivered
// on the tmux command's stdin (load-buffer -b buf -). Stdin delivery avoids
// argv size limits and quoting/escaping problems entirely.
func loadBufferStdin(buf string, content string) error {
	cmd := exec.Command("tmux", "load-buffer", "-b", buf, "-")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdin = strings.NewReader(content)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux load-buffer -b %s - : %v: %s", buf, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// listPanes enumerates all panes of the tmux server, deduplicated by pane id.
func listPanes() (map[PaneID]Pane, error) {
	out, err := runTmux("list-panes", "-a", "-F",
		"#{pane_id}|#{pane_tty}|#{session_name}|#{window_index}|#{window_name}|#{pane_index}|#{pane_current_command}|#{pane_active}|#{pane_dead}")
	if err != nil {
		return nil, err
	}
	byID := map[PaneID]Pane{}
	aliases := map[PaneID]map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "|", 9)
		if len(f) != 9 {
			continue
		}
		idx, _ := strconv.Atoi(f[5])
		p := Pane{
			ID:        PaneID(f[0]),
			TTY:       f[1],
			Session:   f[2],
			WinIndex:  f[3],
			WinName:   f[4],
			PaneIndex: idx,
			Cmd:       f[6],
			Active:    f[7] == "1",
			Dead:      f[8] == "1",
		}
		if aliases[p.ID] == nil {
			aliases[p.ID] = map[string]bool{}
		}
		aliases[p.ID][p.Session] = true
		if _, seen := byID[p.ID]; !seen {
			byID[p.ID] = p
		}
	}
	// attach all tmux session aliases per pane (workspaces may join the same
	// pane into multiple sessions via link-windows/grouping)
	groupOut, gerr := runTmux("list-panes", "-a", "-F", "#{pane_id}|#{session_group}")
	glines := []string{}
	if gerr == nil {
		glines = strings.Split(groupOut, "\n")
	}
	groupOf := map[PaneID]string{}
	for _, line := range glines {
		f := strings.SplitN(line, "|", 2)
		if len(f) == 2 && f[0] != "" {
			groupOf[PaneID(f[0])] = f[1]
		}
	}
	sessOut, serr := runTmux("list-sessions", "-F", "#{session_name}|#{session_group}")
	if serr == nil {
		members := map[string][]string{} // group -> session names
		anon := map[string][]string{}    // group "" -> names
		for _, line := range strings.Split(sessOut, "\n") {
			f := strings.SplitN(line, "|", 2)
			if len(f) != 2 {
				continue
			}
			if f[1] == "" {
				anon[f[0]] = append(anon[f[0]], f[0])
				continue
			}
			members[f[1]] = append(members[f[1]], f[0])
		}
		for id, p := range byID {
			names := map[string]bool{}
			if g, ok := groupOf[id]; ok && g != "" {
				names[members[g][0]] = true
				for _, n := range members[g] {
					names[n] = true
				}
			}
			for n := range aliases[id] {
				names[n] = true
			}
			var al []string
			for n := range names {
				al = append(al, n)
			}
			sort.Strings(al)
			p.Aliases = al
			byID[id] = p
		}
	}
	return byID, nil
}
