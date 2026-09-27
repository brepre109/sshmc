#!/usr/bin/env python3
"""sshmc — Midnight-Commander-style dual-pane file manager over live SSH sessions.

Each pane attaches to the local filesystem or to an SSH host (discovered from
running `ssh` processes and ~/.ssh/config). Connections are multiplexed through
a ControlMaster socket so every listing/copy reuses one authenticated channel.
Copies use scp (local<->remote) or a tar pipe (remote<->remote), so two panes on
two different servers can transfer directly through this machine.

Run:  python3 sshmc.py
Keys: Tab switch pane | F2/s session | Enter open | Backspace up | Space mark
      F5/c copy | F6/m move | F7/n mkdir | F8/d delete | F3/v view | F4/r refresh
      : command bar | F10/q quit
"""

from __future__ import annotations

import os
import posixpath
import re
import shlex
import shutil
import stat
import subprocess
import sys
from dataclasses import dataclass, field
from datetime import datetime
from pathlib import Path
from typing import Iterable, Optional

from textual import work
from textual.app import App, ComposeResult
from textual.binding import Binding
from textual.containers import Horizontal, Vertical
from textual.screen import ModalScreen
from textual.widgets import DataTable, Footer, Input, Label, ListItem, ListView, RichLog, Static

# ---------------------------------------------------------------- ssh plumbing

CONTROL_DIR = Path.home() / ".cache" / "sshmc"
CONTROL_DIR.mkdir(parents=True, exist_ok=True)

# One socket per (user, host, port); every ssh/scp invocation shares it.
MUX = [
    "-o", f"ControlPath={CONTROL_DIR}/cm-%C",
    "-o", "ControlMaster=auto",
    "-o", "ControlPersist=600",
]
BATCH = ["-o", "BatchMode=yes", "-o", "ConnectTimeout=10"]

# single-letter ssh options that consume a value
SSH_ARG_FLAGS = set("bcDEeFIiJLlmOopQRSWw")


@dataclass(frozen=True)
class Session:
    """A place a pane can point at: the local FS or an ssh destination."""

    host: str = ""
    user: Optional[str] = None
    port: Optional[str] = None
    identity: Optional[str] = None
    jump: Optional[str] = None
    extra: tuple = ()  # extra ("-o", "...") pairs
    source: str = "local"  # local | live | config | manual

    @property
    def is_local(self) -> bool:
        return self.source == "local"

    @property
    def target(self) -> str:
        if self.user and "@" not in self.host:
            return f"{self.user}@{self.host}"
        return self.host

    @property
    def label(self) -> str:
        if self.is_local:
            return "LOCAL"
        tag = {"live": "•live", "config": "cfg", "manual": ""}.get(self.source, "")
        port = f":{self.port}" if self.port else ""
        return f"{self.target}{port}" + (f" [{tag}]" if tag else "")

    def ssh_flags(self) -> list[str]:
        out: list[str] = []
        if self.port:
            out += ["-p", self.port]
        if self.identity:
            out += ["-i", self.identity]
        if self.jump:
            out += ["-J", self.jump]
        out += list(self.extra)
        return out

    def scp_flags(self) -> list[str]:
        out: list[str] = []
        if self.port:
            out += ["-P", self.port]
        if self.identity:
            out += ["-i", self.identity]
        if self.jump:
            out += ["-J", self.jump]
        out += list(self.extra)
        return out


LOCAL = Session()


def ssh_run(sess: Session, command: str, timeout: int = 60) -> tuple[int, str, str]:
    """Run a shell command on the session's host through the mux socket."""
    argv = ["ssh", *MUX, *BATCH, *sess.ssh_flags(), sess.target, "--", command]
    proc = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    return proc.returncode, proc.stdout, proc.stderr


def ssh_run_rc_err(sess: Session, command: str, timeout: int = 60) -> tuple[int, str]:
    rc, out, err = ssh_run(sess, command, timeout=timeout)
    return rc, (err or out).strip()


