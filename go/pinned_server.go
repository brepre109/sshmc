package main

// Optional "pinned" server: a host that sshmc connects to automatically at
// startup and lists in the F2 picker as "pinned:". It is configured entirely
// through environment variables so no credentials or hostnames are compiled
// into the binary:
//
//	SSHMC_PIN_HOST         host name or address (required to enable the feature)
//	SSHMC_PIN_USER         login user (optional; ssh default otherwise)
//	SSHMC_PIN_PORT         port (optional)
//	SSHMC_PIN_IDENTITY     path to a private key file (optional; ssh-agent / ~/.ssh/config otherwise)
//	SSHMC_PIN_DIR          directory the right pane opens in (optional; remote home otherwise)
//	SSHMC_PIN_KNOWN_HOSTS  known_hosts file with the server's pinned host keys (optional;
//	                       when set, StrictHostKeyChecking=yes is enforced against it)

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PinnedServerConfig is the environment-derived configuration of the pinned server.
type PinnedServerConfig struct {
	Host, User, Port, Identity, Dir, KnownHosts string
}

// PinnedServerFromEnv reads the SSHMC_PIN_* variables. ok is false when no
// pinned server is configured.
func PinnedServerFromEnv(getenv func(string) string) (cfg PinnedServerConfig, ok bool) {
	cfg = PinnedServerConfig{
		Host:       strings.TrimSpace(getenv("SSHMC_PIN_HOST")),
		User:       strings.TrimSpace(getenv("SSHMC_PIN_USER")),
		Port:       strings.TrimSpace(getenv("SSHMC_PIN_PORT")),
		Identity:   strings.TrimSpace(getenv("SSHMC_PIN_IDENTITY")),
		Dir:        strings.TrimSpace(getenv("SSHMC_PIN_DIR")),
		KnownHosts: strings.TrimSpace(getenv("SSHMC_PIN_KNOWN_HOSTS")),
	}
	return cfg, cfg.Host != ""
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Session validates the configuration and builds a ready-to-connect session.
// The identity file is used in place (it is never copied or embedded).
func (c PinnedServerConfig) Session() (*Session, error) {
	if c.Host == "" {
		return nil, errors.New("SSHMC_PIN_HOST is not set")
	}
	s := &Session{Host: c.Host, User: c.User, Port: c.Port, Source: "pinned"}
	if c.Identity != "" {
		path := expandHome(c.Identity)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("SSHMC_PIN_IDENTITY: %w", err)
		}
		s.Identity = path
		s.Extra = append(s.Extra, "-o", "IdentitiesOnly=yes")
	}
	if c.KnownHosts != "" {
		path := expandHome(c.KnownHosts)
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("SSHMC_PIN_KNOWN_HOSTS: %w", err)
		}
		s.Extra = append(s.Extra,
			"-o", "StrictHostKeyChecking=yes",
			"-o", "UserKnownHostsFile="+path)
	}
	return s, nil
}
