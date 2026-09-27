package main

// tview UI: dual file panes, log + command bar below, modal dialogs.

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// ---------------------------------------------------------------- pane

type Pane struct {
	name       string // "left" | "right"
	table      *tview.Table
	session    *Session
	cwd        string
	all        []Entry // unfiltered listing
	entries    []Entry // what the table shows (respects showHidden)
	marked     map[string]bool
	showHidden bool
}

func NewPane(name string) *Pane {
	home, _ := os.UserHomeDir()
	p := &Pane{
		name:       name,
		table:      tview.NewTable(),
		session:    LocalSession(),
		cwd:        home,
		marked:     map[string]bool{},
		showHidden: true,
	}
	p.table.SetSelectable(true, false).SetFixed(1, 0)
	p.table.SetBorder(true)
	return p
}

func (p *Pane) hasDotDot() bool { return parentPath(p.session, p.cwd) != "" }

// entryAt maps a table row to an entry (nil for header or "..").
func (p *Pane) entryAt(row int) *Entry {
	off := 1 // header
	if p.hasDotDot() {
		off = 2
	}
	i := row - off
	if i < 0 || i >= len(p.entries) {
		return nil
	}
	return &p.entries[i]
}

func (p *Pane) cursorEntry() *Entry {
	row, _ := p.table.GetSelection()
	return p.entryAt(row)
}

func (p *Pane) selection() []Entry {
	if len(p.marked) > 0 {
		var out []Entry
		for _, e := range p.entries {
			if p.marked[e.Name] {
				out = append(out, e)
			}
		}
		return out
	}
	if e := p.cursorEntry(); e != nil {
		return []Entry{*e}
	}
	return nil
}