def master_alive(sess: Session) -> bool:
    if sess.is_local:
        return True
    proc = subprocess.run(
        ["ssh", *MUX, *sess.ssh_flags(), "-O", "check", sess.target],
        capture_output=True, text=True, timeout=10,
    )
    return proc.returncode == 0


def parse_ssh_tokens(tokens: list[str], cwd: Optional[str] = None,
                     source: str = "manual") -> Optional[Session]:
    """Parse `ssh ...` argv tokens (after the `ssh` word) into a Session."""
    user = port = identity = jump = None
    extra: list[str] = []
    dest = None
    i = 0
    while i < len(tokens):
        t = tokens[i]
        if t == "--":
            i += 1
            continue
        if t.startswith("-") and len(t) >= 2:
            flag = t[1]
            val = None
            if flag in SSH_ARG_FLAGS:
                if len(t) > 2:
                    val = t[2:]
                else:
                    i += 1
                    val = tokens[i] if i < len(tokens) else None
            if flag == "p":
                port = val
            elif flag == "i":
                identity = val
            elif flag == "l":
                user = val
            elif flag == "J":
                jump = val
            elif flag == "o" and val and not re.match(
                    r"(?i)\s*(batchmode|controlpath|controlmaster|controlpersist)", val):
                extra += ["-o", val]
            elif flag == "F" and val:
                extra += ["-F", val]
            i += 1
            continue
        dest = t  # first non-flag word is the destination; ignore remote command
        break
    if not dest:
        return None
    if identity:
        expanded = os.path.expanduser(identity)
        if not os.path.isabs(expanded) and cwd:
            identity = os.path.join(cwd, identity)
        else:
            identity = expanded
    return Session(host=dest, user=user, port=port, identity=identity,
                   jump=jump, extra=tuple(extra), source=source)


def discover_live_sessions() -> list[Session]:
    """Find ssh client processes owned by anyone on this machine."""
    try:
        out = subprocess.run(["ps", "-eo", "pid=,args="], capture_output=True,
                             text=True, timeout=10).stdout
    except Exception:
        return []
    found: list[Session] = []
    seen = set()
    for line in out.splitlines():
        line = line.strip()
        m = re.match(r"(\d+)\s+(.*)", line)
        if not m:
            continue
        pid, args = m.group(1), m.group(2)
        try:
            tokens = shlex.split(args)
        except ValueError:
            tokens = args.split()
        if not tokens:
            continue
        prog = os.path.basename(tokens[0])
        if prog != "ssh":  # excludes sshd, ssh-agent, mux masters ("ssh: ... [mux]")
            continue
        cwd = None
        try:
            cwd = os.readlink(f"/proc/{pid}/cwd")
        except OSError:
            pass
        sess = parse_ssh_tokens(tokens[1:], cwd=cwd, source="live")
        if sess and (key := (sess.target, sess.port)) not in seen:
            seen.add(key)
            found.append(sess)
    return found


def config_hosts() -> list[Session]:
    """Non-wildcard Host aliases from ~/.ssh/config."""
    path = Path.home() / ".ssh" / "config"
    hosts: list[Session] = []
    seen = set()
    try:
        text = path.read_text()
    except OSError:
        return []
    for line in text.splitlines():
        m = re.match(r"(?i)^\s*host\s+(.+?)\s*$", line)
        if not m:
            continue
        for alias in m.group(1).split():
            if any(ch in alias for ch in "*?!") or alias in seen:
                continue
            seen.add(alias)
            hosts.append(Session(host=alias, source="config"))
    return hosts


# ---------------------------------------------------------------- file listing

@dataclass
class Entry:
    name: str
    is_dir: bool
    is_link: bool = False
    size: Optional[int] = None
    mtime_str: str = ""


