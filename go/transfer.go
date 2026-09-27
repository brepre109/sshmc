package main

// File operations. local<->remote transfers use the pure-Go rsync client
// (gokrazy/rsync) over the shared ssh mux socket, falling back to scp when
// the remote side has no rsync binary. Server->server copies stream a tar
// pipe through this machine so each leg keeps its own port/key/jump flags.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gokrazy/rsync/rsynccmd"
)

var (
	rsyncOKMu sync.Mutex
	rsyncOK   = map[string]bool{} // target -> remote rsync available
)

func remoteHasRsync(sess *Session) bool {
	rsyncOKMu.Lock()
	defer rsyncOKMu.Unlock()
	key := sess.Target() + ":" + sess.Port
	if ok, seen := rsyncOK[key]; seen {
		return ok
	}
	rc, _, _ := sess.SSHRun("command -v rsync >/dev/null 2>&1", 15*time.Second)
	rsyncOK[key] = rc == 0
	return rc == 0
}

func runCmd(name string, args ...string) (int, string) {
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		rc = 1
	}
	return rc, strings.TrimSpace(string(out))
}

// rsyncRun drives the embedded Go rsync client (no local rsync needed; the
// remote end runs `rsync --server` spawned over our mux ssh).
func rsyncRun(remote *Session, srcArg, dstArg string) error {
	var out strings.Builder
	cmd := rsynccmd.Command("rsync", "-a", "-e", remote.rshCommand(), srcArg, dstArg)
	cmd.Stdout = &out
	cmd.Stderr = &out
	// gokrazy/rsync sandboxes itself with Landlock by default, which would
	// deny access to every path a user might browse to — disable it.
	cmd.DontRestrict = true
	_, err := cmd.Run(context.Background())
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if msg != "" {
			return fmt.Errorf("%v: %s", err, msg)
		}
		return err
	}
	return nil
}

// CopyItem copies one file/dir between any combination of panes.
func CopyItem(src *Session, srcDir, name string, dst *Session, dstDir string) error {
	sp := path.Join(srcDir, name)
	if src.IsLocal() {
		sp = filepath.Join(srcDir, name) // native separators on Windows
	}

	switch {
	case src.IsLocal() && dst.IsLocal():
		return localCopy(sp, filepath.Join(dstDir, name))

	case !src.IsLocal() && !dst.IsLocal() && src.SameHost(dst):
		if rc, msg := src.SSHRunErr(fmt.Sprintf("cp -a %s %s/", shQuote(sp), shQuote(dstDir)), 10*time.Minute); rc != 0 {
			return fmt.Errorf("remote cp: %s", msg)
		}
		return nil

	case !src.IsLocal() && !dst.IsLocal():
		return tarPipe(src, srcDir, name, dst, dstDir)

	case src.IsLocal(): // upload
		if remoteHasRsync(dst) {
			if err := rsyncRun(dst, sp, dst.Target()+":"+shQuote(dstDir)+"/"); err == nil {
				return nil
			} else if !isTransientRsyncErr(err) {
				return err
			}
		}
		args := append(muxArgs(), batchArgs()...)
		args = append(args, dst.scpFlags()...)
		args = append(args, "-r", "-q", sp, dst.Target()+":"+shQuote(dstDir)+"/")
		if rc, msg := runCmd("scp", args...); rc != 0 {
			return fmt.Errorf("scp: %s", msg)
		}
		return nil

	default: // download
		if remoteHasRsync(src) {
			if err := rsyncRun(src, src.Target()+":"+shQuote(sp), dstDir+"/"); err == nil {
				return nil
			} else if !isTransientRsyncErr(err) {
				return err
			}
		}
		args := append(muxArgs(), batchArgs()...)
		args = append(args, src.scpFlags()...)
		args = append(args, "-r", "-q", src.Target()+":"+shQuote(sp), dstDir+"/")
		if rc, msg := runCmd("scp", args...); rc != 0 {
			return fmt.Errorf("scp: %s", msg)
		}
		return nil
	}
}

// isTransientRsyncErr reports errors worth retrying with scp (e.g. protocol
// hiccups), as opposed to real failures like "permission denied".
func isTransientRsyncErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "command not found") ||
		strings.Contains(msg, "connection unexpectedly closed") ||
		strings.Contains(msg, "protocol")
}

// tarPipe streams src through this machine to dst: ssh A tar cf - | ssh B tar xf -
func tarPipe(src *Session, srcDir, name string, dst *Session, dstDir string) error {
	readArgs := append(muxArgs(), batchArgs()...)
	readArgs = append(readArgs, src.sshFlags()...)
	readArgs = append(readArgs, src.Target(), "--",
		fmt.Sprintf("tar cf - -C %s %s", shQuote(srcDir), shQuote(name)))
	writeArgs := append(muxArgs(), batchArgs()...)
	writeArgs = append(writeArgs, dst.sshFlags()...)
	writeArgs = append(writeArgs, dst.Target(), "--",
		fmt.Sprintf("tar xf - -C %s", shQuote(dstDir)))

	reader := exec.Command("ssh", readArgs...)
	writer := exec.Command("ssh", writeArgs...)
	var rErr, wErr strings.Builder
	reader.Stderr = &rErr
	writer.Stderr = &wErr
	pipe, err := reader.StdoutPipe()
	if err != nil {
		return err
	}
	writer.Stdin = pipe
	if err := reader.Start(); err != nil {
		return err
	}
	if err := writer.Start(); err != nil {
		reader.Process.Kill()
		return err
	}
	rWait := reader.Wait()
	wWait := writer.Wait()
	if rWait != nil || wWait != nil {
		msg := strings.TrimSpace(rErr.String() + " " + wErr.String())
		return fmt.Errorf("tar pipe failed: %s", msg)
	}
	return nil
}

func DeleteItem(sess *Session, dir, name string) error {
	if sess.IsLocal() {
		return os.RemoveAll(filepath.Join(dir, name))
	}
	p := path.Join(dir, name)
	if rc, msg := sess.SSHRunErr("rm -rf -- "+shQuote(p), 60*time.Second); rc != 0 {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func MkdirItem(sess *Session, dir, name string) error {
	if sess.IsLocal() {
		return os.MkdirAll(filepath.Join(dir, name), 0o755)
	}
	p := path.Join(dir, name)
	if rc, msg := sess.SSHRunErr("mkdir -p -- "+shQuote(p), 30*time.Second); rc != 0 {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// localCopy is a native recursive copy (works on every OS; overwrites the
// destination), preserving file modes and modification times best-effort.
func localCopy(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		os.Remove(dst)
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
			return err
		}
		des, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, de := range des {
			if err := localCopy(filepath.Join(src, de.Name()), filepath.Join(dst, de.Name())); err != nil {
				return err
			}
		}
		return nil
	default:
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Close(); err != nil {
			return err
		}
		os.Chtimes(dst, time.Now(), info.ModTime())
		return nil
	}
}
