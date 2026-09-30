// herdr-mm mirrors opted-in herdr agent panes into Mattermost channels, one channel per pane, obeying the users whitelisted in each channel.
//
//	herdr-mm start   startup hook: launch the daemon detached (a second daemon exits at once)
//	herdr-mm daemon  hold the Mattermost WebSocket and type @mentions of the bot into agents
//	herdr-mm toggle  pane action: start or stop mirroring $HERDR_PANE_ID
//	herdr-mm event   event hook: sync a mirrored pane's thread on status change, move or close
//	herdr-mm stop    action: stop the daemon and wait for it to exit
//	herdr-mm status  popup pane: list agent panes, toggle the selected one, pick its channel, edit the settings, close on q or Esc
package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	mmURL, token                                         string // from settings.json, then .env, in $HERDR_PLUGIN_CONFIG_DIR
	envPath, settingsPath, stateDir, herdrBin, claudeDir string
	botID, botName                                       string    // filled by connect
	lastPost                                             int64     // create_at of the last post the daemon handled
	connected                                            bool      // the daemon's WebSocket has connected before
	downAt                                               time.Time // when the WebSocket last dropped
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
	return a, nil
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
		a.lastPost = n // otherwise catch-up starts at each pane's root post
	}
	log.Print("daemon started")
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
