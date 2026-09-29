// herdr-mm mirrors opted-in herdr agent panes into a Mattermost DM between a bot and one user, or a channel.
//
//	herdr-mm start   startup hook: launch the daemon detached (a second daemon exits at once)
//	herdr-mm daemon  hold the Mattermost WebSocket and type thread replies into agents
//	herdr-mm toggle  pane action: start or stop mirroring $HERDR_PANE_ID
//	herdr-mm event   event hook: sync a mirrored pane's thread on status change, move or close
//	herdr-mm stop    action: stop the daemon and wait for it to exit
//	herdr-mm status  popup pane: list agent panes, toggle the selected one, pick its DM or channel, edit the settings, close on q or Esc
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"unicode/utf8"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: herdr-mm start|daemon|toggle|event|stop|status")
		os.Exit(2)
	}
	cmd := os.Args[1]
	a, err := load()
	if err == nil {
		switch cmd {
		case "start":
			err = a.start()
		case "daemon":
			err = a.daemon()
		case "toggle":
			if err = a.toggle(os.Getenv("HERDR_PANE_ID")); err == nil {
				err = a.start() // replies need the daemon, and startup hooks only run when herdr starts
			}
		case "event":
			err = a.event(os.Getenv("HERDR_PLUGIN_EVENT"), []byte(os.Getenv("HERDR_PLUGIN_EVENT_JSON")))
		case "stop":
			err = a.stop()
		case "status":
			a.popup()
			return
		default:
			err = fmt.Errorf("unknown command")
		}
	}
	if cmd == "status" { // load failed; show why until closed
		texts := catalog[lang(os.Getenv("HERDR_PLUGIN_STATE_DIR"))]
		fmt.Printf(texts["popup.error"]+"\n\n%s", err, texts["popup.close"])
		waitQuit()
		return
	}
	if err != nil {
		log.Fatalf("herdr-mm %s: %v", cmd, err)
	}
}

type app struct {
	mmURL, token, user                                   string // from settings.json, then .env, in $HERDR_PLUGIN_CONFIG_DIR
	envPath, settingsPath, stateDir, herdrBin, claudeDir string
	botID, botName, userID, dmID                         string // filled by connect
	lastPost                                             int64  // create_at of the last post the daemon handled
	connected                                            bool   // the daemon's WebSocket has connected before
}

func load() (*app, error) {
	configDir, stateDir := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"), os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if configDir == "" || stateDir == "" {
		return nil, errors.New("HERDR_PLUGIN_CONFIG_DIR and HERDR_PLUGIN_STATE_DIR are not set; herdr-mm runs as a herdr plugin")
	}
	a := &app{envPath: filepath.Join(configDir, ".env"), settingsPath: filepath.Join(configDir, "settings.json"), stateDir: stateDir, herdrBin: os.Getenv("HERDR_BIN_PATH"), claudeDir: os.Getenv("CLAUDE_CONFIG_DIR")}
	if a.herdrBin == "" {
		a.herdrBin = "herdr"
	}
	if a.claudeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		a.claudeDir = filepath.Join(home, ".claude")
	}
	env, err := readEnv(a.envPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	s, err := readSettings(a.settingsPath)
	if err != nil {
		return nil, err
	}
	// Saved settings win; .env fills the fields not saved.
	a.mmURL = strings.TrimRight(cmp.Or(s.URL, env["MM_URL"]), "/")
	a.token = cmp.Or(s.Token, env["MM_BOT_TOKEN"])
	a.user = strings.TrimPrefix(cmp.Or(s.User, env["MM_USER"]), "@")
	return a, nil
}

// settings are the values entered in the popup's settings screen, saved in settings.json.
type settings struct {
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
	User  string `json:"user,omitempty"`
}

func readSettings(path string) (settings, error) {
	var s settings
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// writeFile replaces path by rename, so it is never read half written, with mode 0600.
func writeFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.WriteFile(path+".tmp", b, 0o600)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	return err
}

