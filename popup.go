package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// row is a pane herdr reports an agent in, or an open pane linked to a channel, listed in the status popup.
type row struct {
	ID, Name, Agent, Cwd, Status string
	Mirrored                     bool
	Target                       target
}

// listedAgent is an entry of herdr agent list.
type listedAgent struct {
	agentInfo
	PaneID string `json:"pane_id"`
}

// rows lists the agent panes and the open panes linked to a channel. It does not take state.lock, which a
// toggle or event hook holds across Mattermost calls.
func (a *app) rows() ([]row, error) {
	panes, err := a.readPanes()
	if err != nil {
		return nil, err
	}
	targets, err := a.readTargets()
	if err != nil {
		return nil, err
	}
	labels, hidden, agents, err := a.labels(panes)
	if err != nil {
		return nil, err
	}
	var rows []row
	for _, r := range listRows(agents, panes) {
		if !hidden[r.ID] {
			r.Name, r.Target = labels[r.ID], targets[r.ID]
			rows = append(rows, r)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

// listRows marks which of herdr's agents are mirrored.
func listRows(agents []listedAgent, panes map[string]*pane) []row {
	var rows []row
	for _, ag := range agents {
		_, on := panes[ag.PaneID]
		rows = append(rows, row{ID: ag.PaneID, Agent: ag.Agent, Cwd: ag.Cwd, Status: ag.Status, Mirrored: on})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return rows
}

// status prints whether the daemon runs and the rows, with a cursor on rows[sel], fitted within
// width cells, then the selected row's full name when it was cut, and its full channel and whitelist.
func (a *app) status(w io.Writer, rows []row, sel, width int) error {
	if pid := a.daemonPid(); pid != 0 {
		fmt.Fprintf(w, a.t("popup.running")+"\n\n", pid)
	} else {
		fmt.Fprint(w, a.t("popup.stopped")+"\n\n")
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, a.t("popup.none"))
		return nil
	}
	lists, listsErr := a.readWhitelists()
	whitelist := func(t target, max int) string {
		switch {
		case listsErr != nil:
			return "?"
		case len(lists[t.ID]) == 0:
			return a.t("whitelist.none")
		}
		return usernames(lists[t.ID], max)
	}
	table := [][]string{strings.Split(a.t("popup.header"), "\t")}
	for i, r := range rows {
		cursor, on := " ", a.t("popup.no")
		if i == sel {
			cursor = ">"
		}
		if r.Mirrored {
			on = a.t("popup.yes")
		}
		ch, wl := "", ""
		if r.Target.ID != "" {
			ch, wl = a.targetName(r.Target), whitelist(r.Target, 1)
		}
		table = append(table, []string{cursor, r.Name, r.Agent, emoji[r.Status] + " " + a.t(r.Status), on, ch, wl})
	}
	lines, widths := fitTable(table, width, []int{5, 6}, []int{1})
	for _, line := range lines {
		fmt.Fprintln(w, line)
	}
	var detail [][2]string
	if r := rows[sel]; cells(r.Name) > widths[1] {
		detail = append(detail, [2]string{a.t("popup.name"), r.Name})
	}
	if t := rows[sel].Target; t.ID != "" {
		detail = append(detail, [2]string{a.t("popup.channel"), a.targetName(t)}, [2]string{a.t("popup.allowed"), whitelist(t, len(lists[t.ID]))})
	}
	if len(detail) > 0 {
		pad := 0
		for _, l := range detail {
			pad = max(pad, cells(l[0])+1)
		}
		indent := "  " + strings.Repeat(" ", pad)
		fmt.Fprintln(w)
		for _, l := range detail {
			for i, line := range wrap(l[1], width-len(indent)) {
				if i == 0 {
					fmt.Fprintln(w, "  "+l[0]+strings.Repeat(" ", pad-cells(l[0]))+line)
				} else {
					fmt.Fprintln(w, indent+line)
				}
			}
		}
	}
	return nil
}

// fitTable lays the table out in columns two cells apart, padded by display cells, and until every
// line fits within width cells cuts with … one cell at a time from the widest column of the first
// shrink group, then of the next, never below its header's width. It returns the lines and the
// column widths.
func fitTable(table [][]string, width int, shrink ...[]int) ([]string, []int) {
	widths := make([]int, len(table[0]))
	for _, r := range table {
		for c, cell := range r {
			widths[c] = max(widths[c], cells(cell))
		}
	}
	over := 2*(len(widths)-1) - width
	for _, w := range widths {
		over += w
	}
	for _, group := range shrink {
		for ; over > 0; over-- {
			widest := -1
			for _, c := range group {
				if widths[c] > cells(table[0][c]) && (widest < 0 || widths[c] > widths[widest]) {
					widest = c
				}
			}
			if widest < 0 {
				break
			}
			widths[widest]--
		}
	}
	var lines []string
	for _, r := range table {
		var b strings.Builder
		for c, cell := range r {
			cell = cut(cell, widths[c])
			b.WriteString(cell + strings.Repeat(" ", widths[c]-cells(cell)+2))
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return lines, widths
}

// runeCells is how many cells a terminal draws r in: two for wide characters and emoji, none for
// combining marks and variation selectors, one otherwise.
func runeCells(r rune) int {
	switch {
	case wide(r):
		return 2
	case unicode.Is(unicode.Mn, r):
		return 0
	}
	return 1
}

// cells is how many cells a terminal draws s in.
func cells(s string) int {
	n := 0
	for _, r := range s {
		n += runeCells(r)
	}
	return n
}

// cut shortens s to at most n cells, ending it with … when it had to cut.
func cut(s string, n int) string {
	if cells(s) <= n {
		return s
	}
	var b strings.Builder
	used := 1 // the …
	for _, r := range s {
		if used+runeCells(r) > n {
			break
		}
		used += runeCells(r)
		b.WriteRune(r)
	}
	if n < 1 {
		return ""
	}
	return b.String() + "…"
}

// wrap breaks s into lines of at most n cells, at spaces, splitting a word longer than a line.
func wrap(s string, n int) []string {
	n = max(n, 1)
	var lines []string
	line := ""
	for _, word := range strings.Fields(s) {
		if line != "" && cells(line)+1+cells(word) <= n {
			line += " " + word
			continue
		}
		if line != "" {
			lines = append(lines, line)
		}
		for line = word; cells(line) > n; {
			i, used := 0, 0
			for j, r := range line {
				if used+runeCells(r) > n && used > 0 {
					break
				}
				used += runeCells(r)
				i = j + utf8.RuneLen(r)
			}
			lines = append(lines, line[:i])
			line = line[i:]
		}
	}
	return append(lines, line)
}

// termWidth is how many cells wide the terminal on stdout is, or 80 when it cannot tell.
func termWidth() int {
	if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 0 {
		return int(ws.Col)
	}
	return 80
}

// targetName names a pane's channel, marked public or private, or the unlink choice of the picker for none.
func (a *app) targetName(t target) string {
	switch {
	case t.ID == "":
		return a.t("picker.unlink")
	case t.Private:
		return "🔒 " + t.Name
	}
	return "# " + t.Name
}

// readKeys reads the next keys typed; keys typed quickly arrive in one read. A lone Esc is "\x1b".
func readKeys() ([]string, error) {
	b := make([]byte, 16)
	n, err := os.Stdin.Read(b)
	if err != nil {
		return nil, err
	}
	var keys []string
	for k := string(b[:n]); k != ""; {
		key := k[:1]
		if strings.HasPrefix(k, "\x1b[") && len(k) >= 3 {
			key = k[:3]
		}
		k = k[len(key):]
		keys = append(keys, key)
	}
	return keys, nil
}

// popup redraws the status after every key: arrows or j/k move, Enter or Space toggles the selected
// pane, t picks its channel, w edits its channel's whitelist, s opens the settings, q or Esc closes.
func (a *app) popup() {
	defer rawMode()()
	sel, msg := 0, ""
	for {
		rows, err := a.rows()
		sel = max(min(sel, len(rows)-1), 0)
		fmt.Print("\x1b[H\x1b[2J")
		if err == nil {
			err = a.status(os.Stdout, rows, sel, termWidth())
		}
		if err != nil {
			fmt.Printf(a.t("popup.error")+"\n", err)
		}
		if msg != "" {
			fmt.Printf("\n%s\n", msg)
		}
		fmt.Print("\n" + a.t("popup.hint"))
		keys, err := readKeys()
		if err != nil {
			return
		}
		msg = ""
	keys:
		for _, key := range keys {
			switch key {
			case "q", "\x1b":
				return
			case "t":
				if sel < len(rows) {
					msg = a.pickTarget(rows[sel], false)
				}
				break keys // the picker read the keys that followed
			case "w":
				if sel < len(rows) && rows[sel].Target.ID == "" {
					msg = a.t("whitelist.nochannel")
				} else if sel < len(rows) {
					msg = a.whitelistScreen(rows[sel].Target)
				}
				break keys
			case "s":
				msg = a.settingsScreen()
				break keys
			case "l":
				if err := a.switchLang(); err != nil {
					msg = err.Error()
				}
			case "\x1b[A", "k":
				sel = max(sel-1, 0)
			case "\x1b[B", "j":
				sel = max(min(sel+1, len(rows)-1), 0)
			case "\r", "\n", " ":
				if sel < len(rows) && !rows[sel].Mirrored && rows[sel].Target.ID == "" {
					msg = a.pickTarget(rows[sel], true) // mirroring needs a channel
					break keys
				}
				if sel < len(rows) {
					msg = a.popupToggle(rows[sel])
				}
			}
		}
	}
}

// popupToggle toggles a pane as the toggle action does and returns the error to show, if any.
func (a *app) popupToggle(r row) string {
	fmt.Printf("\n\n"+a.t("popup.toggling"), r.Name)
	err := a.toggle(r.ID)
	if err == nil {
		err = a.start()
	}
	if err != nil {
		return fmt.Sprintf(a.t("popup.failed"), r.Name, err)
	}
	return ""
}

// pickTarget lists the bot's channels not linked to another pane, and an unlink line when the pane has
// a channel, and links the pane to the one picked with Enter, asks for that channel's whitelist, then
// mirrors the pane when mirror is set; q or Esc cancels. It returns the error to show, if any.
func (a *app) pickTarget(r row, mirror bool) string {
	fmt.Printf("\n\n"+a.t("picker.loading"), r.Name)
	opts, err := a.targetOptions(r.ID)
	if err != nil {
		return err.Error()
	}
	if r.Target.ID != "" {
		opts = append(opts, target{})
	}
	if len(opts) == 0 {
		return a.t("picker.none")
	}
	sel := max(slices.IndexFunc(opts, func(t target) bool { return t.ID == r.Target.ID }), 0)
	for {
		fmt.Print("\x1b[H\x1b[2J")
		fmt.Printf(a.t("picker.title")+"\n\n", r.Name)
		for i, t := range opts {
			cursor := " "
			if i == sel {
				cursor = ">"
			}
			fmt.Printf("%s %s\n", cursor, a.targetName(t))
		}
		fmt.Print("\n" + a.t("picker.hint"))
		keys, err := readKeys()
		if err != nil {
			return ""
		}
		for _, key := range keys {
			switch key {
			case "q", "\x1b":
				return ""
			case "\x1b[A", "k":
				sel = max(sel-1, 0)
			case "\x1b[B", "j":
				sel = min(sel+1, len(opts)-1)
			case "\r", "\n", " ":
				if opts[sel].ID == "" {
					fmt.Printf("\n\n"+a.t("picker.unlinking"), r.Name)
				} else {
					fmt.Printf("\n\n"+a.t("picker.linking"), r.Name, a.targetName(opts[sel]))
				}
				if err := a.retarget(r.ID, opts[sel]); err != nil {
					return fmt.Sprintf(a.t("popup.failed"), r.Name, err)
				}
				if opts[sel].ID != "" {
					if msg := a.whitelistScreen(opts[sel]); msg != a.t("whitelist.saved") {
						return msg // linked, but the whitelist was not saved: not mirrored yet
					}
				}
				if mirror {
					return a.popupToggle(r)
				}
				return ""
			}
		}
	}
}

// targetOptions is the channels the bot is in that no other pane is linked to.
func (a *app) targetOptions(id string) ([]target, error) {
	if err := a.requireMM(); err != nil {
		return nil, err
	}
	chans, err := a.channels()
	if err != nil {
		return nil, err
	}
	targets, err := a.readTargets()
	if err != nil {
		return nil, err
	}
	return freeChannels(chans, targets, id), nil
}

func stty(args ...string) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	cmd.Run()
}

// rawMode puts the terminal in raw-enough mode without a dependency and returns the undo.
func rawMode() func() {
	stty("-icanon", "-echo", "min", "1")
	return func() { stty("icanon", "echo") }
}

// waitQuit reads keys until q or Esc.
func waitQuit() {
	defer rawMode()()
	b := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil || n == 1 && (b[0] == 'q' || b[0] == 0x1b) {
			return
		}
	}
}
