package main

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestCtrlCInterruptsLocal(t *testing.T) {
	ui, sim, stop := startSimUI(t)
	defer stop()
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(600 * time.Millisecond)
	term := ui.terms["local"]
	if term == nil {
		t.Fatal("no terminal")
	}
	typeString(sim, "sleep 300; echo NI_$((500+5))")
	time.Sleep(500 * time.Millisecond)
	sim.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	time.Sleep(300 * time.Millisecond)
	typeString(sim, "echo AFTER_$((10+1))")
	waitFor(t, 5*time.Second, "prompt back after Ctrl+C", func() bool {
		return strings.Contains(term.vt.String(), "AFTER_11")
	})
	if strings.Contains(term.vt.String(), "NI_505") {
		t.Fatal("sleep was not interrupted")
	}
}

func TestCtrlCInterruptsRemote(t *testing.T) {
	requireLiveTests(t)
	live := DiscoverLiveSessions()
	if len(live) == 0 || !live[0].MasterAlive() {
		t.Skip("no live master")
	}
	sess := live[0]
	ui, sim, stop := startSimUI(t)
	defer stop()
	ui.app.QueueUpdateDraw(func() {
		ui.left.session = sess
		ui.left.cwd = "/tmp"
		ui.updateTermView()
	})
	time.Sleep(300 * time.Millisecond)
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(2500 * time.Millisecond)
	term := ui.terms[sessKey(sess)]
	if term == nil {
		t.Fatal("no remote terminal")
	}
	typeString(sim, "sleep 300; echo NI_$((500+5))")
	time.Sleep(800 * time.Millisecond)
	sim.InjectKey(tcell.KeyCtrlC, 0, tcell.ModCtrl)
	time.Sleep(500 * time.Millisecond)
	typeString(sim, "echo AFTER_$((20+2))")
	waitFor(t, 20*time.Second, "remote prompt back after Ctrl+C", func() bool {
		return strings.Contains(term.vt.String(), "AFTER_22")
	})
	if strings.Contains(term.vt.String(), "NI_505") {
		t.Fatal("remote sleep was not interrupted")
	}
	typeString(sim, "exit")
	time.Sleep(400 * time.Millisecond)
}