// editSettings asks for each setting on w and reads the answers from r, then saves them. An empty
// answer keeps the saved value, so a field left unsaved still comes from .env. hide turns echo off
// while the token is typed.
func (a *app) editSettings(r *bufio.Reader, w io.Writer, hide func(bool)) error {
	s, err := readSettings(a.settingsPath)
	if err != nil {
		return err
	}
	ask := func(prompt string) (string, error) {
		fmt.Fprint(w, prompt)
		line, err := r.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	fmt.Fprint(w, a.t("settings.title")+"\n\n")
	v, err := ask(fmt.Sprintf(a.t("settings.url"), a.mmURL))
	if err != nil {
		return err
	}
	if v != "" {
		s.URL = strings.TrimRight(v, "/")
	}
	set := a.t("settings.unset")
	if a.token != "" {
		set = a.t("settings.set")
	}
	hide(true)
	v, err = ask(fmt.Sprintf(a.t("settings.token"), set))
	hide(false)
	fmt.Fprintln(w)
	if err != nil {
		return err
	}
	if v != "" {
		s.Token = v
	}
	if v, err = ask(fmt.Sprintf(a.t("settings.user"), a.user)); err != nil {
		return err
	}
	if v != "" {
		s.User = strings.TrimPrefix(v, "@")
	}
	if err := writeFile(a.settingsPath, s); err != nil {
		return err
	}
	a.mmURL, a.token, a.user = strings.TrimRight(cmp.Or(s.URL, a.mmURL), "/"), cmp.Or(s.Token, a.token), cmp.Or(s.User, a.user)
	return nil
}

// readEnv parses KEY=VALUE lines, ignoring blanks, comments and an "export " prefix.
func readEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	env := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
		k, v, ok := strings.Cut(line, "=")
		if ok && !strings.HasPrefix(line, "#") {
			env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return env, err
}

func (a *app) requireMM() error {
	var missing []string
	for _, kv := range [][2]string{{"MM_URL", a.mmURL}, {"MM_BOT_TOKEN", a.token}, {"MM_USER", a.user}} {
		if kv[1] == "" {
			missing = append(missing, kv[0])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(a.t("err.missing"), strings.Join(missing, ", "), a.envPath)
	}
	return nil
}

// start launches the daemon in its own session so it outlives this hook.
func (a *app) start() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(a.stateDir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func (a *app) daemon() error {
	lock, err := os.OpenFile(filepath.Join(a.stateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // held for the life of the process
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return nil // another daemon is already running
	}
	if err := lock.Truncate(0); err != nil {
		return err
	}
	if _, err := fmt.Fprint(lock, os.Getpid()); err != nil { // read by stop and status
		return err
	}
	if err := a.requireMM(); err != nil {
		return err
	}
	if err := a.connectRetry(); err != nil {
		return err
	}
	b, _ := os.ReadFile(filepath.Join(a.stateDir, "last_post"))
	if n, err := strconv.ParseInt(string(b), 10, 64); err == nil {
		a.lastPost = n // otherwise it stays at the DM's newest post, set by connect
	}
	log.Printf("daemon started: obeying @%s in DM %s", a.user, a.dmID)
	return a.listen()
}

// stop ends the running daemon and waits for it to release its lock.
func (a *app) stop() error {
	lock, err := os.OpenFile(filepath.Join(a.stateDir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		fmt.Println("no daemon is running")
		return nil
	}
	var pid int
	if _, err := fmt.Fscan(lock, &pid); err != nil {
		return fmt.Errorf("daemon.lock: %w", err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	fmt.Println("daemon stopped")
	return nil
}

// daemonPid returns the pid of the running daemon, or 0. It does not take the lock, which would make
// a daemon starting at that moment think another one is running.
// ponytail: trusts the pid in daemon.lock; a reused pid reads as running until the next daemon start.
func (a *app) daemonPid() int {
	var pid int
	b, _ := os.ReadFile(filepath.Join(a.stateDir, "daemon.lock"))
	if _, err := fmt.Sscan(string(b), &pid); err != nil || pid <= 0 || syscall.Kill(pid, 0) != nil {
		return 0
	}
	return pid
}

// row is a pane herdr reports an agent in, listed in the status popup.
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

// rows lists the agent panes. It does not take state.lock, which a toggle or
// event hook holds across Mattermost calls.
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

// herdrPane, herdrTab and herdrWorkspace are the parts of herdr's pane, tab and workspace lists that name a pane.
type herdrPane struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string
}

type herdrTab struct {
	TabID string `json:"tab_id"`
	Label string
}

type herdrWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string
	TabCount    int `json:"tab_count"`
}

// labels names every agent pane herdr reports and every mirrored pane, numbered so a pane has the same
// label in the popup and in every post; a mirrored pane herdr no longer lists keeps its last label. It
// also returns herdr's agents and marks the panes to leave out of the popup.
func (a *app) labels(mirrored map[string]*pane) (map[string]string, map[string]bool, []listedAgent, error) {
	var r struct {
		Result struct {
			Agents     []listedAgent
			Panes      []herdrPane
			Tabs       []herdrTab
			Workspaces []herdrWorkspace
		}
	}
	for _, kind := range []string{"agent", "pane", "tab", "workspace"} {
		out, err := a.herdr(kind, "list")
		if err == nil {
			err = json.Unmarshal(out, &r)
		}
		if err != nil {
			return nil, nil, nil, err
		}
	}
	names, hidden := paneNames(r.Result.Panes, r.Result.Tabs, r.Result.Workspaces)
	set := map[string]string{}
	for _, ag := range r.Result.Agents {
		set[ag.PaneID] = names[ag.PaneID]
	}
	for id, p := range mirrored {
		set[id] = cmp.Or(names[id], p.Name)
	}
	return numbered(set), hidden, r.Result.Agents, nil
}

// label sets p.Name to the pane's label; a failed lookup keeps the last one.
func (a *app) label(id string, p *pane, mirrored map[string]*pane) {
	if labels, _, _, err := a.labels(mirrored); err == nil && labels[id] != "" {
		p.Name = labels[id]
	}
}

// paneNames names a pane by its own label when renamed, otherwise by its workspace's label, followed
// by the tab's label when the workspace has more than one tab. It marks hidden the panes of the
// workspaces firstmate opens for its workers, whose labels start with "└ ".
func paneNames(panes []herdrPane, tabs []herdrTab, workspaces []herdrWorkspace) (map[string]string, map[string]bool) {
	tabLabel := map[string]string{}
	for _, t := range tabs {
		tabLabel[t.TabID] = t.Label
	}
	ws := map[string]herdrWorkspace{}
	for _, w := range workspaces {
		ws[w.WorkspaceID] = w
	}
	names, hidden := map[string]string{}, map[string]bool{}
	for _, p := range panes {
		w := ws[p.WorkspaceID]
		hidden[p.PaneID] = strings.HasPrefix(w.Label, "└ ")
		switch {
		case p.Label != "":
			names[p.PaneID] = p.Label
		case w.TabCount > 1:
			names[p.PaneID] = w.Label + " / " + tabLabel[p.TabID]
		default:
			names[p.PaneID] = w.Label
		}
	}
	return names, hidden
}

// numbered maps pane ids to their names, with " #2", " #3" added to the second and later, in pane id
// order, of those that share a name.
func numbered(names map[string]string) map[string]string {
	seen, out := map[string]int{}, map[string]string{}
	for _, id := range slices.SortedFunc(maps.Keys(names), paneOrder) {
		n := names[id]
		if seen[n]++; n != "" && seen[n] > 1 {
			n += " #" + strconv.Itoa(seen[n])
		}
		out[id] = n
	}
	return out
}

// paneOrder orders pane ids like wV:p3 or w13:p2 by workspace, then pane. herdr counts each part up
// like a number with digits 1-9, then A-Z, then 0, so a shorter part comes first and 0 ranks after Z.
func paneOrder(x, y string) int {
	xw, xp, _ := strings.Cut(x, ":")
	yw, yp, _ := strings.Cut(y, ":")
	count := func(a, b string) int {
		a, b = strings.ReplaceAll(a, "0", "~"), strings.ReplaceAll(b, "0", "~")
		return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b))
	}
	return cmp.Or(count(xw, yw), count(xp, yp))
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

// status prints whether the daemon runs and the rows, with a cursor on rows[sel].
func (a *app) status(w io.Writer, rows []row, sel int) error {
	if pid := a.daemonPid(); pid != 0 {
		fmt.Fprintf(w, a.t("popup.running")+"\n\n", pid)
	} else {
		fmt.Fprint(w, a.t("popup.stopped")+"\n\n")
	}
	if len(rows) == 0 {
		fmt.Fprintln(w, a.t("popup.none"))
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, a.t("popup.header"))
	for i, r := range rows {
		cursor, on := " ", ""
		if i == sel {
			cursor = ">"
		}
		if r.Mirrored {
			on = a.t("popup.yes")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s %s\t%s\t%s\n", cursor, r.Name, r.Agent, emoji[r.Status], a.t(r.Status), on, a.targetName(r.Target))
	}
	return tw.Flush()
}

// targetName names a pane's target in the popup.
func (a *app) targetName(t target) string {
	if t.ID == "" {
		return a.t("target.dm")
	}
	return "~" + t.Name
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
// pane, t picks its target, s opens the settings, q or Esc closes.
func (a *app) popup() {
	defer rawMode()()
	sel, msg := 0, ""
	for {
		rows, err := a.rows()
		sel = max(min(sel, len(rows)-1), 0)
		fmt.Print("\x1b[H\x1b[2J")
		if err == nil {
			err = a.status(os.Stdout, rows, sel)
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
					msg = a.pickTarget(rows[sel])
				}
				break keys // the picker read the keys that followed
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

// pickTarget lists the DM and the bot's channels not linked to another pane, and links the pane to the
// one picked with Enter; q or Esc cancels. It returns the error to show, if any.
func (a *app) pickTarget(r row) string {
	fmt.Printf("\n\n"+a.t("picker.loading"), r.Name)
	opts, err := a.targetOptions(r.ID)
	if err != nil {
		return err.Error()
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
				fmt.Printf("\n\n"+a.t("picker.linking"), r.Name, a.targetName(opts[sel]))
				if err := a.retarget(r.ID, opts[sel]); err != nil {
					return fmt.Sprintf(a.t("popup.failed"), r.Name, err)
				}
				return ""
			}
		}
	}
}

// targetOptions is the DM followed by the channels the bot is in that no other pane is linked to.
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
	return append([]target{{}}, freeChannels(chans, targets, id)...), nil
}

// settingsScreen asks for the settings, saves them and restarts the daemon so it uses them.
func (a *app) settingsScreen() string {
	fmt.Print("\x1b[H\x1b[2J")
	stty("icanon", "echo")
	defer stty("-icanon", "-echo", "min", "1")
	err := a.editSettings(bufio.NewReader(os.Stdin), os.Stdout, func(hide bool) {
		if hide {
			stty("-echo")
		} else {
			stty("echo")
		}
	})
	if err == nil {
		err = a.stop() // the next daemon reads the settings afresh
	}
	if err == nil {
		err = a.start()
	}
	if err != nil {
		return err.Error()
	}
	return a.t("settings.saved")
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

// pluginOn reports whether the herdr server is running with this plugin enabled.
func (a *app) pluginOn() bool {
	var status struct{ Running bool }
	out, err := a.herdr("status", "server", "--json")
	if err != nil || json.Unmarshal(out, &status) != nil || !status.Running {
		return false
	}
	var list struct {
		Result struct{ Plugins []struct{ Enabled bool } }
	}
	out, err = a.herdr("plugin", "list", "--plugin", "herdr-mattermost", "--json")
	return err == nil && json.Unmarshal(out, &list) == nil && len(list.Result.Plugins) == 1 && list.Result.Plugins[0].Enabled
}

// pane is what is remembered about a mirrored pane, keyed by pane id in panes.json.
type pane struct {
	RootID     string `json:"root_id"`
	ChannelID  string `json:"channel_id"`
	Status     string `json:"status"`
	Agent      string `json:"agent"`
	Cwd        string `json:"cwd"`
	Name       string `json:"name,omitempty"`        // the pane's label, kept by sync for the stop notice
	LastReply  string `json:"last_reply,omitempty"`  // uuid of the transcript entry last handled, posted or not
	LastDialog string `json:"last_dialog,omitempty"` // dialog last posted while blocked
	Prompted   recent `json:"prompted,omitempty"`    // thread replies last typed into the agent
	Channel    string `json:"channel,omitempty"`     // name of the channel the thread is in, "" for the DM
	ReplyRoot  string `json:"reply_root,omitempty"`  // in a channel, the thread of the last question typed in
}

// recent is the last few thread replies typed into a pane, oldest first. panes.json written before
// it was a list holds a single string.
type recent []string

func (r *recent) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*r = recent{s}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(r))
}

// withState runs fn on the mirrored panes under an exclusive file lock, then saves them. The lock
// serializes the event hooks herdr runs concurrently, the toggle action and the daemon.
func (a *app) withState(fn func(map[string]*pane) error) error {
	lock, err := os.OpenFile(filepath.Join(a.stateDir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	panes, err := a.readPanes()
	if err != nil {
		return err
	}
	fnErr := fn(panes) // saved even on error: fn only records what already happened
	return errors.Join(fnErr, writeFile(filepath.Join(a.stateDir, "panes.json"), panes))
}

// target is a channel a pane is linked to, by id, with its name for the popup. A pane with none is
// linked to the DM.
type target struct{ ID, Name string }

// readTargets loads targets.json, which maps pane ids to their channel. A channel holds one pane:
// when several are linked to it, the first in pane order keeps it and the others fall back to the DM.
// Writes go through writeTargets under state.lock.
func (a *app) readTargets() (map[string]target, error) {
	path := filepath.Join(a.stateDir, "targets.json")
	all := map[string]target{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &all); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	out, taken := map[string]target{}, map[string]bool{}
	for _, id := range slices.SortedFunc(maps.Keys(all), paneOrder) {
		if t := all[id]; t.ID != "" && !taken[t.ID] {
			out[id], taken[t.ID] = t, true
		}
	}
	return out, nil
}

func (a *app) writeTargets(t map[string]target) error {
	return writeFile(filepath.Join(a.stateDir, "targets.json"), t)
}

// freeChannels leaves out of chans the channels linked to a pane other than id.
func freeChannels(chans []target, targets map[string]target, id string) []target {
	return slices.DeleteFunc(slices.Clone(chans), func(c target) bool {
		for other, t := range targets {
			if other != id && t.ID == c.ID {
				return true
			}
		}
		return false
	})
}

// retarget links a pane to a channel, or to the DM for a target with no id. A mirrored pane's thread
// is stopped where it was and a new one started in the new place.
func (a *app) retarget(id string, to target) error {
	var changed, mirrored bool
	err := a.withState(func(panes map[string]*pane) error {
		targets, err := a.readTargets()
		if err != nil {
			return err
		}
		if to.ID != "" && len(freeChannels([]target{to}, targets, id)) == 0 {
			return fmt.Errorf(a.t("target.taken"), to.Name)
		}
		changed, mirrored = targets[id].ID != to.ID, panes[id] != nil
		if to.ID == "" {
			delete(targets, id)
		} else {
			targets[id] = to
		}
		return a.writeTargets(targets)
	})
	if err != nil || !changed || !mirrored {
		return err
	}
	if err := a.toggle(id); err != nil {
		return err
	}
	return a.toggle(id)
}

// readPanes loads panes.json. withState replaces it by rename, so it is never read half written.
func (a *app) readPanes() (map[string]*pane, error) {
	path := filepath.Join(a.stateDir, "panes.json")
	panes := map[string]*pane{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &panes); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return panes, nil
}

func (a *app) toggle(id string) error {
	if id == "" {
		return errors.New("HERDR_PANE_ID is not set; run the action on a pane")
	}
	if err := a.requireMM(); err != nil {
		return err
	}
	return a.withState(func(panes map[string]*pane) error {
		if p := panes[id]; p != nil {
			a.label(id, p, panes)
			delete(panes, id)
			p.Status = "off"
			return errors.Join(a.patchPost(p.RootID, a.rootMessage(p)), a.postStopped(p))
		}
		if err := a.connect(); err != nil {
			return err
		}
		info, err := a.agent(id)
		if err != nil {
			return err
		}
		targets, err := a.readTargets()
		if err != nil {
			return err
		}
		t := targets[id]
		p := &pane{ChannelID: cmp.Or(t.ID, a.dmID), Channel: t.Name, Status: info.Status, Agent: info.Agent, Cwd: info.Cwd}
		a.label(id, p, panes)
		_, p.LastReply, _, _, _ = lastReply(a.transcript(info)) // only turns after sharing are posted
		if p.RootID, err = a.createPost(p.ChannelID, "", a.rootMessage(p)); err != nil {
			return err
		}
		panes[id] = p
		return a.sync(id, panes)
	})
}

func (a *app) event(name string, raw []byte) error {
	var ev struct {
		Event string
		Data  struct {
			PaneID         string `json:"pane_id"`
			PreviousPaneID string `json:"previous_pane_id"` // pane.moved
			Pane           struct {
				PaneID string `json:"pane_id"`
			}
		}
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("HERDR_PLUGIN_EVENT_JSON: %w", err)
	}
	if name == "" {
		name = ev.Event
	}
	name = strings.ReplaceAll(name, ".", "_")
	id, oldID := ev.Data.PaneID, ev.Data.PaneID
	if name == "pane_moved" { // a move to another workspace gives the pane a new id
		id, oldID = ev.Data.Pane.PaneID, ev.Data.PreviousPaneID
	}
	return a.withState(func(panes map[string]*pane) error {
		if targets, err := a.readTargets(); err == nil && targets[oldID].ID != "" && (name == "pane_moved" || name == "pane_closed") {
			t := targets[oldID]
			delete(targets, oldID)
			if name == "pane_moved" {
				targets[id] = t
			}
			if err := a.writeTargets(targets); err != nil {
				return err
			}
		}
		p := panes[oldID]
		if p == nil {
			return nil // not mirrored: the hook fires for every pane
		}
		if err := a.requireMM(); err != nil {
			return err
		}
		switch name {
		case "pane_closed":
			a.label(id, p, panes)
			delete(panes, id)
			p.Status = "closed"
			return errors.Join(a.patchPost(p.RootID, a.rootMessage(p)), a.postStopped(p))
		case "pane_moved":
			delete(panes, oldID)
			panes[id] = p
		}
		return a.sync(id, panes)
	})
}

// sync brings a mirrored pane's thread up to date with the agent's live state. It asks herdr for the
// state instead of trusting the event, and dedupes what it posts, so hooks that run late or out of
// order neither lose nor repeat a reply.
func (a *app) sync(id string, panes map[string]*pane) error {
	p := panes[id]
	info, err := a.agent(id)
	if he := (*herdrError)(nil); errors.As(err, &he) && he.Code == "agent_not_found" {
		info, err = agentInfo{Status: "unknown"}, nil // the agent exited; the pane may get a new one
	}
	if err != nil {
		return err
	}
	prev, prevName := p.Status, p.Name
	p.Status = info.Status
	if info.Agent != "" {
		p.Agent, p.Cwd = info.Agent, info.Cwd
	}
	a.label(id, p, panes)
	var patchErr error
	if p.Status != prev || p.Name != prevName {
		patchErr = a.patchPost(p.RootID, a.rootMessage(p))
	}
	return errors.Join(patchErr, a.postNews(id, p, info))
}

// postNews posts the replies of the turns since the last one handled, or the dialog it is blocked on, into the pane's thread,
// once each. A reply is posted only when its turn was prompted from the thread, and a dialog only when
// the turn in effect is: one of the turn's prompts in the transcript is a recent text the daemon typed.
// Turns typed in the terminal stay off Mattermost.
func (a *app) postNews(id string, p *pane, info agentInfo) error {
	switch p.Status {
	case "idle", "done":
		p.LastDialog = ""
		// ponytail: trusts Claude to have written the final entry by the time herdr says idle;
		// wait for the transcript to settle if replies ever come out one turn behind.
		rs, _, cleared, err := replies(a.transcript(info))
		if err != nil {
			return err
		}
		// Every turn since the last one handled is posted, so a thread reply sent while the agent was
		// still answering the previous one doesn't hide that answer. All turns are considered when none
		// was handled yet, as when a pane is shared before its first prompt or a new agent starts in it,
		// or when /clear started the transcript. Otherwise, when the last turn handled is not in it, as
		// after resuming another session, only the newest is considered so old history stays out.
		// ponytail: resuming a session /clear started, or one resumed before a new agent's first turn,
		// considers all its turns; record the session with LastReply if old thread-prompted answers ever
		// get reposted.
		if len(rs) == 0 {
			p.LastReply = ""
			return nil
		}
		next := rs
		if i := slices.IndexFunc(rs, func(r reply) bool { return slices.Contains(r.uuids, p.LastReply) }); i >= 0 {
			next = rs[i:] // the turn handled gained text since, as when it is auto-continued after a usage limit
			if rs[i].uuid == p.LastReply {
				next = rs[i+1:]
			}
		} else if p.LastReply != "" && !cleared {
			next = rs[len(rs)-1:]
		}
		for _, r := range next {
			// ponytail: Prompted is kept after posting so a turn a background task resumes, which inherits
			// the thread prompt, is posted too; a terminal prompt identical to a recent thread reply is
			// posted as well. Clear it per turn if that ever bites. A resumed turn inherits the latest typed
			// prompt, not the one that started the task, so a task started in the terminal that finishes
			// after a thread reply is posted by mistake, and one started from the thread that finishes after
			// a terminal prompt is kept off. Record which turn started each background task if that bites.
			if p.fromThread(r.prompts) {
				text := r.text
				if p.Channel != "" {
					text = "@" + a.user + " " + text // answers the asker, who can only be MM_USER
				}
				if _, err := a.createPost(p.ChannelID, cmp.Or(p.ReplyRoot, p.RootID), text); err != nil {
					return err
				}
			}
			p.LastReply = r.uuid
		}
	case "blocked":
		_, _, _, turn, err := lastReply(a.transcript(info))
		if err != nil || !p.fromThread(turn) {
			return err
		}
		screen, err := a.herdr("agent", "read", id, "--source", "detection")
		if err != nil {
			return err
		}
		if d := dialog(string(screen)); d != p.LastDialog {
			msg := fmt.Sprintf(a.t("dialog")+"\n```\n%s\n```", a.user, p.Agent, a.truncate(d, maxPost-500))
			if _, err := a.createPost(p.ChannelID, cmp.Or(p.ReplyRoot, p.RootID), msg); err != nil {
				return err
			}
			p.LastDialog = d
		}
	default:
		p.LastDialog = ""
	}
	return nil
}

var emoji = map[string]string{"idle": "🟢", "done": "✅", "working": "⏳", "blocked": "✋", "unknown": "❔", "off": "⚪", "closed": "⚫"}

func (a *app) rootMessage(p *pane) string {
	head := fmt.Sprintf("%s **%s** · %s%s · `%s`", emoji[p.Status], a.t(p.Status), bold(p.Name), p.Agent, baseName(p.Cwd))
	switch p.Status {
	case "off":
		return head + "\n" + a.t("root.off")
	case "closed":
		return head + "\n" + a.t("root.closed")
	}
	if p.Channel != "" {
		return head + "\n" + a.t("root.channel")
	}
	return head + "\n" + a.t("root.reply")
}

// postStopped posts the top-level notice that mirroring of a pane was switched off or its pane closed;
// the root post is only edited, which notifies nobody.
func (a *app) postStopped(p *pane) error {
	what := a.t("notice.off")
	if p.Status == "closed" {
		what = a.t("notice.closed")
	}
	msg := fmt.Sprintf("%s %s %s%s · `%s` · [thread](%s/_redirect/pl/%s)",
		emoji[p.Status], what, bold(p.Name), p.Agent, baseName(p.Cwd), a.mmURL, p.RootID)
	_, err := a.createPost(p.ChannelID, "", msg)
	return err
}

// bold formats a pane name to lead a " · " list, or returns "" for a pane with no name yet.
func bold(name string) string {
	if name == "" {
		return ""
	}
	return "**" + name + "** · "
}

func baseName(path string) string {
	if path == "" {
		return "?"
	}
	return filepath.Base(path)
}

const maxPost = 16383 // Mattermost's post length limit, in characters

func (a *app) truncate(s string, max int) string {
	note := a.t("truncated")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-utf8.RuneCountInString(note)]) + note
}

// dialog cuts a Claude screen snapshot down to the box under its last horizontal rule, where
// approval and question dialogs render.
func dialog(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, " \n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		rest := strings.Join(lines[i+1:], "\n")
		if t != "" && strings.Trim(t, "─") == "" && strings.TrimSpace(rest) != "" {
			return strings.Trim(rest, "\n")
		}
	}
	return strings.Join(lines, "\n")
}

type herdrError struct{ Code, Message string }

func (e *herdrError) Error() string { return e.Code + ": " + e.Message }

// herdr runs the herdr CLI; a JSON error on stderr comes back as *herdrError.
func (a *app) herdr(args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(a.herdrBin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var e struct{ Error herdrError }
		if json.Unmarshal(stderr.Bytes(), &e) == nil && e.Error.Code != "" {
			return out, &e.Error
		}
		return out, fmt.Errorf("herdr %s: %v: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type agentInfo struct {
	Status  string `json:"agent_status"`
	Agent   string
	Cwd     string
	Session *struct{ Kind, Value string } `json:"agent_session"`
}

func (a *app) agent(id string) (agentInfo, error) {
	var r struct{ Result struct{ Agent agentInfo } }
	out, err := a.herdr("agent", "get", id)
	if err == nil {
		err = json.Unmarshal(out, &r)
	}
	return r.Result.Agent, err
}

// transcript finds the agent's Claude session log, or "" when there is none.
func (a *app) transcript(info agentInfo) string {
	s := info.Session
	if info.Agent != "claude" || s == nil || s.Value == "" {
		return ""
	}
	if s.Kind == "path" {
		return s.Value
	}
	// Session ids are unique, and the pane's cwd need not be the directory Claude started in.
	if m, _ := filepath.Glob(filepath.Join(a.claudeDir, "projects", "*", s.Value+".jsonl")); len(m) > 0 {
		return m[0]
	}
	return ""
}

// fromThread reports whether one of a turn's prompts is a recent thread reply typed into the pane.
func (p *pane) fromThread(turn []string) bool {
	return slices.ContainsFunc(turn, func(s string) bool {
		return strings.TrimSpace(s) != "" && slices.ContainsFunc(p.Prompted, func(q string) bool { return sameText(s, q) })
	})
}

// sameText compares prompts ignoring whitespace differences typing may introduce.
func sameText(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// pasteMarker matches the tags Claude wraps pasted text in when it records a prompt.
var pasteMarker = regexp.MustCompile(`</?pasted_content id="[^"]*">`)

// block is a content block of a transcript message.
type block struct{ Type, Text string }

// tag returns the text inside the first <name>...</name> in s.
func tag(s, name string) string {
	_, v, _ := strings.Cut(s, "<"+name+">")
	v, _, _ = strings.Cut(v, "</"+name+">")
	return v
}

// lastReply returns the text of the newest main-thread assistant message in a Claude transcript, the
// uuid of its last entry, the prompts typed in its turn and those of the turn in effect at the end of
// the transcript.
func lastReply(path string) (text, uuid string, prompts, turn []string, err error) {
	rs, turn, _, err := replies(path)
	if len(rs) == 0 {
		return "", "", nil, turn, err
	}
	r := rs[len(rs)-1]
	return r.text, r.uuid, r.prompts, turn, err
}

// reply is the newest assistant text message of a turn, with the uuid of its last entry, those of every
// text entry in the turn and the prompts typed in it.
type reply struct {
	text, uuid     string
	uuids, prompts []string
}

// replies returns the reply of every main-thread turn in a Claude transcript, oldest first, the prompts
// of the turn in effect at its end and whether its first typed prompt is /clear, which starts a new
// transcript. A turn starts at a typed prompt or a task notification, which
// keeps the prompts of the turn it resumes. Claude writes one entry per content block, so text is gathered
// by message id.
func replies(path string) (rs []reply, turn []string, cleared bool, err error) {
	if path == "" {
		return nil, nil, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, err
	}
	defer f.Close()
	var msgID, uuid string
	var texts, uuids, prompts []string
	endTurn := func() {
		if uuid != "" {
			rs = append(rs, reply{strings.Join(texts, "\n\n"), uuid, uuids, prompts})
		}
		msgID, uuid, texts, uuids, prompts = "", "", nil, nil, nil
	}
	r := bufio.NewReader(f) // not a Scanner: tool results make lines longer than its buffer
	for {
		line, readErr := r.ReadBytes('\n')
		var e struct {
			Type             string
			IsSidechain      bool
			IsMeta           bool
			IsCompactSummary bool
			Origin           struct{ Kind string }
			UUID             string
			Attachment       struct{ CommandMode, Prompt string }
			Message          struct {
				ID      string
				Content json.RawMessage
			}
		}
		// A typed prompt starts a turn as a user entry whose content is a string or blocks without a
		// tool_result (compaction summaries and task notifications are not typed); a prompt typed while
		// the agent works is queued into the running turn as an attachment.
		if bytes.Contains(line, []byte(`"user"`)) && json.Unmarshal(line, &e) == nil && e.Type == "user" && !e.IsSidechain && !e.IsMeta && e.Origin.Kind == "task-notification" {
			endTurn()
		}
		if bytes.Contains(line, []byte(`"user"`)) && json.Unmarshal(line, &e) == nil && e.Type == "user" && !e.IsSidechain && !e.IsMeta && !e.IsCompactSummary && e.Origin.Kind != "task-notification" {
			var s string
			var blocks []block
			if json.Unmarshal(e.Message.Content, &s) == nil {
				if name := tag(s, "command-name"); strings.HasPrefix(s, "<command-") && name != "" {
					s = name + " " + tag(s, "command-args") // a slash command is recorded as tags
				}
				cleared = cleared || turn == nil && strings.TrimSpace(s) == "/clear"
				endTurn()
				turn = []string{pasteMarker.ReplaceAllString(s, "")}
			} else if json.Unmarshal(e.Message.Content, &blocks) == nil && !slices.ContainsFunc(blocks, func(b block) bool { return b.Type == "tool_result" }) {
				var t []string // a prompt with a pasted image is recorded as blocks
				for _, b := range blocks {
					if b.Type == "text" {
						t = append(t, b.Text)
					}
				}
				endTurn()
				turn = []string{strings.Join(t, "\n")}
			}
		}
		if bytes.Contains(line, []byte(`"queued_command"`)) && json.Unmarshal(line, &e) == nil && e.Attachment.CommandMode == "prompt" && !e.IsSidechain {
			turn = append(turn, e.Attachment.Prompt)
		}
		if bytes.Contains(line, []byte(`"assistant"`)) && json.Unmarshal(line, &e) == nil && e.Type == "assistant" && !e.IsSidechain {
			var content []block
			json.Unmarshal(e.Message.Content, &content)
			var t []string
			for _, c := range content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					t = append(t, c.Text)
				}
			}
			if len(t) > 0 {
				if e.Message.ID != msgID {
					msgID, texts = e.Message.ID, nil
				}
				texts, uuid, uuids, prompts = append(texts, t...), e.UUID, append(uuids, e.UUID), turn
			}
		}
		if readErr == io.EOF {
			endTurn()
			return rs, turn, cleared, nil
		}
		if readErr != nil {
			return nil, nil, false, readErr
		}
	}
}
