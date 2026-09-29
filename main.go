// herdr-mm mirrors opted-in herdr agent panes into a Mattermost DM between a bot and one user.
//
//	herdr-mm start   startup hook: launch the daemon detached (a second daemon exits at once)
//	herdr-mm daemon  hold the Mattermost WebSocket and type thread replies into agents
//	herdr-mm toggle  pane action: start or stop mirroring $HERDR_PANE_ID
//	herdr-mm event   event hook: sync a mirrored pane's thread on status change, move or close
//	herdr-mm stop    action: stop the daemon and wait for it to exit
//	herdr-mm status  popup pane: show the daemon and the mirrored panes, then wait for q or Esc
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
			if err = a.status(os.Stdout); err == nil {
				waitQuit()
			}
		default:
			err = fmt.Errorf("unknown command")
		}
	}
	if err != nil {
		log.Fatalf("herdr-mm %s: %v", cmd, err)
	}
}

type app struct {
	mmURL, token, user                     string // from $HERDR_PLUGIN_CONFIG_DIR/.env
	envPath, stateDir, herdrBin, claudeDir string
	botID, userID, dmID                    string // filled by connect
	lastPost                               int64  // create_at of the last DM post the daemon handled
}

func load() (*app, error) {
	configDir, stateDir := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"), os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if configDir == "" || stateDir == "" {
		return nil, errors.New("HERDR_PLUGIN_CONFIG_DIR and HERDR_PLUGIN_STATE_DIR are not set; herdr-mm runs as a herdr plugin")
	}
	a := &app{envPath: filepath.Join(configDir, ".env"), stateDir: stateDir, herdrBin: os.Getenv("HERDR_BIN_PATH"), claudeDir: os.Getenv("CLAUDE_CONFIG_DIR")}
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
	a.mmURL, a.token, a.user = strings.TrimRight(env["MM_URL"], "/"), env["MM_BOT_TOKEN"], strings.TrimPrefix(env["MM_USER"], "@")
	return a, nil
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
		return fmt.Errorf("missing %s in %s", strings.Join(missing, ", "), a.envPath)
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
	if _, err := fmt.Fprint(lock, os.Getpid()); err != nil { // read by stop
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

// status prints whether the daemon runs and the mirrored panes.
func (a *app) status(w io.Writer) error {
	if pid := a.daemonPid(); pid != 0 {
		fmt.Fprintf(w, "Mattermost daemon: running (pid %d)\n\n", pid)
	} else {
		fmt.Fprint(w, "Mattermost daemon: not running\n\n")
	}
	err := a.withState(func(panes map[string]*pane) error {
		if len(panes) == 0 {
			fmt.Fprintln(w, "No panes are mirrored. Run the toggle action on an agent pane to mirror it.")
			return nil
		}
		ids := make([]string, 0, len(panes))
		for id := range panes {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "PANE\tSTATUS\tAGENT\tDIRECTORY")
		for _, id := range ids {
			p := panes[id]
			fmt.Fprintf(tw, "%s\t%s %s\t%s\t%s\n", id, emoji[p.Status], p.Status, p.Agent, p.Cwd)
		}
		return tw.Flush()
	})
	fmt.Fprint(w, "\nPress q or Esc to close.")
	return err
}

// waitQuit reads keys until q or Esc. stty puts the terminal in raw-enough mode without a dependency.
func waitQuit() {
	stty := func(args ...string) {
		cmd := exec.Command("stty", args...)
		cmd.Stdin = os.Stdin
		cmd.Run()
	}
	stty("-icanon", "-echo", "min", "1")
	defer stty("icanon", "echo")
	b := make([]byte, 16)
	for {
		n, err := os.Stdin.Read(b)
		if err != nil || bytes.ContainsAny(b[:n], "qQ\x1b") {
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
	LastReply  string `json:"last_reply,omitempty"`  // uuid of the transcript entry last posted
	LastDialog string `json:"last_dialog,omitempty"` // dialog last posted while blocked
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
	path := filepath.Join(a.stateDir, "panes.json")
	panes := map[string]*pane{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &panes); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	fnErr := fn(panes) // saved even on error: fn only records what already happened
	b, err := json.MarshalIndent(panes, "", "  ")
	if err == nil {
		err = os.WriteFile(path+".tmp", b, 0o600)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	return errors.Join(fnErr, err)
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
			delete(panes, id)
			p.Status = "off"
			return a.patchPost(p.RootID, rootMessage(id, p))
		}
		if err := a.connect(); err != nil {
			return err
		}
		info, err := a.agent(id)
		if err != nil {
			return err
		}
		p := &pane{ChannelID: a.dmID, Status: info.Status, Agent: info.Agent, Cwd: info.Cwd}
		_, p.LastReply, _ = lastReply(a.transcript(info)) // only turns after sharing are posted
		if p.RootID, err = a.createPost(a.dmID, "", rootMessage(id, p)); err != nil {
			return err
		}
		panes[id] = p
		return a.sync(id, p) // posts the dialog if the agent is already blocked
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
		p := panes[oldID]
		if p == nil {
			return nil // not mirrored: the hook fires for every pane
		}
		if err := a.requireMM(); err != nil {
			return err
		}
		switch name {
		case "pane_closed":
			delete(panes, id)
			p.Status = "closed"
			return a.patchPost(p.RootID, rootMessage(id, p))
		case "pane_moved":
			delete(panes, oldID)
			panes[id] = p
			return errors.Join(a.patchPost(p.RootID, rootMessage(id, p)), a.sync(id, p))
		}
		return a.sync(id, p)
	})
}

// sync brings a mirrored pane's thread up to date with the agent's live state. It asks herdr for the
// state instead of trusting the event, and dedupes what it posts, so hooks that run late or out of
// order neither lose nor repeat a reply.
func (a *app) sync(id string, p *pane) error {
	info, err := a.agent(id)
	if he := (*herdrError)(nil); errors.As(err, &he) && he.Code == "agent_not_found" {
		info, err = agentInfo{Status: "unknown"}, nil // the agent exited; the pane may get a new one
	}
	if err != nil {
		return err
	}
	prev := p.Status
	p.Status = info.Status
	if info.Agent != "" {
		p.Agent, p.Cwd = info.Agent, info.Cwd
	}
	var patchErr error
	if p.Status != prev {
		patchErr = a.patchPost(p.RootID, rootMessage(id, p))
	}
	return errors.Join(patchErr, a.postNews(id, p, info))
}

// postNews posts the agent's newest reply, or the dialog it is blocked on, into the pane's thread,
// once each.
func (a *app) postNews(id string, p *pane, info agentInfo) error {
	switch p.Status {
	case "idle", "done":
		p.LastDialog = ""
		// ponytail: trusts Claude to have written the final entry by the time herdr says idle;
		// wait for the transcript to settle if replies ever come out one turn behind.
		text, uuid, err := lastReply(a.transcript(info))
		if err != nil {
			return err
		}
		if uuid != "" && uuid != p.LastReply {
			if _, err := a.createPost(p.ChannelID, p.RootID, text); err != nil {
				return err
			}
			p.LastReply = uuid
		}
	case "blocked":
		screen, err := a.herdr("agent", "read", id, "--source", "detection")
		if err != nil {
			return err
		}
		if d := dialog(string(screen)); d != p.LastDialog {
			msg := fmt.Sprintf("@%s ✋ **%s** is waiting on a dialog. Answer it on the machine:\n```\n%s\n```", a.user, p.Agent, truncate(d, maxPost-500))
			if _, err := a.createPost(p.ChannelID, p.RootID, msg); err != nil {
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

func rootMessage(id string, p *pane) string {
	head := fmt.Sprintf("%s **%s** · %s · `%s` · pane `%s`", emoji[p.Status], p.Status, p.Agent, baseName(p.Cwd), id)
	switch p.Status {
	case "off":
		return head + "\n_Mirroring stopped._"
	case "closed":
		return head + "\n_Pane closed, mirroring stopped._"
	}
	return head + "\n_Reply in this thread to prompt the agent._"
}

func baseName(path string) string {
	if path == "" {
		return "?"
	}
	return filepath.Base(path)
}

const maxPost = 16383 // Mattermost's post length limit, in characters

func truncate(s string, max int) string {
	const note = "\n… (truncated)"
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

// lastReply returns the text of the newest main-thread assistant message in a Claude transcript and
// the uuid of its last entry. Claude writes one entry per content block, so text is gathered by
// message id.
func lastReply(path string) (text, uuid string, err error) {
	if path == "" {
		return "", "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	var msgID string
	var texts []string
	r := bufio.NewReader(f) // not a Scanner: tool results make lines longer than its buffer
	for {
		line, readErr := r.ReadBytes('\n')
		var e struct {
			Type        string
			IsSidechain bool
			UUID        string
			Message     struct {
				ID      string
				Content []struct{ Type, Text string }
			}
		}
		if bytes.Contains(line, []byte(`"assistant"`)) && json.Unmarshal(line, &e) == nil && e.Type == "assistant" && !e.IsSidechain {
			var t []string
			for _, c := range e.Message.Content {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					t = append(t, c.Text)
				}
			}
			if len(t) > 0 {
				if e.Message.ID != msgID {
					msgID, texts = e.Message.ID, nil
				}
				texts, uuid = append(texts, t...), e.UUID
			}
		}
		if readErr == io.EOF {
			return strings.Join(texts, "\n\n"), uuid, nil
		}
		if readErr != nil {
			return "", "", readErr
		}
	}
}
