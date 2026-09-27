package main

// SSH plumbing: sessions, ControlMaster multiplexing, live-session discovery.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

var controlDir = func() string {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".cache", "sshmc")
	os.MkdirAll(dir, 0o700)
	return dir
}()

// muxArgs are shared by every ssh/scp invocation so they reuse one
// authenticated master connection per (user, host, port). Windows OpenSSH
// does not support ControlMaster, so there each operation opens its own
// connection (use key/agent auth to avoid repeated prompts).
func muxArgs() []string {
	if runtime.GOOS == "windows" {
		return nil
	}
	return []string{
		"-o", "ControlPath=" + controlDir + "/cm-%C",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=600",
	}
}

func batchArgs() []string {
	return []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
}

// Session is a place a pane can point at: local FS or an ssh destination.
type Session struct {
	Host     string
	User     string
	Port     string
	Identity string
	Jump     string
	Extra    []string // extra ("-o", "...") pairs
	Source   string   // "local" | "pinned" | "live" | "config" | "manual"
}

func LocalSession() *Session { return &Session{Source: "local"} }

func (s *Session) IsLocal() bool { return s.Source == "local" }

func (s *Session) Target() string {
	if s.User != "" && !strings.Contains(s.Host, "@") {
		return s.User + "@" + s.Host
	}
	return s.Host
}

func (s *Session) Label() string {
	if s.IsLocal() {
		return "LOCAL"
	}
	label := s.Target()
	if s.Port != "" {
		label += ":" + s.Port
	}
	switch s.Source {
	case "pinned":
		label += " [pinned]"
	case "live":
		label += " [live]"
	case "config":
		label += " [cfg]"
	}
	return label
}

// SameHost reports whether two sessions address the same endpoint.
func (s *Session) SameHost(o *Session) bool {
	return s.Target() == o.Target() && s.Port == o.Port
}

func (s *Session) sshFlags() []string {
	var out []string
	if s.Port != "" {
		out = append(out, "-p", s.Port)
	}
	if s.Identity != "" {
		out = append(out, "-i", s.Identity)
	}
	if s.Jump != "" {
		out = append(out, "-J", s.Jump)
	}
	return append(out, s.Extra...)
}

func (s *Session) scpFlags() []string {
	var out []string
	if s.Port != "" {
		out = append(out, "-P", s.Port)
	}
	if s.Identity != "" {
		out = append(out, "-i", s.Identity)
	}
	if s.Jump != "" {
		out = append(out, "-J", s.Jump)
	}
	return append(out, s.Extra...)
}

// rshCommand builds the `-e` argument for rsync: the ssh command line that
// tunnels through our mux socket with this session's flags.
func (s *Session) rshCommand() string {
	parts := append([]string{"ssh"}, muxArgs()...)
	parts = append(parts, batchArgs()...)
	parts = append(parts, s.sshFlags()...)
	for i := range parts {
		parts[i] = shQuote(parts[i])
	}
	return strings.Join(parts, " ")
}

// SSHRun executes a shell command on the session's host through the mux.
func (s *Session) SSHRun(command string, timeout time.Duration) (int, string, string) {
	args := append(muxArgs(), batchArgs()...)
	args = append(args, s.sshFlags()...)
	args = append(args, s.Target(), "--", command)
	cmd := exec.Command("ssh", args...)
	var out, errb strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errb
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return 1, "", err.Error()
	}
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(timeout):
		cmd.Process.Kill()
		return 124, out.String(), "timeout after " + timeout.String()
	case err := <-done:
		rc := 0
		if ee, ok := err.(*exec.ExitError); ok {
			rc = ee.ExitCode()
		} else if err != nil {
			rc = 1
		}
		return rc, out.String(), errb.String()
	}
}

// SSHRunErr is SSHRun collapsed to (rc, best error text).
func (s *Session) SSHRunErr(command string, timeout time.Duration) (int, string) {
	rc, out, errText := s.SSHRun(command, timeout)
	msg := strings.TrimSpace(errText)
	if msg == "" {
		msg = strings.TrimSpace(out)
	}
	return rc, msg
}

// MasterAlive checks whether a mux master connection exists for the session.
// On Windows there is no mux; report true so no master setup is attempted.
func (s *Session) MasterAlive() bool {
	if s.IsLocal() || runtime.GOOS == "windows" {
		return true
	}
	args := append(muxArgs(), s.sshFlags()...)
	args = append(args, "-O", "check", s.Target())
	return exec.Command("ssh", args...).Run() == nil
}

