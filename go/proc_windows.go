//go:build windows

package main

// Windows ssh-client process discovery via a PowerShell CIM query.
// (Windows 10/11 ship the OpenSSH client as ssh.exe.)

import (
	"os/exec"
	"strings"
)

type procInfo struct {
	pid  string
	args string
	cwd  string // not resolvable on Windows; relative -i paths stay relative
}

func listSSHProcesses() []procInfo {
	out, err := exec.Command("powershell.exe", "-NoProfile", "-Command",
		`Get-CimInstance Win32_Process -Filter "Name='ssh.exe'" | ForEach-Object { "$($_.ProcessId) $($_.CommandLine)" }`,
	).Output()
	if err != nil {
		return nil
	}
	var procs []procInfo
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimRight(line, "\r")
		m := psLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		procs = append(procs, procInfo{pid: m[1], args: m[2]})
	}
	return procs
}