def human_size(n: Optional[int]) -> str:
    if n is None:
        return "?"
    for unit in ("B", "K", "M", "G", "T"):
        if n < 1024 or unit == "T":
            return f"{n:.0f}{unit}" if unit == "B" else f"{n:.1f}{unit}"
        n /= 1024
    return "?"


def sort_entries(entries: list[Entry]) -> list[Entry]:
    return sorted(entries, key=lambda e: (not e.is_dir, e.name.lower()))


def list_local(path: str) -> tuple[str, list[Entry]]:
    real = os.path.realpath(os.path.expanduser(path or "~"))
    entries = []
    with os.scandir(real) as it:
        for de in it:
            try:
                st = de.stat(follow_symlinks=False)
                is_link = stat.S_ISLNK(st.st_mode)
                is_dir = de.is_dir(follow_symlinks=True)
                size = None if is_dir else st.st_size
                mtime = datetime.fromtimestamp(st.st_mtime).strftime("%Y-%m-%d %H:%M")
            except OSError:
                is_link, is_dir, size, mtime = False, False, None, ""
            entries.append(Entry(de.name, is_dir, is_link, size, mtime))
    return real, sort_entries(entries)


LS_EPOCH = re.compile(r"^([\-dlbcpsD])\S{8,}\s+\d+\s+\S+\s+\S+\s+(\d+)\s+(\d+)\s+(.+)$")
LS_PLAIN = re.compile(r"^([\-dlbcpsD])\S{8,}\s+\d+\s+\S+\s+\S+\s+(\d+)\s+(\S+\s+\S+\s+\S+)\s+(.+)$")


def list_remote(sess: Session, path: str) -> tuple[str, list[Entry]]:
    q = shlex.quote(path) if path else ""
    cmd = (f"cd {q} && pwd && "
           f"(LC_ALL=C ls -lA --time-style=+%s . 2>/dev/null || LC_ALL=C ls -lA .)")
    rc, out, err = ssh_run(sess, cmd)
    lines = out.splitlines()
    if rc != 0 and not lines:
        raise RuntimeError(err.strip() or f"ssh exited {rc}")
    real = lines[0].strip() if lines else path
    entries: list[Entry] = []
    for line in lines[1:]:
        if not line or line.startswith("total"):
            continue
        m = LS_EPOCH.match(line)
        if m:
            kind, size, epoch, name = m.groups()
            mtime = datetime.fromtimestamp(int(epoch)).strftime("%Y-%m-%d %H:%M")
        else:
            m = LS_PLAIN.match(line)
            if not m:
                continue
            kind, size, mtime, name = m.groups()
        is_link = kind == "l"
        if is_link and " -> " in name:
            name = name.split(" -> ")[0]
        is_dir = kind == "d"
        entries.append(Entry(name, is_dir, is_link,
                             None if is_dir else int(size), mtime))
    return real, sort_entries(entries)


def list_dir(sess: Session, path: str) -> tuple[str, list[Entry]]:
    return list_local(path) if sess.is_local else list_remote(sess, path)


# ---------------------------------------------------------------- transfers

def run_argv(argv: list[str], timeout: int = 3600) -> tuple[int, str]:
    proc = subprocess.run(argv, capture_output=True, text=True, timeout=timeout)
    return proc.returncode, (proc.stderr or proc.stdout).strip()


