//go:build windows

package main

// Windows PTY backend for the embedded terminal, using ConPTY
// (Windows 10 1809+ / Windows 11).

import (
	"context"
	"fmt"
	"os"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

type winPty struct {
	cp *conpty.ConPty
}

func startPty(argv []string, dir string, cols, rows int) (ptySession, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, fmt.Errorf("ConPTY not available (needs Windows 10 1809+)")
	}
	cmdline := windows.ComposeCommandLine(argv)
	opts := []conpty.ConPtyOption{
		conpty.ConPtyDimensions(cols, rows),
		conpty.ConPtyEnv(append(os.Environ(), "TERM=xterm-256color")),
	}
	if dir != "" {
		opts = append(opts, conpty.ConPtyWorkDir(dir))
	}
	cp, err := conpty.Start(cmdline, opts...)
	if err != nil {
		return nil, err
	}
	return &winPty{cp: cp}, nil
}

func (p *winPty) Read(b []byte) (int, error)  { return p.cp.Read(b) }
func (p *winPty) Write(b []byte) (int, error) { return p.cp.Write(b) }
func (p *winPty) Close() error                { return p.cp.Close() }

func (p *winPty) Resize(cols, rows int) error { return p.cp.Resize(cols, rows) }

// Close terminates the ConPTY child as well; Kill is the same operation.
func (p *winPty) Kill() { p.cp.Close() }

func (p *winPty) WaitDone() { p.cp.Wait(context.Background()) }
