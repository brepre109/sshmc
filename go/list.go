package main

// Directory listing: local via os, remote by parsing `ls -lA` output.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Entry struct {
	Name     string
	IsDir    bool
	IsLink   bool
	Size     int64 // -1 for dirs/unknown
	MtimeStr string
}

func humanSize(n int64) string {
	if n < 0 {
		return "?"
	}
	f := float64(n)
	for _, unit := range []string{"B", "K", "M", "G", "T"} {
		if f < 1024 || unit == "T" {
			if unit == "B" {
				return fmt.Sprintf("%.0f%s", f, unit)
			}
			return fmt.Sprintf("%.1f%s", f, unit)
		}
		f /= 1024
	}
	return "?"
}

func sortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
}

func listLocal(path string) (string, []Entry, error) {
	if path == "" || path == "~" {
		path, _ = os.UserHomeDir()
	} else if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		real = filepath.Clean(path)
	}
	des, err := os.ReadDir(real)
	if err != nil {
		return "", nil, err
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		e := Entry{Name: de.Name(), Size: -1}
		if info, err := de.Info(); err == nil {
			e.IsLink = info.Mode()&os.ModeSymlink != 0
			e.MtimeStr = info.ModTime().Format("2006-01-02 15:04")
			if !de.IsDir() {
				e.Size = info.Size()
			}
		}
		e.IsDir = de.IsDir()
		if e.IsLink {
			if st, err := os.Stat(filepath.Join(real, de.Name())); err == nil && st.IsDir() {
				e.IsDir = true
				e.Size = -1
			}
		}
		entries = append(entries, e)
	}
	sortEntries(entries)
	return real, entries, nil
}

var (
	lsEpoch = regexp.MustCompile(`^([\-dlbcpsD])\S{8,}\s+\d+\s+\S+\s+\S+\s+(\d+)\s+(\d+)\s+(.+)$`)
	lsPlain = regexp.MustCompile(`^([\-dlbcpsD])\S{8,}\s+\d+\s+\S+\s+\S+\s+(\d+)\s+(\S+\s+\S+\s+\S+)\s+(.+)$`)
)

func listRemote(sess *Session, path string) (string, []Entry, error) {
	q := ""
	if path != "" {
		q = shQuote(path)
	}
	cmd := fmt.Sprintf("cd %s && pwd && (LC_ALL=C ls -lA --time-style=+%%s . 2>/dev/null || LC_ALL=C ls -lA .)", q)
	rc, out, errText := sess.SSHRun(cmd, 30*time.Second)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if rc != 0 && (len(lines) == 0 || lines[0] == "") {
		msg := strings.TrimSpace(errText)
		if msg == "" {
			msg = fmt.Sprintf("ssh exited %d", rc)
		}
		return "", nil, fmt.Errorf("%s", msg)
	}
	real := strings.TrimSpace(lines[0])
	var entries []Entry
	for _, line := range lines[1:] {
		if line == "" || strings.HasPrefix(line, "total") {
			continue
		}
		var kind, sizeStr, mtime, name string
		if m := lsEpoch.FindStringSubmatch(line); m != nil {
			kind, sizeStr, name = m[1], m[2], m[4]
			if epoch, err := strconv.ParseInt(m[3], 10, 64); err == nil {
				mtime = time.Unix(epoch, 0).Format("2006-01-02 15:04")
			}
		} else if m := lsPlain.FindStringSubmatch(line); m != nil {
			kind, sizeStr, mtime, name = m[1], m[2], m[3], m[4]
		} else {
			continue
		}
		e := Entry{Name: name, IsDir: kind == "d", IsLink: kind == "l", MtimeStr: mtime, Size: -1}
		if e.IsLink {
			if idx := strings.Index(e.Name, " -> "); idx >= 0 {
				e.Name = e.Name[:idx]
			}
		}
		if !e.IsDir {
			if n, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
				e.Size = n
			}
		}
		entries = append(entries, e)
	}
	sortEntries(entries)
	return real, entries, nil
}

// ListDir lists a directory on whatever the session points at.
func ListDir(sess *Session, path string) (string, []Entry, error) {
	if sess.IsLocal() {
		return listLocal(path)
	}
	return listRemote(sess, path)
}
