package main

// Term is an embedded live terminal: a real PTY running a local shell or an
// interactive ssh session (over the mux socket), rendered through a vt10x
// emulator as a tview primitive. One Term is kept per attached connection.

import (
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/hinshun/vt10x"
	"github.com/rivo/tview"
)

// ptySession abstracts the OS pseudo-terminal: creack/pty on Unix
// (pty_unix.go), ConPTY on Windows (pty_windows.go).
type ptySession interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Kill()     // force-terminate the child process
	WaitDone() // block until the child has exited
}

type Term struct {
	*tview.Box
	app    *tview.Application
	vt     vt10x.Terminal
	pty    ptySession
	onDead func(*Term)

	mu         sync.Mutex
	curW, curH int
	dead       bool

	drawPending int32
}

func NewTerm(app *tview.Application, label string, argv []string, dir string, onDead func(*Term)) (*Term, error) {
	p, err := startPty(argv, dir, 80, 24)
	if err != nil {
		return nil, err
	}
	t := &Term{
		Box:    tview.NewBox(),
		app:    app,
		pty:    p,
		onDead: onDead,
		curW:   80,
		curH:   24,
	}
	t.SetBorder(true).SetTitle(" " + label + " — F9 back to files ")
	t.vt = vt10x.New(vt10x.WithSize(80, 24), vt10x.WithWriter(p))
	go t.readLoop()
	return t, nil
}

func (t *Term) readLoop() {
	buf := make([]byte, 8192)
	for {
		n, err := t.pty.Read(buf)
		if n > 0 {
			t.vt.Write(buf[:n])
			t.scheduleDraw()
		}
		if err != nil {
			break
		}
	}
	t.mu.Lock()
	t.dead = true
	t.mu.Unlock()
	t.pty.WaitDone()
	if t.onDead != nil {
		t.app.QueueUpdateDraw(func() { t.onDead(t) })
	}
}

// scheduleDraw coalesces PTY output bursts into ~40 fps redraws.
func (t *Term) scheduleDraw() {
	if atomic.CompareAndSwapInt32(&t.drawPending, 0, 1) {
		time.AfterFunc(25*time.Millisecond, func() {
			atomic.StoreInt32(&t.drawPending, 0)
			t.app.QueueUpdateDraw(func() {})
		})
	}
}

func (t *Term) Dead() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.dead
}

func (t *Term) Close() {
	t.pty.Kill()
	t.pty.Close()
}

func (t *Term) Draw(screen tcell.Screen) {
	t.Box.DrawForSubclass(screen, t)
	x, y, w, h := t.GetInnerRect()
	if w < 1 || h < 1 {
		return
	}
	t.mu.Lock()
	if (w != t.curW || h != t.curH) && !t.dead {
		t.curW, t.curH = w, h
		t.pty.Resize(w, h)
		t.vt.Resize(w, h)
	}
	t.mu.Unlock()

	t.vt.Lock()
	defer t.vt.Unlock()
	cols, rows := t.vt.Size()
	for row := 0; row < rows && row < h; row++ {
		for col := 0; col < cols && col < w; col++ {
			g := t.vt.Cell(col, row)
			st := tcell.StyleDefault.
				Foreground(vtColor(g.FG, tcell.ColorDefault)).
				Background(vtColor(g.BG, tcell.ColorDefault))
			// vt10x attr bits are unexported; values fixed in pinned version:
			// reverse=1, underline=2, bold=4
			if g.Mode&1 != 0 {
				st = st.Reverse(true)
			}
			if g.Mode&2 != 0 {
				st = st.Underline(true)
			}
			if g.Mode&4 != 0 {
				st = st.Bold(true)
			}
			ch := g.Char
			if ch == 0 {
				ch = ' '
			}
			screen.SetContent(x+col, y+row, ch, nil, st)
		}
	}
	if t.HasFocus() && t.vt.CursorVisible() {
		cur := t.vt.Cursor()
		if cur.X >= 0 && cur.X < w && cur.Y >= 0 && cur.Y < h {
			screen.ShowCursor(x+cur.X, y+cur.Y)
		}
	}
}

func vtColor(c vt10x.Color, def tcell.Color) tcell.Color {
	switch {
	case c == vt10x.DefaultFG, c == vt10x.DefaultBG, c == vt10x.DefaultCursor:
		return def
	case c < 256:
		return tcell.PaletteColor(int(c))
	case c < 1<<24: // truecolor packed as RGB
		return tcell.NewHexColor(int32(c))
	default:
		return def
	}
}

// Feed translates a tcell key event to bytes on the PTY. The global input
// capture routes all keys here while the terminal has focus (so tview's
// Ctrl-C quit and our app hotkeys never fire inside the shell).
func (t *Term) Feed(ev *tcell.EventKey) {
	if t.Dead() {
		return
	}
	if b := keyBytes(ev); len(b) > 0 {
		t.pty.Write(b)
	}
}

func (t *Term) InputHandler() func(*tcell.EventKey, func(tview.Primitive)) {
	return t.WrapInputHandler(func(ev *tcell.EventKey, _ func(tview.Primitive)) {
		t.Feed(ev)
	})
}

