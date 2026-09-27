//go:build !windows

package main

// Unix ssh-client process discovery: ps output + /proc cwd resolution.

import (
	"os"
	"os/exec"
	"strings"
)

type procInfo struct {
	pid  string
	args string
	cwd  string // working directory, for resolving relative -i paths ("" if unknown)
}

func listSSHProcesses() []procInfo {
	out, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	var procs []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		m := psLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		cwd, _ := os.Readlink("/proc/" + m[1] + "/cwd")
		procs = append(procs, procInfo{pid: m[1], args: m[2], cwd: cwd})
	}
	return procs
}
