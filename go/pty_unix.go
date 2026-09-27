//go:build !windows

package main

// Unix PTY backend for the embedded terminal (creack/pty).

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type unixPty struct {
	f   *os.File
	cmd *exec.Cmd
}

func startPty(argv []string, dir string, cols, rows int) (ptySession, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixPty{f: f, cmd: cmd}, nil
}

func (p *unixPty) Read(b []byte) (int, error)  { return p.f.Read(b) }
func (p *unixPty) Write(b []byte) (int, error) { return p.f.Write(b) }
func (p *unixPty) Close() error                { return p.f.Close() }

func (p *unixPty) Resize(cols, rows int) error {
	return pty.Setsize(p.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (p *unixPty) Kill() {
	if p.cmd.Process != nil {
		p.cmd.Process.Kill()
	}
}

func (p *unixPty) WaitDone() { p.cmd.Wait() }
