package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestPinnedServerDisabledWithoutHost(t *testing.T) {
	if _, ok := PinnedServerFromEnv(envMap(nil)); ok {
		t.Fatal("pinned server should be disabled when SSHMC_PIN_HOST is empty")
	}
}

func TestPinnedServerSession(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "id_test")
	kh := filepath.Join(dir, "known_hosts")
	for _, p := range []string{key, kh} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, ok := PinnedServerFromEnv(envMap(map[string]string{
		"SSHMC_PIN_HOST": "files.example.com", "SSHMC_PIN_USER": "deploy",
		"SSHMC_PIN_PORT": "2222", "SSHMC_PIN_IDENTITY": key, "SSHMC_PIN_KNOWN_HOSTS": kh,
		"SSHMC_PIN_DIR": "/srv/uploads",
	}))
	if !ok {
		t.Fatal("expected pinned server to be enabled")
	}
	s, err := cfg.Session()
	if err != nil {
		t.Fatal(err)
	}
	if s.Target() != "deploy@files.example.com" || s.Port != "2222" || s.Identity != key {
		t.Fatalf("unexpected session: %+v", s)
	}
	if got := s.Label(); !strings.Contains(got, "[pinned]") {
		t.Fatalf("label = %q", got)
	}
	if !strings.Contains(strings.Join(s.Extra, " "), "UserKnownHostsFile="+kh) {
		t.Fatalf("known_hosts not applied: %v", s.Extra)
	}
}

func TestPinnedServerMissingIdentity(t *testing.T) {
	cfg := PinnedServerConfig{Host: "files.example.com", Identity: "/nonexistent/key"}
	if _, err := cfg.Session(); err == nil {
		t.Fatal("expected an error for a missing identity file")
	}
}
