package main

// Headless TUI smoke test on a tcell SimulationScreen: navigate, mark files,
// copy between two local panes, confirm via the modal, and quit.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestUISmoke(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	for _, n := range []string{"alpha.txt", "beta.txt", "gamma.txt"} {
		os.WriteFile(filepath.Join(src, n), []byte(n+"\n"), 0o644)
	}

	sim := tcell.NewSimulationScreen("UTF-8")
	sim.SetSize(140, 40)

	ui := NewUI()
	ui.left.cwd = src
	ui.right.cwd = dst
	ui.app.SetScreen(sim)

	done := make(chan error, 1)
	go func() { done <- ui.Run() }()
	time.Sleep(700 * time.Millisecond)

	if len(ui.left.entries) != 3 {
		t.Fatalf("left pane entries = %d, want 3", len(ui.left.entries))
	}

	key := func(k tcell.Key, r rune) {
		sim.InjectKey(k, r, tcell.ModNone)
		time.Sleep(150 * time.Millisecond)
	}

	// mark alpha and beta (first space skips "..")
	key(tcell.KeyRune, ' ')
	key(tcell.KeyRune, ' ')
	key(tcell.KeyRune, ' ')
	if !ui.left.marked["alpha.txt"] || !ui.left.marked["beta.txt"] || len(ui.left.marked) != 2 {
		t.Fatalf("marked = %v", ui.left.marked)
	}

	// copy to right pane, confirm modal ("Yes" focused by default)
	key(tcell.KeyF5, 0)
	key(tcell.KeyEnter, 0)
	time.Sleep(1 * time.Second)

	for _, n := range []string{"alpha.txt", "beta.txt"} {
		if _, err := os.Stat(filepath.Join(dst, n)); err != nil {
			t.Fatalf("%s not copied: %v", n, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "gamma.txt")); err == nil {
		t.Fatal("gamma.txt copied but was not marked")
	}
	if len(ui.left.marked) != 0 {
		t.Fatalf("marks not cleared after copy: %v", ui.left.marked)
	}

	// tab to right pane, F8 delete alpha (cursor row 2 = first file), confirm
	key(tcell.KeyTab, 0)
	if ui.active != ui.right {
		t.Fatal("tab did not switch pane")
	}
	key(tcell.KeyRune, ' ') // skip ".."; cursor now on alpha
	ui.right.marked = map[string]bool{"alpha.txt": true}
	key(tcell.KeyF8, 0)
	key(tcell.KeyEnter, 0)
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dst, "alpha.txt")); err == nil {
		t.Fatal("alpha.txt not deleted")
	}

	// quit
	key(tcell.KeyRune, 'q')
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("app exited with error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("app did not stop on q")
	}
}
