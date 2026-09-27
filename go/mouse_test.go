package main

// Tests: Alt+Tab pane switching from inside the terminal, mouse drag-resize
// of both dividers, and click-to-focus.

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

func TestAltTabFromTerminal(t *testing.T) {
	ui, sim, stop := startSimUI(t)
	defer stop()

	// Alt+Tab from file panes switches panes (Tab path already covers mods)
	sim.InjectKey(tcell.KeyTab, 0, tcell.ModAlt)
	time.Sleep(200 * time.Millisecond)
	if ui.active != ui.right {
		t.Fatalf("Alt+Tab in files: active = %s", ui.active.name)
	}

	// open shell (on right pane's connection), then Alt+Tab out of it
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone)
	time.Sleep(600 * time.Millisecond)
	if _, ok := ui.app.GetFocus().(*Term); !ok {
		t.Fatalf("focus not terminal: %T", ui.app.GetFocus())
	}
	sim.InjectKey(tcell.KeyTab, 0, tcell.ModAlt)
	time.Sleep(300 * time.Millisecond)
	if ui.app.GetFocus() != ui.left.table {
		t.Fatalf("Alt+Tab from terminal: focus = %T (active=%s)", ui.app.GetFocus(), ui.active.name)
	}

	// plain Tab inside the terminal must NOT switch panes (goes to shell)
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone) // back into shell (left/local)
	time.Sleep(400 * time.Millisecond)
	sim.InjectKey(tcell.KeyTab, 0, tcell.ModNone)
	time.Sleep(200 * time.Millisecond)
	if _, ok := ui.app.GetFocus().(*Term); !ok {
		t.Fatalf("plain Tab left the terminal: %T", ui.app.GetFocus())
	}
}

func TestMouseDragResize(t *testing.T) {
	ui, sim, stop := startSimUI(t)
	defer stop()

	// --- vertical split drag ---
	lx, _, lw, _ := ui.left.table.GetRect()
	divX := lx + lw
	_, py, _, ph := ui.panesFlex.GetRect()
	rowInPanes := py + ph/2

	sim.InjectMouse(divX, rowInPanes, tcell.Button1, tcell.ModNone) // grab
	time.Sleep(150 * time.Millisecond)
	if ui.dragMode != 1 {
		t.Fatalf("vertical divider not grabbed (dragMode=%d)", ui.dragMode)
	}
	sim.InjectMouse(divX-21, rowInPanes, tcell.Button1, tcell.ModNone) // drag left ~15%
	time.Sleep(150 * time.Millisecond)
	sim.InjectMouse(divX-21, rowInPanes, tcell.ButtonNone, tcell.ModNone) // release
	time.Sleep(300 * time.Millisecond)
	if ui.dragMode != 0 {
		t.Fatal("drag not released")
	}
	if ui.leftProp >= 10 {
		t.Fatalf("leftProp after drag left = %d, want < 10", ui.leftProp)
	}
	_, _, lwAfter, _ := ui.left.table.GetRect()
	if lwAfter >= lw {
		t.Fatalf("left pane width %d -> %d, expected shrink", lw, lwAfter)
	}

	// --- files/bottom divider drag ---
	_, by, _, _ := ui.bottom.GetRect()
	hBefore := ui.bottomH
	sim.InjectMouse(30, by, tcell.Button1, tcell.ModNone) // grab
	time.Sleep(150 * time.Millisecond)
	if ui.dragMode != 2 {
		t.Fatalf("bottom divider not grabbed (dragMode=%d)", ui.dragMode)
	}
	sim.InjectMouse(30, by-4, tcell.Button1, tcell.ModNone) // drag up 4 rows
	time.Sleep(150 * time.Millisecond)
	sim.InjectMouse(30, by-4, tcell.ButtonNone, tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
	if ui.bottomH != hBefore+4 {
		t.Fatalf("bottomH %d -> %d, want +4", hBefore, ui.bottomH)
	}

	// --- click-to-focus: right pane, then terminal ---
	rx, ry, rw, rh := ui.right.table.GetRect()
	sim.InjectMouse(rx+rw/2, ry+rh/2, tcell.Button1, tcell.ModNone)
	time.Sleep(100 * time.Millisecond)
	sim.InjectMouse(rx+rw/2, ry+rh/2, tcell.ButtonNone, tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
	if ui.active != ui.right {
		t.Fatalf("click on right pane: active = %s", ui.active.name)
	}

	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone) // spawn shell for right pane
	time.Sleep(600 * time.Millisecond)
	sim.InjectKey(tcell.KeyF9, 0, tcell.ModNone) // back to files
	time.Sleep(200 * time.Millisecond)
	tx, ty, tw, th := ui.visibleTerm.GetRect()
	sim.InjectMouse(tx+tw/2, ty+th/2, tcell.Button1, tcell.ModNone)
	time.Sleep(100 * time.Millisecond)
	sim.InjectMouse(tx+tw/2, ty+th/2, tcell.ButtonNone, tcell.ModNone)
	time.Sleep(300 * time.Millisecond)
	if _, ok := ui.app.GetFocus().(*Term); !ok {
		t.Fatalf("click on terminal did not focus it: %T", ui.app.GetFocus())
	}
}