def copy_item(src: Session, src_dir: str, name: str,
              dst: Session, dst_dir: str) -> tuple[int, str]:
    """Copy one file/dir between any combination of local and remote panes."""
    sp = posixpath.join(src_dir, name)
    if src.is_local and dst.is_local:
        return run_argv(["cp", "-a", sp, dst_dir + "/"])
    if not src.is_local and not dst.is_local:
        if (src.target, src.port) == (dst.target, dst.port):
            return ssh_run_rc_err(src, f"cp -a {shlex.quote(sp)} {shlex.quote(dst_dir)}/")
        # server -> server: tar pipe through this machine; each leg keeps its
        # own flags (ports/keys), which scp -3 cannot express per host.
        reader = subprocess.Popen(
            ["ssh", *MUX, *BATCH, *src.ssh_flags(), src.target, "--",
             f"tar cf - -C {shlex.quote(src_dir)} {shlex.quote(name)}"],
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        writer = subprocess.Popen(
            ["ssh", *MUX, *BATCH, *dst.ssh_flags(), dst.target, "--",
             f"tar xf - -C {shlex.quote(dst_dir)}"],
            stdin=reader.stdout, stderr=subprocess.PIPE)
        reader.stdout.close()
        w_err = writer.communicate()[1]
        r_err = reader.stderr.read()
        reader.wait()
        rc = reader.returncode or writer.returncode
        return rc, ((r_err or b"") + (w_err or b"")).decode(errors="replace").strip()
    # local <-> remote via scp
    argv = ["scp", *MUX, *BATCH, "-r", "-q"]
    if src.is_local:
        argv += dst.scp_flags() + [sp, f"{dst.target}:{shlex.quote(dst_dir)}/"]
    else:
        argv += src.scp_flags() + [f"{src.target}:{shlex.quote(sp)}", dst_dir + "/"]
    return run_argv(argv)


def delete_item(sess: Session, directory: str, name: str) -> tuple[int, str]:
    p = posixpath.join(directory, name)
    if sess.is_local:
        try:
            if os.path.isdir(p) and not os.path.islink(p):
                shutil.rmtree(p)
            else:
                os.remove(p)
            return 0, ""
        except OSError as exc:
            return 1, str(exc)
    return ssh_run_rc_err(sess, f"rm -rf -- {shlex.quote(p)}")


def mkdir_item(sess: Session, directory: str, name: str) -> tuple[int, str]:
    p = posixpath.join(directory, name)
    if sess.is_local:
        try:
            os.makedirs(p, exist_ok=False)
            return 0, ""
        except OSError as exc:
            return 1, str(exc)
    return ssh_run_rc_err(sess, f"mkdir -p -- {shlex.quote(p)}")


# ---------------------------------------------------------------- modal screens

class SessionPicker(ModalScreen[Optional[Session]]):
    """Choose local FS, a live ssh session, a config host, or manual entry."""

    BINDINGS = [Binding("escape", "dismiss(None)", "Cancel")]

    def __init__(self, side: str) -> None:
        super().__init__()
        self.side = side
        self.options: list[Optional[Session]] = []

    def compose(self) -> ComposeResult:
        items: list[ListItem] = []

        def add(label: str, sess: Optional[Session]) -> None:
            self.options.append(sess)
            items.append(ListItem(Label(label)))

        add("Local filesystem", LOCAL)
        live = discover_live_sessions()
        for s in live:
            add(f"live:  {s.target}" + (f" -p {s.port}" if s.port else ""), s)
        for s in config_hosts():
            add(f"conf:  {s.host}", s)
        add("Enter destination manually (user@host ...)", None)
        with Vertical(id="picker-box"):
            yield Label(f"Attach {self.side} pane to:", id="picker-title")
            yield ListView(*items, id="picker-list")

    def on_list_view_selected(self, event: ListView.Selected) -> None:
        idx = self.query_one(ListView).index or 0
        choice = self.options[idx]
        if choice is None and idx == len(self.options) - 1:
            self.app.push_screen(PromptScreen("ssh destination:", "user@host [-p N] [-i key]"),
                                 self._manual)
        else:
            self.dismiss(choice)

    def _manual(self, text: Optional[str]) -> None:
        if not text:
            self.dismiss(None)
            return
        tokens = shlex.split(text)
        if tokens and tokens[0] == "ssh":
            tokens = tokens[1:]
        self.dismiss(parse_ssh_tokens(tokens))


class PromptScreen(ModalScreen[Optional[str]]):
    BINDINGS = [Binding("escape", "dismiss(None)", "Cancel")]

    def __init__(self, question: str, placeholder: str = "") -> None:
        super().__init__()
        self.question, self.placeholder = question, placeholder

    def compose(self) -> ComposeResult:
        with Vertical(id="prompt-box"):
            yield Label(self.question)
            yield Input(placeholder=self.placeholder)

    def on_mount(self) -> None:
        self.query_one(Input).focus()

    def on_input_submitted(self, event: Input.Submitted) -> None:
        self.dismiss(event.value.strip() or None)


class ConfirmScreen(ModalScreen[bool]):
    BINDINGS = [
        Binding("escape,n", "dismiss(False)", "No"),
        Binding("enter,y", "confirm", "Yes", priority=True),
    ]

    def __init__(self, message: str) -> None:
        super().__init__()
        self.message = message

    def compose(self) -> ComposeResult:
        with Vertical(id="confirm-box"):
            yield Label(self.message)
            yield Label("[b]Enter/y[/b] = yes    [b]Esc/n[/b] = no", id="confirm-hint")

    def action_confirm(self) -> None:
        self.dismiss(True)


# ---------------------------------------------------------------- panes

class PaneTable(DataTable):
    """DataTable that hands space/insert (mark) back to the app."""

    def on_key(self, event) -> None:
        if event.key in ("space", "insert"):
            event.stop()
            event.prevent_default()
            self.app.action_mark()


class FilePane(Vertical):
    """One half of the dual-pane view: title bar + file table."""

    def __init__(self, pane_id: str) -> None:
        super().__init__(id=pane_id)
        self.session: Session = LOCAL
        self.cwd: str = str(Path.home())
        self.entries: list[Entry] = []
        self.marked: set[str] = set()

    def compose(self) -> ComposeResult:
        yield Static("", classes="pane-title")
        table = PaneTable(cursor_type="row")
        table.add_column(" ", key="mark", width=1)
        table.add_column("Name", key="name", width=34)
        table.add_column("Size", key="size", width=8)
        table.add_column("Modified", key="mtime", width=16)
        yield table

    @property
    def table(self) -> DataTable:
        return self.query_one(DataTable)

    def title_text(self) -> str:
        n = f"  ({len(self.marked)} marked)" if self.marked else ""
        return f" {self.session.label}  {self.cwd}{n}"

    def refresh_title(self) -> None:
        self.query_one(".pane-title", Static).update(self.title_text())

    def populate(self, real: str, entries: list[Entry]) -> None:
        self.cwd = real
        self.entries = entries
        self.marked.clear()
        t = self.table
        t.clear()
        if real != "/":
            t.add_row("", "..", "", "", key="\0..")
        for e in entries:
            deco = "/" if e.is_dir else ("@" if e.is_link else "")
            t.add_row("", e.name + deco,
                      "<DIR>" if e.is_dir else human_size(e.size),
                      e.mtime_str, key=e.name)
        self.refresh_title()

    def cursor_entry(self) -> Optional[Entry]:
        row = self.table.cursor_row
        offset = 0 if self.cwd == "/" else 1
        idx = row - offset
        if idx < 0 or idx >= len(self.entries):
            return None
        return self.entries[idx]

    def selection(self) -> list[Entry]:
        if self.marked:
            return [e for e in self.entries if e.name in self.marked]
        e = self.cursor_entry()
        return [e] if e else []

    def toggle_mark(self) -> None:
        e = self.cursor_entry()
        if not e:  # ".." row: just advance, like mc
            self.table.move_cursor(row=self.table.cursor_row + 1)
            return
        if e.name in self.marked:
            self.marked.discard(e.name)
            self.table.update_cell(e.name, "mark", "")
        else:
            self.marked.add(e.name)
            self.table.update_cell(e.name, "mark", "*")
        self.refresh_title()
        self.table.move_cursor(row=self.table.cursor_row + 1)


# ---------------------------------------------------------------- application

class SshCommander(App):
    TITLE = "sshmc — dual-pane SSH file commander"

    CSS = """
    #panes { height: 1fr; }
    FilePane { width: 1fr; border: round $panel; }
    FilePane.active { border: round $accent; }
    .pane-title { height: 1; background: $boost; color: $text; text-style: bold; }
    DataTable { height: 1fr; }
    #log { height: 8; border-top: hkey $panel; padding: 0 1; }
    #cmd { dock: bottom; }
    #picker-box, #prompt-box, #confirm-box {
        width: 70; max-width: 90%; height: auto; max-height: 80%;
        background: $surface; border: thick $accent; padding: 1 2;
    }
    SessionPicker, PromptScreen, ConfirmScreen { align: center middle; }
    #picker-list { height: auto; max-height: 20; }
    #confirm-hint { margin-top: 1; color: $text-muted; }
    """

    BINDINGS = [
        Binding("tab", "switch_pane", "Switch", priority=True),
        Binding("f2,s", "pick_session", "Session"),
        Binding("f5,c", "copy", "Copy"),
        Binding("f6,m", "move", "Move"),
        Binding("f7,n", "mkdir", "Mkdir"),
        Binding("f8,d", "delete", "Delete"),
        Binding("f3,v", "view", "View"),
        Binding("f4,r", "refresh", "Refresh"),
        Binding("space", "mark", "Mark", show=False),
        Binding("backspace", "updir", "Up", show=False),
        Binding("colon,grave_accent", "focus_cmd", "Cmd"),
        Binding("escape", "focus_files", show=False),
        Binding("f10,q", "quit", "Quit"),
    ]

    def __init__(self) -> None:
        super().__init__()
        self.active_pane_id = "left"

    # -- layout

    def compose(self) -> ComposeResult:
        with Horizontal(id="panes"):
            yield FilePane("left")
            yield FilePane("right")
        yield RichLog(id="log", markup=False, highlight=False, wrap=True, max_lines=2000)
        yield Input(id="cmd", placeholder="command… (runs on active pane; `cd` changes dir; Esc back to files)")
        yield Footer()

    def on_mount(self) -> None:
        self.log_line("sshmc ready. F2/s: attach a pane to an SSH session. "
                      "Space marks files, F5 copies to the other pane.")
        live = discover_live_sessions()
        if live:
            self.log_line("Live ssh sessions detected: "
                          + ", ".join(s.target for s in live))
        for pane in self.query(FilePane):
            self.load_pane(pane, pane.cwd)
        self.pane("left").table.focus()

    # -- helpers

    def pane(self, pane_id: str) -> FilePane:
        return self.query_one(f"#{pane_id}", FilePane)

    @property
    def active(self) -> FilePane:
        return self.pane(self.active_pane_id)

    @property
    def other(self) -> FilePane:
        return self.pane("right" if self.active_pane_id == "left" else "left")

    def log_line(self, text: str) -> None:
        self.query_one("#log", RichLog).write(text)

    def on_descendant_focus(self, event) -> None:
        widget = event.widget
        for pane_id in ("left", "right"):
            pane = self.pane(pane_id)
            if widget in pane.walk_children() or widget is pane:
                self.active_pane_id = pane_id
        for pane_id in ("left", "right"):
            self.pane(pane_id).set_class(pane_id == self.active_pane_id, "active")

    # -- loading

    @work(thread=True, group="load")
    def load_pane(self, pane: FilePane, path: str) -> None:
        try:
            real, entries = list_dir(pane.session, path)
        except Exception as exc:
            self.call_from_thread(self.log_line, f"[{pane.session.label}] cannot open {path}: {exc}")
            return
        self.call_from_thread(pane.populate, real, entries)

    def attach_session(self, pane: FilePane, sess: Optional[Session]) -> None:
        if sess is None:
            return
        if not sess.is_local and not master_alive(sess):
            with self.suspend():
                print(f"\n== sshmc: connecting to {sess.target} "
                      f"(answer any host-key/passphrase prompts) ==\n")
                rc = subprocess.call(["ssh", *MUX, "-fN", *sess.ssh_flags(), sess.target])
            if rc != 0 or not master_alive(sess):
                self.log_line(f"Connection to {sess.target} failed (exit {rc}).")
                return
        pane.session = sess
        start = str(Path.home()) if sess.is_local else ""
        self.log_line(f"{pane.id} pane -> {sess.label}")
        self.load_pane(pane, start)

    # -- actions

    def action_switch_pane(self) -> None:
        self.other.table.focus()

    def action_pick_session(self) -> None:
        pane = self.active
        self.push_screen(SessionPicker(pane.id or ""),
                         lambda sess: self.attach_session(pane, sess))

    def action_refresh(self) -> None:
        for pane in self.query(FilePane):
            self.load_pane(pane, pane.cwd)

    def action_updir(self) -> None:
        pane = self.active
        parent = posixpath.dirname(pane.cwd.rstrip("/")) or "/"
        self.load_pane(pane, parent)

    def action_mark(self) -> None:
        self.active.toggle_mark()

    def action_focus_cmd(self) -> None:
        self.query_one("#cmd", Input).focus()

    def action_focus_files(self) -> None:
        self.active.table.focus()

    def on_data_table_row_selected(self, event: DataTable.RowSelected) -> None:
        node = event.data_table.parent
        pane = node if isinstance(node, FilePane) else self.active
        key = event.row_key.value
        if key == "\0..":
            self.action_updir()
            return
        entry = next((e for e in pane.entries if e.name == key), None)
        if entry is None:
            return
        if entry.is_dir or entry.is_link:
            self.load_pane(pane, posixpath.join(pane.cwd, entry.name))
        else:
            self.view_file(pane, entry)

    def action_view(self) -> None:
        pane = self.active
        entry = pane.cursor_entry()
        if entry and not entry.is_dir:
            self.view_file(pane, entry)

    @work(thread=True, group="view")
    def view_file(self, pane: FilePane, entry: Entry) -> None:
        p = posixpath.join(pane.cwd, entry.name)
        try:
            if pane.session.is_local:
                with open(p, "rb") as fh:
                    data = fh.read(20000)
                text = data.decode(errors="replace")
            else:
                rc, out, err = ssh_run(pane.session, f"head -c 20000 -- {shlex.quote(p)}")
                text = out if rc == 0 else err
        except Exception as exc:
            text = str(exc)
        self.call_from_thread(self._show_view, p, text)

    def _show_view(self, path: str, text: str) -> None:
        self.log_line(f"----- {path} (first 20 KB) -----")
        for line in text.splitlines()[:200]:
            self.log_line(line)
        self.log_line("----- end -----")

    # -- transfers

    def _transfer(self, move: bool) -> None:
        src, dst = self.active, self.other
        items = src.selection()
        if not items:
            self.log_line("Nothing selected.")
            return
        verb = "Move" if move else "Copy"
        names = ", ".join(e.name for e in items[:5]) + (" …" if len(items) > 5 else "")
        msg = (f"{verb} {len(items)} item(s): {names}\n\n"
               f"from  {src.session.label}:{src.cwd}\n"
               f"to    {dst.session.label}:{dst.cwd}\n\n"
               f"(existing files are overwritten)")
        self.push_screen(ConfirmScreen(msg),
                         lambda ok: self.transfer_items(src, dst, items, move) if ok else None)

    def action_copy(self) -> None:
        self._transfer(move=False)

    def action_move(self) -> None:
        self._transfer(move=True)

    @work(thread=True, group="transfer")
    def transfer_items(self, src: FilePane, dst: FilePane,
                       items: list[Entry], move: bool) -> None:
        verb = "mv" if move else "cp"
        ok = 0
        for e in items:
            self.call_from_thread(self.log_line,
                                  f"{verb}: {e.name}  {src.session.label} -> {dst.session.label} …")
            rc, err = copy_item(src.session, src.cwd, e.name, dst.session, dst.cwd)
            if rc == 0 and move:
                rc, err = delete_item(src.session, src.cwd, e.name)
            if rc == 0:
                ok += 1
            else:
                self.call_from_thread(self.log_line, f"  FAILED {e.name}: {err}")
        self.call_from_thread(self.log_line,
                              f"{'Moved' if move else 'Copied'} {ok}/{len(items)} item(s).")
        # reload both panes (also clears marks so they can't be reused stale)
        self.call_from_thread(self.load_pane, dst, dst.cwd)
        self.call_from_thread(self.load_pane, src, src.cwd)

    def action_mkdir(self) -> None:
        pane = self.active

        def done(name: Optional[str]) -> None:
            if not name:
                return
            rc, err = mkdir_item(pane.session, pane.cwd, name)
            self.log_line(f"mkdir {name}: {'ok' if rc == 0 else err}")
            self.load_pane(pane, pane.cwd)

        self.push_screen(PromptScreen(f"New directory in {pane.session.label}:{pane.cwd}"), done)

    def action_delete(self) -> None:
        pane = self.active
        items = pane.selection()
        if not items:
            return
        names = ", ".join(e.name for e in items[:5]) + (" …" if len(items) > 5 else "")
        msg = f"DELETE {len(items)} item(s) on {pane.session.label}?\n\n{names}\n\nThis cannot be undone."
        self.push_screen(ConfirmScreen(msg),
                         lambda ok: self.delete_items(pane, items) if ok else None)

    @work(thread=True, group="delete")
    def delete_items(self, pane: FilePane, items: list[Entry]) -> None:
        for e in items:
            rc, err = delete_item(pane.session, pane.cwd, e.name)
            if rc != 0:
                self.call_from_thread(self.log_line, f"rm {e.name} FAILED: {err}")
        self.call_from_thread(self.log_line, f"Deleted {len(items)} item(s).")
        self.call_from_thread(self.load_pane, pane, pane.cwd)

    # -- command bar

    def on_input_submitted(self, event: Input.Submitted) -> None:
        if event.input.id != "cmd":
            return
        cmd = event.value.strip()
        event.input.value = ""
        if not cmd:
            return
        pane = self.active
        if cmd == "cd" or cmd.startswith("cd "):
            arg = cmd[2:].strip() or ("~" if pane.session.is_local else "")
            target = arg if arg.startswith(("/", "~")) or not arg else posixpath.join(pane.cwd, arg)
            self.load_pane(pane, target)
            return
        self.log_line(f"[{pane.session.label} {pane.cwd}]$ {cmd}")
        self.run_command(pane, cmd)

    @work(thread=True, group="cmd")
    def run_command(self, pane: FilePane, cmd: str) -> None:
        try:
            if pane.session.is_local:
                proc = subprocess.run(cmd, shell=True, cwd=pane.cwd,
                                      capture_output=True, text=True, timeout=120)
                rc, out, err = proc.returncode, proc.stdout, proc.stderr
            else:
                rc, out, err = ssh_run(pane.session,
                                       f"cd {shlex.quote(pane.cwd)} && ({cmd})", timeout=120)
        except Exception as exc:
            rc, out, err = 1, "", str(exc)
        text = (out + err).strip()
        if len(text) > 20000:
            text = text[:20000] + "\n… (truncated)"
        for line in text.splitlines():
            self.call_from_thread(self.log_line, line)
        self.call_from_thread(self.log_line, f"(exit {rc})")
        self.call_from_thread(self.load_pane, pane, pane.cwd)


def main() -> None:
    SshCommander().run()


if __name__ == "__main__":
    main()