// MouseHandler focuses the terminal on click (a bare Box ignores clicks).
func (t *Term) MouseHandler() func(tview.MouseAction, *tcell.EventMouse, func(tview.Primitive)) (bool, tview.Primitive) {
	return t.WrapMouseHandler(func(action tview.MouseAction, ev *tcell.EventMouse, setFocus func(tview.Primitive)) (bool, tview.Primitive) {
		if action == tview.MouseLeftDown && t.InRect(ev.Position()) {
			setFocus(t)
			return true, nil
		}
		return false, nil
	})
}

func keyBytes(ev *tcell.EventKey) []byte {
	k := ev.Key()

	// tcell >= 2.9 normalizes Ctrl+letter to Key('LETTER')+ModCtrl instead of
	// the legacy control-code keys (KeyCtrlC etc.) — handle both forms, or
	// every Ctrl combo silently vanishes inside the shell.
	if ev.Modifiers()&tcell.ModCtrl != 0 {
		switch {
		case k >= 'a' && k <= 'z':
			return []byte{byte(k) - 'a' + 1}
		case k >= 'A' && k <= 'Z':
			return []byte{byte(k) - 'A' + 1}
		case k == ' ':
			return []byte{0} // Ctrl+Space
		case k >= 1 && k <= 26:
			return []byte{byte(k)} // legacy form
		case k == tcell.KeyUp:
			return []byte("\x1b[1;5A")
		case k == tcell.KeyDown:
			return []byte("\x1b[1;5B")
		case k == tcell.KeyRight:
			return []byte("\x1b[1;5C")
		case k == tcell.KeyLeft:
			return []byte("\x1b[1;5D")
		case k == tcell.KeyRune:
			r := ev.Rune()
			if r >= 'a' && r <= 'z' {
				return []byte{byte(r) - 'a' + 1}
			}
			if r >= 'A' && r <= 'Z' {
				return []byte{byte(r) - 'A' + 1}
			}
		}
	}

	switch k {
	case tcell.KeyRune:
		b := []byte(string(ev.Rune()))
		if ev.Modifiers()&tcell.ModAlt != 0 {
			b = append([]byte{0x1b}, b...)
		}
		return b
	case tcell.KeyEnter:
		return []byte{'\r'}
	case tcell.KeyTab:
		return []byte{'\t'}
	case tcell.KeyBacktab:
		return []byte("\x1b[Z")
	case tcell.KeyBackspace, tcell.KeyBackspace2:
		return []byte{0x7f}
	case tcell.KeyEsc:
		return []byte{0x1b}
	case tcell.KeyUp:
		return []byte("\x1b[A")
	case tcell.KeyDown:
		return []byte("\x1b[B")
	case tcell.KeyRight:
		return []byte("\x1b[C")
	case tcell.KeyLeft:
		return []byte("\x1b[D")
	case tcell.KeyHome:
		return []byte("\x1b[H")
	case tcell.KeyEnd:
		return []byte("\x1b[F")
	case tcell.KeyPgUp:
		return []byte("\x1b[5~")
	case tcell.KeyPgDn:
		return []byte("\x1b[6~")
	case tcell.KeyDelete:
		return []byte("\x1b[3~")
	case tcell.KeyInsert:
		return []byte("\x1b[2~")
	case tcell.KeyF1:
		return []byte("\x1bOP")
	case tcell.KeyF2:
		return []byte("\x1bOQ")
	case tcell.KeyF3:
		return []byte("\x1bOR")
	case tcell.KeyF4:
		return []byte("\x1bOS")
	case tcell.KeyF5:
		return []byte("\x1b[15~")
	case tcell.KeyF6:
		return []byte("\x1b[17~")
	case tcell.KeyF7:
		return []byte("\x1b[18~")
	case tcell.KeyF8:
		return []byte("\x1b[19~")
	case tcell.KeyF10:
		return []byte("\x1b[21~")
	case tcell.KeyF11:
		return []byte("\x1b[23~")
	case tcell.KeyF12:
		return []byte("\x1b[24~")
	}
	// Ctrl-A..Ctrl-Z arrive as key codes 1..26 (Tab/Enter caught above)
	if k := ev.Key(); k >= 1 && k <= 26 {
		return []byte{byte(k)}
	}
	return nil
}

// shellArgv builds the command for a session's interactive shell.
func shellArgv(sess *Session, cwd string) (argv []string, dir string) {
	if sess.IsLocal() {
		if runtime.GOOS == "windows" {
			return []string{"powershell.exe", "-NoLogo"}, cwd
		}
		sh := os.Getenv("SHELL")
		if sh == "" {
			sh = "bash"
		}
		return []string{sh}, cwd
	}
	args := append([]string{"ssh"}, muxArgs()...)
	args = append(args, sess.sshFlags()...)
	args = append(args, "-t", sess.Target(),
		"cd "+shQuote(cwd)+" 2>/dev/null; exec \"${SHELL:-bash}\" -l")
	return args, ""
}
