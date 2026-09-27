package main

// Tests for the dotfile toggle and the embedded PTY terminal (local + remote).

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestHiddenToggle(t *testing.T) {
	p := NewPane("left")
	p.cwd = "/x"
	p.populate("/x", []Entry{
		{Name: ".hidden", IsDir: false, Size: 1},
		{Name: ".git", IsDir: true, Size: -1},
		{Name: "visible.txt", IsDir: false, Size: 2},
	})
	if len(p.entries) != 3 {
		t.Fatalf("default should show all, got %d", len(p.entries))
	}
	p.toggleHidden()
	if len(p.entries) != 1 || p.entries[0].Name != "visible.txt" {
		t.Fatalf("filtered view wrong: %+v", p.entries)
	}
	if !strings.Contains(p.table.GetTitle(), "no dotfiles") {
		t.Fatalf("title missing indicator: %q", p.table.GetTitle())
	}
	p.toggleHidden()
	if len(p.entries) != 3 {
		t.Fatalf("toggle back failed, got %d", len(p.entries))
	}
}

// startSimUI boots the app on a simulation screen and returns it with a stop func.
func startSimUI(t *testing.T) (*UI, tcell.SimulationScreen, func()) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	sim.SetSize(140, 40)
	ui := NewUI()
	ui.app.SetScreen(sim)
	done := make(chan error, 1)
	go func() { done <- ui.Run() }()
	time.Sleep(600 * time.Millisecond)
	return ui, sim, func() {
		ui.app.QueueUpdateDraw(func() {})
		ui.app.Stop()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("app did not stop")
		}
	}
}

func typeString(sim tcell.SimulationScreen, s string) {
	for _, r := range s {
		sim.InjectKey(tcell.KeyRune, r, tcell.ModNone)
		time.Sleep(20 * time.Millisecond)
	}
	sim.InjectKey(tcell.KeyEnter, 0, tcell.ModNone)
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLocalTerminal(t *testing.T) {
	ui, sim, stop := startSimUI(t)
	defer stop()

	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(600 * time.Millisecond)

	term := ui.terms["local"]
	if term == nil {
		t.Fatal("no local terminal spawned on F9")
	}
	if _, ok := ui.app.GetFocus().(*Term); !ok {
		t.Fatalf("focus not on terminal: %T", ui.app.GetFocus())
	}

	typeString(sim, "echo TERMTEST_$((40+2))")
	waitFor(t, 5*time.Second, "shell output", func() bool {
		return strings.Contains(term.vt.String(), "TERMTEST_42")
	})

	// F9 returns to files; terminal keeps running
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
	if ui.app.GetFocus() != ui.left.table {
		t.Fatalf("F9 did not return to files: %T", ui.app.GetFocus())
	}
	if term.Dead() {
		t.Fatal("terminal died on unfocus")
	}

	// re-enter and exit the shell -> onDead cleans up
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
	typeString(sim, "exit")
	waitFor(t, 5*time.Second, "shell exit cleanup", func() bool {
		var gone bool
		ui.app.QueueUpdateDraw(func() { gone = ui.terms["local"] == nil })
		time.Sleep(50 * time.Millisecond)
		return gone
	})
}

func TestRemoteTerminal(t *testing.T) {
	requireLiveTests(t)
	live := DiscoverLiveSessions()
	if len(live) == 0 {
		t.Skip("no live ssh session")
	}
	sess := live[0]
	if !sess.MasterAlive() {
		t.Skip("no master connection")
	}

	ui, sim, stop := startSimUI(t)
	defer stop()

	// attach left pane to the remote session, then open its shell
	ui.app.QueueUpdateDraw(func() {
		ui.left.session = sess
		ui.left.cwd = "/tmp"
		ui.updateTermView()
	})
	time.Sleep(300 * time.Millisecond)
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(1500 * time.Millisecond)

	term := ui.terms[sessKey(sess)]
	if term == nil {
		t.Fatal("no remote terminal spawned")
	}
	typeString(sim, "echo REMOTE_$((40+2))_OK; pwd")
	waitFor(t, 10*time.Second, "remote shell output", func() bool {
		s := term.vt.String()
		return strings.Contains(s, "REMOTE_42_OK") && strings.Contains(s, "/tmp")
	})
	typeString(sim, "exit")
	time.Sleep(500 * time.Millisecond)
}
