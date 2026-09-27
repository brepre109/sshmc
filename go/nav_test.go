package main

// Tests for Alt+arrow focus navigation and Ctrl+arrow pane resizing.

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestAltArrowNavAndResize(t *testing.T) {
	ui, sim, stop := startSimUI(t)
	defer stop()

	inject := func(k tcell.Key, mod tcell.ModMask) {
		sim.InjectKey(k, 0, mod)
		time.Sleep(200 * time.Millisecond)
	}

	// Alt+Right -> right pane
	inject(tcell.KeyRight, tcell.ModAlt)
	if ui.active != ui.right {
		t.Fatalf("Alt+Right: active = %s", ui.active.name)
	}
	// Alt+Left -> left pane
	inject(tcell.KeyLeft, tcell.ModAlt)
	if ui.active != ui.left {
		t.Fatalf("Alt+Left: active = %s", ui.active.name)
	}

	// Alt+Down -> opens/focuses shell
	inject(tcell.KeyDown, tcell.ModAlt)
	time.Sleep(400 * time.Millisecond)
	if _, ok := ui.app.GetFocus().(*Term); !ok {
		t.Fatalf("Alt+Down: focus = %T", ui.app.GetFocus())
	}
	// Alt+Up from inside the terminal -> back to files
	inject(tcell.KeyUp, tcell.ModAlt)
	if ui.app.GetFocus() != ui.left.table {
		t.Fatalf("Alt+Up: focus = %T", ui.app.GetFocus())
	}

	// Ctrl+Left x3 shrinks the left pane
	_, _, wBefore, _ := ui.left.table.GetRect()
	for i := 0; i < 3; i++ {
		inject(tcell.KeyLeft, tcell.ModCtrl)
	}
	_, _, wAfter, _ := ui.left.table.GetRect()
	if wAfter >= wBefore {
		t.Fatalf("Ctrl+Left did not shrink left pane: %d -> %d", wBefore, wAfter)
	}
	if ui.leftProp != 7 {
		t.Fatalf("leftProp = %d, want 7", ui.leftProp)
	}

	// Ctrl+Up x4 grows the bottom area
	_, _, _, hBefore := ui.bottom.GetRect()
	for i := 0; i < 4; i++ {
		inject(tcell.KeyUp, tcell.ModCtrl)
	}
	_, _, _, hAfter := ui.bottom.GetRect()
	if hAfter != hBefore+4 {
		t.Fatalf("Ctrl+Up: bottom %d -> %d, want +4", hBefore, hAfter)
	}

	// clamping: hammer Ctrl+Right far past the limit
	for i := 0; i < 30; i++ {
		sim.InjectKey(tcell.KeyRight, 0, tcell.ModCtrl)
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if ui.leftProp != 17 {
		t.Fatalf("leftProp not clamped: %d", ui.leftProp)
	}
}
