package main

// Integration tests for the non-UI layer. The remote tests need a live,
// key-authenticated ssh session discoverable on this machine; they skip
// otherwise. Run: go test -v ./...

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSSHTokens(t *testing.T) {
	s := parseSSHTokens(strings.Fields("-p 2222 -i key.pem -l bob -o StrictHostKeyChecking=no myhost uptime"), "/tmp", "manual")
	if s == nil || s.Target() != "bob@myhost" || s.Port != "2222" || s.Identity != "/tmp/key.pem" {
		t.Fatalf("bad parse: %+v", s)
	}
	if len(s.Extra) != 2 || s.Extra[1] != "StrictHostKeyChecking=no" {
		t.Fatalf("extra not kept: %+v", s.Extra)
	}
}

func TestRSHCommandQuotesCredentialPaths(t *testing.T) {
	s := &Session{
		Host:     "example.com",
		Identity: `C:\Users\Test User\identity.pem`,
		Extra:    []string{"-o", `UserKnownHostsFile=C:\Users\Test User\known_hosts`},
		Source:   "pinned",
	}
	command := s.rshCommand()
	if !strings.Contains(command, `'C:\Users\Test User\identity.pem'`) ||
		!strings.Contains(command, `'UserKnownHostsFile=C:\Users\Test User\known_hosts'`) {
		t.Fatalf("credential paths are not shell-quoted: %s", command)
	}
}

func TestLsParsing(t *testing.T) {
	if m := lsEpoch.FindStringSubmatch("drwxrwxr-x 2 u u 4096 1720900000 my dir"); m == nil || m[4] != "my dir" {
		t.Fatalf("epoch dir parse failed: %v", m)
	}
	if m := lsEpoch.FindStringSubmatch("lrwxrwxrwx 1 u u 7 1720900002 link -> /target"); m == nil {
		t.Fatal("symlink parse failed")
	}
	if m := lsPlain.FindStringSubmatch("-rw-r--r--  1 root  wheel  1024 Jul 10 09:00 notes.txt"); m == nil || m[4] != "notes.txt" {
		t.Fatalf("plain parse failed: %v", m)
	}
}

func TestListLocal(t *testing.T) {
	real, entries, err := listLocal("~")
	if err != nil || real == "" || len(entries) == 0 {
		t.Fatalf("listLocal: %v %q %d", err, real, len(entries))
	}
}

// requireLiveTests gates integration tests that attach to ssh sessions
// already running on this machine. Opt in with SSHMC_LIVE_TESTS=1.
func requireLiveTests(t *testing.T) {
	t.Helper()
	if os.Getenv("SSHMC_LIVE_TESTS") != "1" {
		t.Skip("set SSHMC_LIVE_TESTS=1 to run tests against live ssh sessions")
	}
}

func liveSession(t *testing.T) *Session {
	t.Helper()
	requireLiveTests(t)
	live := DiscoverLiveSessions()
	if len(live) == 0 {
		t.Skip("no live ssh session on this machine")
	}
	s := live[0]
	if !s.MasterAlive() {
		if err := s.ConnectMaster(); err != nil || !s.MasterAlive() {
			t.Skipf("cannot establish master to %s: %v", s.Target(), err)
		}
	}
	return s
}

func TestRemoteListing(t *testing.T) {
	s := liveSession(t)
	real, entries, err := listRemote(s, "")
	if err != nil || real == "" {
		t.Fatalf("listRemote: %v %q", err, real)
	}
	t.Logf("remote home %s: %d entries", real, len(entries))
}

func TestTransfersRoundTrip(t *testing.T) {
	s := liveSession(t)
	local := LocalSession()

	tmp := t.TempDir()
	sub := filepath.Join(tmp, "down")
	os.MkdirAll(sub, 0o755)
	payload := "go rsync round-trip " + time.Now().Format(time.RFC3339) + "\n"
	name := "sshmc_go_test.txt"
	if err := os.WriteFile(filepath.Join(tmp, name), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
	if rc, msg := s.SSHRunErr("mkdir -p /tmp/sshmc_go", 15*time.Second); rc != 0 {
		t.Fatalf("remote mkdir: %s", msg)
	}
	defer s.SSHRunErr("rm -rf /tmp/sshmc_go", 15*time.Second)

	// upload (rsynccmd path — remote has rsync)
	if err := CopyItem(local, tmp, name, s, "/tmp/sshmc_go"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	// download back
	if err := CopyItem(s, "/tmp/sshmc_go", name, local, sub); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(sub, name))
	if err != nil || string(got) != payload {
		t.Fatalf("round trip mismatch: %v %q", err, got)
	}

	// directory upload through rsync
	dirName := "adir with space"
	os.MkdirAll(filepath.Join(tmp, dirName), 0o755)
	os.WriteFile(filepath.Join(tmp, dirName, "inner.txt"), []byte("nested\n"), 0o644)
	if err := CopyItem(local, tmp, dirName, s, "/tmp/sshmc_go"); err != nil {
		t.Fatalf("dir upload: %v", err)
	}
	rc, out, _ := s.SSHRun("cat '/tmp/sshmc_go/adir with space/inner.txt'", 15*time.Second)
	if rc != 0 || strings.TrimSpace(out) != "nested" {
		t.Fatalf("dir verify: rc=%d out=%q", rc, out)
	}

	// server->server tar pipe: same host forced onto the tarPipe branch by a
	// port-qualified twin session (still exercises both ssh legs + tar).
	twin := *s
	twin.Port = "22"
	if err := tarPipe(s, "/tmp/sshmc_go", name, &twin, "/tmp/sshmc_go/down2"); err == nil {
		t.Fatal("expected tarPipe failure into missing dir")
	}
	s.SSHRunErr("mkdir -p /tmp/sshmc_go/down2", 15*time.Second)
	if err := tarPipe(s, "/tmp/sshmc_go", name, &twin, "/tmp/sshmc_go/down2"); err != nil {
		t.Fatalf("tar pipe: %v", err)
	}
	rc, out, _ = s.SSHRun("cat /tmp/sshmc_go/down2/"+name, 15*time.Second)
	if rc != 0 || string(out) != payload {
		t.Fatalf("tar pipe verify: rc=%d out=%q", rc, out)
	}

	// delete + mkdir helpers
	if err := MkdirItem(s, "/tmp/sshmc_go", "made-by-test"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := DeleteItem(s, "/tmp/sshmc_go", "made-by-test"); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
