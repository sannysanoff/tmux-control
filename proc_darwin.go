//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// scanHermesProcs finds hermes processes on macOS, where there is no /proc: one
// ps call lists every process with its controlling terminal, its state and its
// command line. Two normalisations are needed. ps prints the terminal without
// the /dev prefix ("ttys001") while tmux prints it with one ("/dev/ttys001"), so
// the tty is prefixed here; and a process with no controlling terminal ("??")
// cannot belong to a pane, so it is skipped.
func scanHermesProcs() []hermesProc {
	out, err := exec.Command("ps", "-eo", "tty=,pid=,stat=,command=").Output()
	if err != nil {
		return nil
	}
	var procs []hermesProc
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		if f[0] == "??" || f[0] == "" {
			continue
		}
		pid, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		cmdline := strings.Join(f[3:], " ")
		if !hermesProcRe.MatchString(cmdline) {
			continue
		}
		procs = append(procs, hermesProc{
			pid:       pid,
			tty:       "/dev/" + f[0],
			suspended: strings.Contains(f[2], "T"),
			cmdline:   cmdline,
			profile:   profileFor(pid, cmdline),
		})
	}
	return procs
}

// procStartTime reports when the process started, as a unix timestamp. There is
// no /proc, so it comes from ps's lstart column ("Tue Oct  6 19:30:48 2026").
func procStartTime(pid int) (float64, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return 0, fmt.Errorf("ps: no such process %d", pid)
	}
	t, err := time.Parse("Mon Jan _2 15:04:05 2006", s)
	if err != nil {
		return 0, err
	}
	return float64(t.Unix()), nil
}

// procAlive reports whether the pid still exists. Signal 0 is the portable check:
// ESRCH means gone, EPERM means it is there but owned by another user.
func procAlive(pid int, start float64) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// procEnvHermesHome reads HERMES_HOME out of a process's environment. There is no
// /proc to read, so it comes from ps -E, which appends the environment to the
// command line ("… ENV=val ENV2=val2"). A value is taken up to the next space,
// which is how every path this looks for is shaped.
func procEnvHermesHome(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-E", "-ww", "-o", "command=").Output()
	if err != nil {
		return ""
	}
	const key = "HERMES_HOME="
	i := strings.Index(string(out), key)
	if i < 0 {
		return ""
	}
	rest := string(out)[i+len(key):]
	if j := strings.IndexAny(rest, " \n	"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(rest)
}