// ConnectMaster establishes the master connection. Interactive: the caller
// must have released the terminal (tview Suspend) so auth prompts work.
func (s *Session) ConnectMaster() error {
	args := append(muxArgs(), "-fN")
	args = append(args, s.sshFlags()...)
	args = append(args, s.Target())
	cmd := exec.Command("ssh", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh master to %s failed: %w", s.Target(), err)
	}
	return nil
}

// ---------------------------------------------------------------- discovery

// single-letter ssh options that consume a value
const sshArgFlags = "bcDEeFIiJLlmOopQRSWw"

// parseSSHTokens turns `ssh ...` argv tokens (after the "ssh" word) into a
// Session. cwd resolves relative -i paths (from /proc/<pid>/cwd).
func parseSSHTokens(tokens []string, cwd, source string) *Session {
	s := &Session{Source: source}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t == "--" {
			continue
		}
		if strings.HasPrefix(t, "-") && len(t) >= 2 {
			flag := t[1]
			val := ""
			if strings.ContainsRune(sshArgFlags, rune(flag)) {
				if len(t) > 2 {
					val = t[2:]
				} else if i+1 < len(tokens) {
					i++
					val = tokens[i]
				}
			}
			switch flag {
			case 'p':
				s.Port = val
			case 'i':
				s.Identity = val
			case 'l':
				s.User = val
			case 'J':
				s.Jump = val
			case 'o':
				low := strings.ToLower(val)
				if !strings.HasPrefix(low, "batchmode") &&
					!strings.HasPrefix(low, "controlpath") &&
					!strings.HasPrefix(low, "controlmaster") &&
					!strings.HasPrefix(low, "controlpersist") {
					s.Extra = append(s.Extra, "-o", val)
				}
			case 'F':
				if val != "" {
					s.Extra = append(s.Extra, "-F", val)
				}
			}
			continue
		}
		s.Host = t // first non-flag word; ignore any remote command after it
		break
	}
	if s.Host == "" {
		return nil
	}
	if s.Identity != "" {
		id := s.Identity
		if strings.HasPrefix(id, "~") {
			home, _ := os.UserHomeDir()
			id = filepath.Join(home, strings.TrimPrefix(id, "~"))
		}
		if !filepath.IsAbs(id) && cwd != "" {
			id = filepath.Join(cwd, id)
		}
		s.Identity = id
	}
	return s
}

var psLine = regexp.MustCompile(`^\s*(\d+)\s+(.*)$`)

// DiscoverLiveSessions finds running ssh client processes on this machine.
// The process list comes from proc_unix.go / proc_windows.go.
func DiscoverLiveSessions() []*Session {
	var found []*Session
	seen := map[string]bool{}
	for _, p := range listSSHProcesses() {
		tokens := strings.Fields(p.args)
		if len(tokens) == 0 {
			continue
		}
		prog := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.Trim(tokens[0], `"`))), ".exe")
		if prog != "ssh" {
			continue // excludes sshd, ssh-agent, and mux masters ("ssh: ... [mux]")
		}
		s := parseSSHTokens(tokens[1:], p.cwd, "live")
		if s == nil {
			continue
		}
		key := s.Target() + ":" + s.Port
		if !seen[key] {
			seen[key] = true
			found = append(found, s)
		}
	}
	return found
}

var hostLine = regexp.MustCompile(`(?i)^\s*host\s+(.+?)\s*$`)

// ConfigHosts lists non-wildcard Host aliases from ~/.ssh/config.
func ConfigHosts() []*Session {
	home, _ := os.UserHomeDir()
	data, err := os.ReadFile(filepath.Join(home, ".ssh", "config"))
	if err != nil {
		return nil
	}
	var hosts []*Session
	seen := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		m := hostLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, alias := range strings.Fields(m[1]) {
			if strings.ContainsAny(alias, "*?!") || seen[alias] {
				continue
			}
			seen[alias] = true
			hosts = append(hosts, &Session{Host: alias, Source: "config"})
		}
	}
	return hosts
}

// shQuote quotes a string for a POSIX shell (remote side of ssh).
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	if regexp.MustCompile(`^[a-zA-Z0-9_./~+@:,-]+$`).MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
