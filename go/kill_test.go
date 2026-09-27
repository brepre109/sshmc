package main

// The 'k' key must recover a wedged shell: kill it, then F9 gets a fresh one.

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestKillShell(t *testing.T) {
	ui, sim, stop := startSimUI(t)
	defer stop()

	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(600 * time.Millisecond)
	term := ui.terms["local"]
	if term == nil {
		t.Fatal("no shell")
	}
	// simulate a wedged shell: a command that ignores SIGINT
	typeString(sim, "trap '' INT; sleep 300")
	time.Sleep(500 * time.Millisecond)

	// escape to files (F9 always works), then kill
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
	sim.InjectKey(tcell.KeyRune, 'k', tcell.ModNone)
	waitFor(t, 5*time.Second, "shell cleanup after kill", func() bool {
		var gone bool
		done := make(chan struct{})
		ui.app.QueueUpdateDraw(func() { gone = ui.terms["local"] == nil; close(done) })
		<-done
		return gone
	})
	if !term.Dead() {
		t.Fatal("old term not dead")
	}

	// F9 spawns a fresh working shell
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(700 * time.Millisecond)
	fresh := ui.terms["local"]
	if fresh == nil || fresh == term {
		t.Fatalf("no fresh shell after kill (fresh=%p old=%p)", fresh, term)
	}
	typeString(sim, "echo FRESH_$((7+7))")
	waitFor(t, 5*time.Second, "fresh shell works", func() bool {
		return strings.Contains(fresh.vt.String(), "FRESH_14")
	})

	// 'k' with no shell just logs, no crash
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone) // back to files
	time.Sleep(200 * time.Millisecond)
	typeStringNoop := func() {}
	_ = typeStringNoop
	ui.app.QueueUpdateDraw(func() {
		if tm := ui.terms["local"]; tm != nil {
			tm.Close()
		}
	})
	time.Sleep(500 * time.Millisecond)
	sim.InjectKey(tcell.KeyRune, 'k', tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
}