func (p *Pane) markedNames() string {
	names := make([]string, 0, len(p.marked))
	for n := range p.marked {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (p *Pane) refreshTitle() {
	extra := ""
	if !p.showHidden {
		extra += "  [no dotfiles]"
	}
	if n := len(p.marked); n > 0 {
		extra += fmt.Sprintf("  (%d marked)", n)
	}
	p.table.SetTitle(fmt.Sprintf(" %s  %s%s ", p.session.Label(), p.cwd, extra))
}

func (p *Pane) populate(real string, entries []Entry) {
	p.cwd = real
	p.all = entries
	p.render()
}

func (p *Pane) toggleHidden() {
	p.showHidden = !p.showHidden
	p.render()
}

// render fills the table from p.all, applying the dotfile filter.
func (p *Pane) render() {
	p.entries = p.entries[:0]
	for _, e := range p.all {
		if p.showHidden || !strings.HasPrefix(e.Name, ".") {
			p.entries = append(p.entries, e)
		}
	}
	entries := p.entries
	p.marked = map[string]bool{}
	t := p.table
	t.Clear()
	hdr := tcell.StyleDefault.Bold(true)
	for col, h := range []string{" ", "Name", "Size", "Modified"} {
		t.SetCell(0, col, tview.NewTableCell(h).SetStyle(hdr).SetSelectable(false))
	}
	row := 1
	if p.hasDotDot() {
		t.SetCell(row, 0, tview.NewTableCell(" "))
		t.SetCell(row, 1, tview.NewTableCell("..").SetExpansion(1))
		t.SetCell(row, 2, tview.NewTableCell(""))
		t.SetCell(row, 3, tview.NewTableCell(""))
		row++
	}
	for _, e := range entries {
		deco, size := "", humanSize(e.Size)
		color := tview.Styles.PrimaryTextColor
		if e.IsDir {
			deco, size = "/", "<DIR>"
			color = tcell.ColorAqua
		} else if e.IsLink {
			deco = "@"
		}
		t.SetCell(row, 0, tview.NewTableCell(" "))
		t.SetCell(row, 1, tview.NewTableCell(e.Name+deco).SetExpansion(1).SetTextColor(color))
		t.SetCell(row, 2, tview.NewTableCell(size).SetAlign(tview.AlignRight))
		t.SetCell(row, 3, tview.NewTableCell(e.MtimeStr))
		row++
	}
	t.ScrollToBeginning()
	t.Select(1, 0)
	p.refreshTitle()
}

func (p *Pane) toggleMark() {
	row, _ := p.table.GetSelection()
	e := p.entryAt(row)
	if e != nil {
		mark := " "
		if !p.marked[e.Name] {
			p.marked[e.Name] = true
			mark = "*"
		} else {
			delete(p.marked, e.Name)
		}
		p.table.SetCell(row, 0, tview.NewTableCell(mark).SetTextColor(tcell.ColorYellow))
		p.refreshTitle()
	}
	if row < p.table.GetRowCount()-1 {
		p.table.Select(row+1, 0)
	}
}

// ---------------------------------------------------------------- app

type UI struct {
	app      *tview.Application
	pages    *tview.Pages
	left     *Pane
	right    *Pane
	active   *Pane
	logView  *tview.TextView
	cmdInput *tview.InputField

	termHost    *tview.Flex // bottom-left slot: placeholder or active terminal
	termNote    *tview.TextView
	terms       map[string]*Term // one live shell per connection
	visibleTerm *Term

	panesFlex *tview.Flex
	rootFlex  *tview.Flex
	bottom    *tview.Flex
	leftProp  int // left pane share of the split, in twentieths [3..17]
	bottomH   int // rows for the terminal/log area
	dragMode  int // 0 none, 1 vertical split, 2 files/bottom divider

	pinnedSession *Session
}

func sessKey(s *Session) string {
	if s.IsLocal() {
		return "local"
	}
	return s.Target() + ":" + s.Port
}

func NewUI() *UI {
	ui := &UI{app: tview.NewApplication()}
	ui.left = NewPane("left")
	ui.right = NewPane("right")
	ui.active = ui.left

	ui.logView = tview.NewTextView().SetScrollable(true).SetMaxLines(2000)
	ui.logView.SetBorder(true).SetTitle(" log ")
	ui.cmdInput = tview.NewInputField().SetLabel("cmd> ").
		SetPlaceholder("runs on active pane; `cd` changes dir; Esc back to files")

	ui.terms = map[string]*Term{}
	ui.termNote = tview.NewTextView().SetTextAlign(tview.AlignCenter)
	ui.termNote.SetBorder(true).SetTitle(" shell ")
	ui.termHost = tview.NewFlex().AddItem(ui.termNote, 0, 1, false)

	ui.leftProp = 10
	ui.bottomH = 14
	ui.panesFlex = tview.NewFlex().
		AddItem(ui.left.table, 0, ui.leftProp, true).
		AddItem(ui.right.table, 0, 20-ui.leftProp, false)
	keybar := tview.NewTextView().SetDynamicColors(true).
		SetText("[yellow]Tab/M-↔[-] pane  [yellow]C-arrows[-] resize  [yellow]F2[-] session  [yellow]Spc[-] mark  [yellow]F5[-] copy  [yellow]F6[-] move  [yellow]F7[-] mkdir  [yellow]F8[-] del  [yellow]F9/M-↓[-] shell  [yellow]k[-] kill-shell  [yellow].[-] dotfiles  [yellow]F3[-] view  [yellow]F4[-] refresh  [yellow]:[-] cmd  [yellow]F10/q[-] quit")
	rightCol := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ui.logView, 0, 1, false).
		AddItem(ui.cmdInput, 1, 0, false)
	ui.bottom = tview.NewFlex().
		AddItem(ui.termHost, 0, 3, false).
		AddItem(rightCol, 0, 2, false)
	ui.rootFlex = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(ui.panesFlex, 0, 1, true).
		AddItem(ui.bottom, ui.bottomH, 0, false).
		AddItem(keybar, 1, 0, false)
	root := ui.rootFlex

	ui.pages = tview.NewPages().AddPage("main", root, true, true)
	ui.app.SetRoot(ui.pages, true)

	ui.left.table.SetSelectedFunc(func(row, col int) { ui.openRow(ui.left, row) })
	ui.right.table.SetSelectedFunc(func(row, col int) { ui.openRow(ui.right, row) })
	ui.left.table.SetFocusFunc(func() { ui.setActive(ui.left) })
	ui.right.table.SetFocusFunc(func() { ui.setActive(ui.right) })

	ui.cmdInput.SetDoneFunc(func(key tcell.Key) {
		switch key {
		case tcell.KeyEnter:
			cmd := strings.TrimSpace(ui.cmdInput.GetText())
			ui.cmdInput.SetText("")
			if cmd != "" {
				ui.runCommand(cmd)
			}
		case tcell.KeyEscape:
			ui.app.SetFocus(ui.active.table)
		}
	})

	ui.app.SetInputCapture(ui.handleKey)
	ui.app.EnableMouse(true)
	ui.app.SetMouseCapture(ui.handleMouse)
	return ui
}

func (ui *UI) other() *Pane {
	if ui.active == ui.left {
		return ui.right
	}
	return ui.left
}

func (ui *UI) setActive(p *Pane) {
	ui.active = p
	ui.left.table.SetBorderColor(tcell.ColorGray)
	ui.right.table.SetBorderColor(tcell.ColorGray)
	p.table.SetBorderColor(tcell.ColorGreen)
	ui.updateTermView()
}

// updateTermView shows the terminal belonging to the active pane's
// connection (or a hint if none has been opened yet).
func (ui *UI) updateTermView() {
	t := ui.terms[sessKey(ui.active.session)]
	if t == ui.visibleTerm && t != nil {
		return
	}
	ui.termHost.Clear()
	ui.visibleTerm = t
	if t != nil {
		ui.termHost.AddItem(t, 0, 1, false)
	} else {
		ui.termNote.SetText("\nF9 opens a live shell on " + ui.active.session.Label())
		ui.termHost.AddItem(ui.termNote, 0, 1, false)
	}
}

func isArrow(k tcell.Key) bool {
	return k == tcell.KeyLeft || k == tcell.KeyRight || k == tcell.KeyUp || k == tcell.KeyDown
}

// altNav moves focus between regions: Alt+←/→ file panes, Alt+↓ shell,
// Alt+↑ back to the active pane's file table.
func (ui *UI) altNav(k tcell.Key) {
	switch k {
	case tcell.KeyLeft:
		ui.app.SetFocus(ui.left.table)
	case tcell.KeyRight:
		ui.app.SetFocus(ui.right.table)
	case tcell.KeyDown:
		ui.openShell()
	case tcell.KeyUp:
		ui.app.SetFocus(ui.active.table)
	}
}

// resizeLayout moves the divider in the direction of the Ctrl+arrow:
// ←/→ shift the left/right split, ↑/↓ move the files/bottom divider.
func (ui *UI) resizeLayout(k tcell.Key) {
	switch k {
	case tcell.KeyLeft:
		if ui.leftProp > 3 {
			ui.leftProp--
		}
	case tcell.KeyRight:
		if ui.leftProp < 17 {
			ui.leftProp++
		}
	case tcell.KeyUp:
		if ui.bottomH < 35 {
			ui.bottomH++
		}
	case tcell.KeyDown:
		if ui.bottomH > 5 {
			ui.bottomH--
		}
	}
	ui.applyLayout()
}

func (ui *UI) applyLayout() {
	ui.panesFlex.ResizeItem(ui.left.table, 0, ui.leftProp)
	ui.panesFlex.ResizeItem(ui.right.table, 0, 20-ui.leftProp)
	ui.rootFlex.ResizeItem(ui.bottom, ui.bottomH, 0)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// handleMouse implements divider dragging: grab within one cell of the
// left/right split or of the files/bottom boundary, drag, release.
// Everything else passes through to tview (click-to-focus, wheel scroll).
func (ui *UI) handleMouse(ev *tcell.EventMouse, action tview.MouseAction) (*tcell.EventMouse, tview.MouseAction) {
	if name, _ := ui.pages.GetFrontPage(); name != "main" {
		return ev, action // modal up: let it own the mouse
	}
	mx, my := ev.Position()
	switch action {
	case tview.MouseLeftDown:
		lx, _, lw, _ := ui.left.table.GetRect()
		_, py, _, ph := ui.panesFlex.GetRect()
		_, by, _, _ := ui.bottom.GetRect()
		divX := lx + lw // boundary column between the two tables
		if my >= py && my < py+ph && absInt(mx-divX) <= 1 {
			ui.dragMode = 1
			return nil, action
		}
		if absInt(my-by) <= 1 {
			ui.dragMode = 2
			return nil, action
		}
	case tview.MouseMove:
		if ui.dragMode != 0 {
			ui.dragTo(mx, my)
			return nil, action
		}
	case tview.MouseLeftUp:
		if ui.dragMode != 0 {
			ui.dragTo(mx, my)
			ui.dragMode = 0
			return nil, action
		}
	}
	return ev, action
}

func (ui *UI) dragTo(mx, my int) {
	switch ui.dragMode {
	case 1:
		px, _, pw, _ := ui.panesFlex.GetRect()
		if pw > 0 {
			ui.leftProp = clampInt((mx-px)*20/pw, 3, 17)
		}
	case 2:
		_, ry, _, rh := ui.rootFlex.GetRect()
		// keybar takes the last row; the bottom area starts at row my
		ui.bottomH = clampInt(ry+rh-1-my, 5, 35)
	}
	ui.applyLayout()
}

// killShell force-terminates the active connection's shell — the escape
// hatch for a wedged terminal (hung ssh, stuck command ignoring Ctrl+C).
// The readLoop notices the closed PTY and onDead cleans everything up;
// the next F9 spawns a fresh shell.
func (ui *UI) killShell() {
	sess := ui.active.session
	if t := ui.terms[sessKey(sess)]; t != nil {
		ui.logf("killing shell on %s… (F9 starts a new one)", sess.Label())
		t.Close()
	} else {
		ui.logf("no shell running for %s", sess.Label())
	}
}

// openShell focuses the active connection's terminal, spawning it if needed.
func (ui *UI) openShell() {
	sess := ui.active.session
	key := sessKey(sess)
	t := ui.terms[key]
	if t == nil || t.Dead() {
		argv, dir := shellArgv(sess, ui.active.cwd)
		nt, err := NewTerm(ui.app, sess.Label(), argv, dir, func(dead *Term) {
			for k, v := range ui.terms {
				if v == dead {
					delete(ui.terms, k)
				}
			}
			ui.logf("shell on %s exited", sess.Label())
			ui.updateTermView()
			if _, ok := ui.app.GetFocus().(*Term); ok {
				ui.app.SetFocus(ui.active.table)
			}
		})
		if err != nil {
			ui.logf("cannot start shell on %s: %v", sess.Label(), err)
			return
		}
		ui.terms[key] = nt
		t = nt
	}
	ui.visibleTerm = nil // force re-show even if already visible
	ui.updateTermView()
	ui.app.SetFocus(t)
}

func (ui *UI) logf(format string, args ...any) {
	fmt.Fprintf(ui.logView, format+"\n", args...)
	ui.logView.ScrollToEnd()
}

// queueLogf is logf callable from goroutines.
func (ui *UI) queueLogf(format string, args ...any) {
	ui.app.QueueUpdateDraw(func() { ui.logf(format, args...) })
}

// ---------------------------------------------------------------- keys

func (ui *UI) handleKey(ev *tcell.EventKey) *tcell.EventKey {
	if name, _ := ui.pages.GetFrontPage(); name != "main" {
		return ev // a modal is up: let it handle input
	}

	// While a terminal has focus every key belongs to the shell (including
	// Ctrl-C, which would otherwise quit tview) — except F9 to come back
	// and Alt+arrows for pane navigation. Ctrl+arrows pass through so bash
	// word-jumping keeps working inside the shell.
	if t, ok := ui.app.GetFocus().(*Term); ok {
		if ev.Key() == tcell.KeyF9 {
			ui.app.SetFocus(ui.active.table)
			return nil
		}
		// Alt+Tab switches to the other file pane even from inside the
		// shell (plain Tab keeps feeding the shell for completion).
		if ev.Key() == tcell.KeyTab && ev.Modifiers()&tcell.ModAlt != 0 {
			ui.app.SetFocus(ui.other().table)
			return nil
		}
		if ev.Modifiers()&tcell.ModAlt != 0 && isArrow(ev.Key()) {
			ui.altNav(ev.Key())
			return nil
		}
		t.Feed(ev)
		return nil
	}

	focusOnTable := ui.app.GetFocus() == ui.left.table || ui.app.GetFocus() == ui.right.table

	if isArrow(ev.Key()) {
		if ev.Modifiers()&tcell.ModAlt != 0 {
			ui.altNav(ev.Key())
			return nil
		}
		if ev.Modifiers()&tcell.ModCtrl != 0 && focusOnTable {
			ui.resizeLayout(ev.Key())
			return nil
		}
	}

	switch ev.Key() {
	case tcell.KeyF9:
		ui.openShell()
		return nil
	case tcell.KeyTab:
		ui.app.SetFocus(ui.other().table)
		return nil
	case tcell.KeyF2:
		ui.pickSession()
		return nil
	case tcell.KeyF3:
		ui.viewFile()
		return nil
	case tcell.KeyF4:
		ui.refreshPanes()
		return nil
	case tcell.KeyF5:
		ui.transfer(false)
		return nil
	case tcell.KeyF6:
		ui.transfer(true)
		return nil
	case tcell.KeyF7:
		ui.mkdirDialog()
		return nil
	case tcell.KeyF8:
		ui.deleteDialog()
		return nil
	case tcell.KeyF10:
		ui.app.Stop()
		return nil
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		if focusOnTable {
			ui.upDir()
			return nil
		}
	}

	if !focusOnTable {
		return ev
	}
	switch ev.Rune() {
	case ' ':
		ui.active.toggleMark()
		return nil
	case 's':
		ui.pickSession()
		return nil
	case 'c':
		ui.transfer(false)
		return nil
	case 'm':
		ui.transfer(true)
		return nil
	case 'n':
		ui.mkdirDialog()
		return nil
	case 'd':
		ui.deleteDialog()
		return nil
	case 'v':
		ui.viewFile()
		return nil
	case 'r':
		ui.refreshPanes()
		return nil
	case '.':
		ui.active.toggleHidden()
		return nil
	case 'k':
		ui.killShell()
		return nil
	case ':', '`':
		ui.app.SetFocus(ui.cmdInput)
		return nil
	case 'q':
		ui.app.Stop()
		return nil
	}
	return ev
}

// ---------------------------------------------------------------- navigation

func (ui *UI) loadPane(p *Pane, dir string) {
	sess := p.session
	go func() {
		real, entries, err := ListDir(sess, dir)
		ui.app.QueueUpdateDraw(func() {
			if err != nil {
				ui.logf("[%s] cannot open %s: %v", sess.Label(), dir, err)
				return
			}
			if p.session == sess { // pane may have been re-attached meanwhile
				p.populate(real, entries)
			}
		})
	}()
}

// joinPath joins with native separators for local panes, POSIX for remote.
func joinPath(sess *Session, dir, name string) string {
	if sess.IsLocal() {
		return filepath.Join(dir, name)
	}
	return path.Join(dir, name)
}

// parentPath returns the parent directory ("" if already at the root).
func parentPath(sess *Session, dir string) string {
	if sess.IsLocal() {
		parent := filepath.Dir(strings.TrimRight(dir, `/\`))
		if parent == dir || parent == "." {
			return ""
		}
		return parent
	}
	parent := path.Dir(strings.TrimRight(dir, "/"))
	if parent == "" || parent == "." {
		parent = "/"
	}
	if parent == dir {
		return ""
	}
	return parent
}

func (ui *UI) openRow(p *Pane, row int) {
	if p.hasDotDot() && row == 1 {
		ui.upDir()
		return
	}
	e := p.entryAt(row)
	if e == nil {
		return
	}
	if e.IsDir || e.IsLink {
		ui.loadPane(p, joinPath(p.session, p.cwd, e.Name))
	} else {
		ui.viewFile()
	}
}

func (ui *UI) upDir() {
	p := ui.active
	if parent := parentPath(p.session, p.cwd); parent != "" {
		ui.loadPane(p, parent)
	}
}

func (ui *UI) refreshPanes() {
	ui.loadPane(ui.left, ui.left.cwd)
	ui.loadPane(ui.right, ui.right.cwd)
}

// ---------------------------------------------------------------- dialogs

func (ui *UI) confirm(text string, onYes func()) {
	modal := tview.NewModal().SetText(text).AddButtons([]string{"Yes", "No"}).
		SetDoneFunc(func(i int, label string) {
			ui.pages.RemovePage("confirm")
			ui.app.SetFocus(ui.active.table)
			if label == "Yes" {
				onYes()
			}
		})
	ui.pages.AddPage("confirm", modal, true, true)
}

func (ui *UI) prompt(title string, onDone func(string)) {
	input := tview.NewInputField().SetLabel("> ")
	input.SetBorder(true).SetTitle(" " + title + " ")
	input.SetDoneFunc(func(key tcell.Key) {
		text := strings.TrimSpace(input.GetText())
		ui.pages.RemovePage("prompt")
		ui.app.SetFocus(ui.active.table)
		if key == tcell.KeyEnter && text != "" {
			onDone(text)
		}
	})
	grid := tview.NewGrid().
		SetColumns(0, 60, 0).SetRows(0, 3, 0).
		AddItem(input, 1, 1, 1, 1, 0, 0, true)
	ui.pages.AddPage("prompt", grid, true, true)
	ui.app.SetFocus(input)
}

func (ui *UI) pickSession() {
	pane := ui.active
	list := tview.NewList().ShowSecondaryText(false)
	list.SetBorder(true).SetTitle(fmt.Sprintf(" Attach %s pane to: ", pane.name))

	var sessions []*Session
	add := func(label string, s *Session) {
		sessions = append(sessions, s)
		list.AddItem(label, "", 0, nil)
	}
	add("Local filesystem", LocalSession())
	if ui.pinnedSession != nil {
		add("pinned: "+ui.pinnedSession.Target(), ui.pinnedSession)
	}
	for _, s := range DiscoverLiveSessions() {
		label := "live:  " + s.Target()
		if s.Port != "" {
			label += " -p " + s.Port
		}
		add(label, s)
	}
	for _, s := range ConfigHosts() {
		add("conf:  "+s.Host, s)
	}
	add("Enter destination manually (user@host ...)", nil)

	close := func() {
		ui.pages.RemovePage("picker")
		ui.app.SetFocus(pane.table)
	}
	list.SetSelectedFunc(func(i int, _, _ string, _ rune) {
		close()
		if i == len(sessions)-1 { // manual entry
			ui.prompt("ssh destination: user@host [-p N] [-i key]", func(text string) {
				tokens := strings.Fields(text)
				if len(tokens) > 0 && tokens[0] == "ssh" {
					tokens = tokens[1:]
				}
				if s := parseSSHTokens(tokens, "", "manual"); s != nil {
					ui.attachSession(pane, s)
				}
			})
			return
		}
		ui.attachSession(pane, sessions[i])
	})
	list.SetDoneFunc(close)

	grid := tview.NewGrid().
		SetColumns(0, 70, 0).SetRows(0, list.GetItemCount()+2, 0).
		AddItem(list, 1, 1, 1, 1, 0, 0, true)
	ui.pages.AddPage("picker", grid, true, true)
	ui.app.SetFocus(list)
}

func (ui *UI) attachSession(pane *Pane, sess *Session) {
	if !sess.IsLocal() && !sess.MasterAlive() {
		ok := false
		ui.app.Suspend(func() {
			fmt.Printf("\n== sshmc: connecting to %s (answer any prompts) ==\n\n", sess.Target())
			if err := sess.ConnectMaster(); err != nil {
				fmt.Println(err)
				time.Sleep(1500 * time.Millisecond)
			} else {
				ok = sess.MasterAlive()
			}
		})
		if !ok {
			ui.logf("Connection to %s failed.", sess.Target())
			return
		}
	}
	pane.session = sess
	start := ""
	if sess.IsLocal() {
		start, _ = os.UserHomeDir()
	}
	ui.logf("%s pane -> %s", pane.name, sess.Label())
	if pane == ui.active {
		ui.updateTermView()
	}
	ui.loadPane(pane, start)
}

// ---------------------------------------------------------------- operations

func (ui *UI) transfer(move bool) {
	src, dst := ui.active, ui.other()
	items := src.selection()
	if len(items) == 0 {
		ui.logf("Nothing selected.")
		return
	}
	verb := "Copy"
	if move {
		verb = "Move"
	}
	names := make([]string, 0, 5)
	for i, e := range items {
		if i == 5 {
			names = append(names, "…")
			break
		}
		names = append(names, e.Name)
	}
	msg := fmt.Sprintf("%s %d item(s): %s\n\nfrom %s:%s\nto     %s:%s\n\n(existing files are overwritten)",
		verb, len(items), strings.Join(names, ", "),
		src.session.Label(), src.cwd, dst.session.Label(), dst.cwd)
	ui.confirm(msg, func() {
		srcSess, srcCwd := src.session, src.cwd
		dstSess, dstCwd := dst.session, dst.cwd
		go func() {
			ok := 0
			for _, e := range items {
				ui.queueLogf("%s: %s  %s -> %s …", strings.ToLower(verb), e.Name,
					srcSess.Label(), dstSess.Label())
				err := CopyItem(srcSess, srcCwd, e.Name, dstSess, dstCwd)
				if err == nil && move {
					err = DeleteItem(srcSess, srcCwd, e.Name)
				}
				if err == nil {
					ok++
				} else {
					ui.queueLogf("  FAILED %s: %v", e.Name, err)
				}
			}
			ui.queueLogf("%sd %d/%d item(s).", verb, ok, len(items))
			ui.app.QueueUpdateDraw(func() {
				// reload both panes (also clears marks so they can't go stale)
				ui.loadPane(src, srcCwd)
				ui.loadPane(dst, dstCwd)
			})
		}()
	})
}

func (ui *UI) deleteDialog() {
	pane := ui.active
	items := pane.selection()
	if len(items) == 0 {
		return
	}
	names := make([]string, 0, 5)
	for i, e := range items {
		if i == 5 {
			names = append(names, "…")
			break
		}
		names = append(names, e.Name)
	}
	msg := fmt.Sprintf("DELETE %d item(s) on %s?\n\n%s\n\nThis cannot be undone.",
		len(items), pane.session.Label(), strings.Join(names, ", "))
	ui.confirm(msg, func() {
		sess, cwd := pane.session, pane.cwd
		go func() {
			for _, e := range items {
				if err := DeleteItem(sess, cwd, e.Name); err != nil {
					ui.queueLogf("rm %s FAILED: %v", e.Name, err)
				}
			}
			ui.queueLogf("Deleted %d item(s).", len(items))
			ui.app.QueueUpdateDraw(func() { ui.loadPane(pane, cwd) })
		}()
	})
}

func (ui *UI) mkdirDialog() {
	pane := ui.active
	ui.prompt(fmt.Sprintf("New directory in %s:%s", pane.session.Label(), pane.cwd), func(name string) {
		sess, cwd := pane.session, pane.cwd
		go func() {
			if err := MkdirItem(sess, cwd, name); err != nil {
				ui.queueLogf("mkdir %s: %v", name, err)
			} else {
				ui.queueLogf("mkdir %s: ok", name)
			}
			ui.app.QueueUpdateDraw(func() { ui.loadPane(pane, cwd) })
		}()
	})
}

func (ui *UI) viewFile() {
	pane := ui.active
	e := pane.cursorEntry()
	if e == nil || e.IsDir {
		return
	}
	sess, p := pane.session, joinPath(pane.session, pane.cwd, e.Name)
	go func() {
		var text string
		if sess.IsLocal() {
			data, err := os.ReadFile(p)
			if err != nil {
				text = err.Error()
			} else {
				if len(data) > 20000 {
					data = data[:20000]
				}
				text = string(data)
			}
		} else {
			_, out, errText := sess.SSHRun("head -c 20000 -- "+shQuote(p), 30*time.Second)
			text = out
			if text == "" {
				text = errText
			}
		}
		lines := strings.Split(text, "\n")
		if len(lines) > 200 {
			lines = lines[:200]
		}
		ui.queueLogf("----- %s (first 20 KB) -----\n%s\n----- end -----", p, strings.Join(lines, "\n"))
	}()
}

// ---------------------------------------------------------------- command bar

func (ui *UI) runCommand(cmdStr string) {
	pane := ui.active
	if cmdStr == "cd" || strings.HasPrefix(cmdStr, "cd ") {
		arg := strings.TrimSpace(strings.TrimPrefix(cmdStr, "cd"))
		abs := strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "~")
		if pane.session.IsLocal() {
			abs = abs || filepath.IsAbs(arg)
		}
		if arg == "" {
			if pane.session.IsLocal() {
				arg = "~"
			}
		} else if !abs {
			arg = joinPath(pane.session, pane.cwd, arg)
		}
		ui.loadPane(pane, arg)
		return
	}
	sess, cwd := pane.session, pane.cwd
	ui.logf("[%s %s]$ %s", sess.Label(), cwd, cmdStr)
	go func() {
		var rc int
		var out string
		if sess.IsLocal() {
			var cmd *exec.Cmd
			if runtime.GOOS == "windows" {
				cmd = exec.Command("cmd", "/c", cmdStr)
			} else {
				cmd = exec.Command("sh", "-c", cmdStr)
			}
			cmd.Dir = cwd
			b, err := cmd.CombinedOutput()
			out = string(b)
			if ee, ok := err.(*exec.ExitError); ok {
				rc = ee.ExitCode()
			} else if err != nil {
				rc = 1
			}
		} else {
			var o, e string
			rc, o, e = sess.SSHRun(fmt.Sprintf("cd %s && (%s)", shQuote(cwd), cmdStr), 2*time.Minute)
			out = o + e
		}
		out = strings.TrimSpace(out)
		if len(out) > 20000 {
			out = out[:20000] + "\n… (truncated)"
		}
		if out != "" {
			ui.queueLogf("%s", out)
		}
		ui.queueLogf("(exit %d)", rc)
		ui.app.QueueUpdateDraw(func() { ui.loadPane(pane, cwd) })
	}()
}

// ---------------------------------------------------------------- startup

// connectPinnedServer connects to the optional pinned server configured via
// SSHMC_PIN_* environment variables (see pinned_server.go), creates its
// ControlMaster (Linux/macOS), and points the right pane at SSHMC_PIN_DIR.
func (ui *UI) connectPinnedServer() {
	cfg, ok := PinnedServerFromEnv(os.Getenv)
	if !ok {
		return
	}
	sess, err := cfg.Session()
	if err != nil {
		ui.logf("pinned server unavailable: %v", err)
		return
	}
	ui.pinnedSession = sess

	ui.logf("Connecting automatically to %s…", sess.Label())
	if runtime.GOOS != "windows" && !sess.MasterAlive() {
		if err := sess.ConnectMaster(); err != nil {
			ui.logf("pinned server connect failed: %v (retry from F2)", err)
			return
		}
	}
	dir := cfg.Dir
	if dir != "" {
		rc, _, errText := sess.SSHRun("test -d "+shQuote(dir), 15*time.Second)
		if rc != 0 {
			message := strings.TrimSpace(errText)
			if message == "" {
				message = fmt.Sprintf("ssh exited %d", rc)
			}
			ui.logf("pinned server connect failed: %s (retry from F2)", message)
			return
		}
	}

	ui.right.session = sess
	ui.right.cwd = dir
	ui.logf("right pane -> %s:%s (automatic)", sess.Label(), dir)
}

func (ui *UI) Run() error {
	ui.logf("sshmc ready. F2/s: attach a pane to an SSH session. Space marks, F5 copies to the other pane.")
	ui.connectPinnedServer()
	if live := DiscoverLiveSessions(); len(live) > 0 {
		targets := make([]string, len(live))
		for i, s := range live {
			targets[i] = s.Target()
		}
		ui.logf("Live ssh sessions detected: %s", strings.Join(targets, ", "))
	}
	ui.setActive(ui.left)
	ui.refreshPanes()
	err := ui.app.Run()
	for _, t := range ui.terms {
		t.Close()
	}
	return err
}
