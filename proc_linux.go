//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// scanHermesProcs finds hermes processes through /proc: deterministic, with no
// external command. A process counts when its cmdline names the hermes-agent
// path; its controlling terminal comes from /proc/PID/fd/0.
func scanHermesProcs() []hermesProc {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []hermesProc
	for _, de := range ents {
		pid, err := strconv.Atoi(de.Name())
		if err != nil {
			continue
		}
		cl, err := os.ReadFile("/proc/" + de.Name() + "/cmdline")
		if err != nil {
			continue
		}
		cmdline := strings.ReplaceAll(string(cl), "\x00", " ")
		if !hermesProcRe.MatchString(cmdline) {
			continue
		}
		hp := hermesProc{pid: pid, cmdline: strings.TrimSpace(cmdline)}
		hp.profile = profileFor(pid, hp.cmdline)
		if st, err := os.ReadFile("/proc/" + de.Name() + "/stat"); err == nil {
			f := strings.Fields(string(st))
			if len(f) > 2 && f[2] == "T" {
				hp.suspended = true
			}
		}
		if fd, err := os.Readlink("/proc/" + de.Name() + "/fd/0"); err == nil && strings.HasPrefix(fd, "/dev/pts/") {
			hp.tty = fd
		}
		out = append(out, hp)
	}
	return out
}

// procStartTime reports when the process started, as a unix timestamp. /proc
// gives starttime in clock ticks since boot (field 22, i.e. the 20th field after
// the parenthesised comm), so it is anchored to boot time from /proc/stat. The
// tick rate is taken as 100 Hz, which is what Linux uses on every architecture
// this daemon runs on; getting it wrong shifts the answer by a constant factor,
// which only matters for a marker written within minutes of the process starting.
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
	ticks, err := strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return 0, err
	}
	btime, err := bootTime()
	if err != nil {
		return 0, err
	}
	return btime + ticks/100.0, nil
}

// bootTime reads the system boot timestamp from /proc/stat's btime line.
func bootTime() (float64, error) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "btime ") {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 64)
			if err != nil {
				return 0, err
			}
			return v, nil
		}
	}
	return 0, os.ErrNotExist
}

// procAlive reports whether the pid directory still exists. The start time is
// not used: existence is the deterministic signal, and clock-tick math to catch
// pid reuse is deliberately avoided.
func procAlive(pid int, start float64) bool {
	if pid <= 0 {
		return false
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err != nil {
		return false
	}
	return true
}

// procEnvHermesHome reads HERMES_HOME out of a process's environment.
func procEnvHermesHome(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/environ")
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(string(b), "\x00") {
		if strings.HasPrefix(kv, "HERMES_HOME=") {
			return strings.TrimPrefix(kv, "HERMES_HOME=")
		}
	}
	return ""
}
