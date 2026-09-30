package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOnePanePerChannel(t *testing.T) {
	e := newTestEnv(t)
	os.WriteFile(filepath.Join(e.a.stateDir, "targets.json"), []byte(`{"w2:p1":{"ID":"ch1","Name":"Dev"},"w1:p1":{"ID":"ch1","Name":"Dev"},"w1:p2":{"ID":"ch2","Name":"Ops"}}`), 0o600)
	targets, err := e.a.readTargets()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := targets["w2:p1"]; ok || targets["w1:p1"].ID != "ch1" || targets["w1:p2"].ID != "ch2" {
		t.Fatalf("the second pane on a channel must lose it: %+v", targets)
	}
	opts, err := e.a.targetOptions("w1:p2")
	if err != nil {
		t.Fatal(err)
	}
	if want := []target{{"ch2", "Ops", true}}; !slices.Equal(opts, want) {
		t.Fatalf("picker for w1:p2 = %+v, want %+v (no DMs, no Town Square or Off-Topic, not a channel another pane holds)", opts, want)
	}
	if got := e.a.targetName(opts[0]) + "|" + e.a.targetName(target{ID: "ch1", Name: "Dev"}) + "|" + e.a.targetName(target{}); got != "🔒 Ops|# Dev|Unlink the channel (stops mirroring)" {
		t.Fatalf("labels = %q", got)
	}
	if err := e.a.retarget("w3:p1", target{ID: "ch1", Name: "Dev"}); err == nil || !strings.Contains(err.Error(), "channel Dev is linked to another pane") {
		t.Fatalf("retarget to a taken channel = %v", err)
	}
	if err := e.a.retarget("w1:p1", target{}); err != nil { // unlinking frees the channel
		t.Fatal(err)
	}
	if err := e.a.retarget("w3:p1", target{ID: "ch1", Name: "Dev"}); err != nil {
		t.Fatal(err)
	}
	e.event(t, "pane.closed", "w1:p2")
	raw := `{"event":"pane_moved","data":{"pane":{"pane_id":"w4:p1"},"previous_pane_id":"w3:p1"}}`
	if err := e.a.event("pane.moved", []byte(raw)); err != nil {
		t.Fatal(err)
	}
	targets, _ = e.a.readTargets()
	if len(targets) != 1 || targets["w4:p1"].ID != "ch1" {
		t.Fatalf("targets after close and move = %+v", targets)
	}
}

func TestRetargetStartsNewThreadWhenStopFails(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	e.mm.mu.Lock()
	e.mm.failPatch = true
	e.mm.mu.Unlock()
	if err := e.a.retarget("w1:p1", target{"ch2", "Ops", true}); err == nil || !strings.Contains(err.Error(), "edit time limit") {
		t.Fatalf("retarget = %v, want the edit error", err)
	}
	panes, err := e.a.readPanes()
	if err != nil {
		t.Fatal(err)
	}
	if p := panes["w1:p1"]; p == nil || p.ChannelID != "ch2" {
		t.Fatalf("the pane must be mirrored in the new channel even when stopping the old thread fails: %+v", panes)
	}
}

func TestChannelControl(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	posts := e.mm.snapshot()
	if len(posts) != 1 || posts[0].ChannelID != "ch1" || posts[0].RootID != "" || !strings.Contains(posts[0].Message, "@mention the bot in this channel") {
		t.Fatalf("the pane's thread starts in its channel: %+v", posts)
	}
	root := posts[0].ID

	for _, p := range []post{
		{ID: "c1", ChannelID: "ch1", UserID: "alice-id", Message: "no mention, ignored", CreateAt: 101},
		{ID: "c2", ChannelID: "ch2", UserID: "alice-id", Message: "@herdr not a linked channel", CreateAt: 102},
		{ID: "c3", ChannelID: "ch1", UserID: "alice-id", Message: "@herdrx not the bot", CreateAt: 103},
		{ID: "c4", ChannelID: "ch1", UserID: "alice-id", Message: "@HERDR: fix the bug", CreateAt: 104},
		{ID: "c5", ChannelID: "ch1", RootID: "c4", UserID: "alice-id", Message: "and the tests @herdr.", CreateAt: 105},
		{ID: "c6", ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr in the pane's thread", CreateAt: 106},
		{ID: "c7", ChannelID: "ch1", RootID: "c4", UserID: "bob-id", Message: "@herdr rm -rf /", CreateAt: 107},
		{ID: "c8", ChannelID: "ch1", UserID: "alice-id", Message: "@herdr hook", Props: map[string]any{"from_webhook": "true"}, CreateAt: 108},
		{ID: "c9", ChannelID: "ch1", UserID: "alice-id", Message: "ping @herdr.vo about it", CreateAt: 109},
		{ID: "c10", ChannelID: "ch1", UserID: "alice-id", Type: "system_header_change", Message: "alice updated the channel header to: Ping @herdr to run my agent", CreateAt: 110},
	} {
		if err := e.a.handleEvent(posted(p)); err != nil {
			t.Fatal(err)
		}
	}
	if got := e.prompts(); got != "w1:p1|fix the bug\nw1:p1|and the tests\nw1:p1|in the pane's thread\n" {
		t.Fatalf("prompts = %q", got)
	}
	var got []string
	for _, p := range e.mm.snapshot()[1:] {
		got = append(got, p.ChannelID+"|"+p.RootID+"|"+p.Message)
	}
	ack := "📥 Received - the agent is working on it."
	if want := []string{"ch1|c4|" + ack, "ch1|c4|" + ack, "ch1|" + root + "|" + ack, "ch1|c4|" + "@bob You are not allowed to call me"}; !slices.Equal(got, want) {
		t.Fatalf("answers = %q, want %q", got, want)
	}

	// The refused post comes back on catch-up; it is answered once.
	e.mm.add(post{ChannelID: "ch1", RootID: "c4", UserID: "bob-id", Message: "@herdr again"})
	n := len(e.mm.snapshot())
	e.a.lastPost = e.mm.snapshot()[n-1].CreateAt - 1 // as when it was sent while the WebSocket was down
	if err := e.a.catchUp(); err != nil {
		t.Fatal(err)
	}
	posts = e.mm.snapshot()
	if len(posts) != n+1 || posts[n].Message != "@bob You are not allowed to call me" {
		t.Fatalf("catch-up answers channel posts once: %+v", posts[n-1:])
	}
	e.a.handleEvent(posted(posts[n-1])) // the WebSocket delivering it too
	if err := e.a.catchUp(); err != nil || len(e.mm.snapshot()) != n+1 {
		t.Fatalf("second catch-up = %v, %d posts", err, len(e.mm.snapshot()))
	}

	e.appendTranscript(t, typed("t1", "in the pane's thread"), assistant("t2", "m1", "text", "Done.", false))
	e.setAgent(t, "working")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	e.setAgent(t, "idle")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	last := e.mm.snapshot()[len(e.mm.snapshot())-1]
	if last.ChannelID != "ch1" || last.RootID != root || last.Message != "@alice Done." {
		t.Fatalf("reply = %+v, want it in the question's thread tagging the asker", last)
	}
}

func TestChannelCatchUp(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	e.mm.add(post{ChannelID: "ch1", UserID: "alice-id", Message: "@herdr run the migration"}) // the pane's thread is not in ch1 yet
	e.mm.add(post{ChannelID: "ch1", UserID: "bob-id", Message: "@herdr hi"})
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	e.mm.add(post{ChannelID: "ch1", UserID: "alice-id", Message: "@herdr run the tests"})
	e.mm.add(post{ChannelID: "dm", UserID: "alice-id", Message: "hello"}) // DMs are not caught up
	e.a.withState(func(panes map[string]*pane) error {
		panes["w2:p1"] = &pane{ChannelID: "gone", Channel: "Gone", RootID: "deleted"} // the bot cannot read it any more
		return nil
	})
	n := len(e.mm.snapshot())
	e.a.lastPost = 0 // all of the above was sent while the WebSocket was down
	if err := e.a.catchUp(); err != nil {
		t.Fatal(err)
	}
	if got := e.prompts(); got != "w1:p1|run the tests\n" {
		t.Fatalf("prompts = %q, want only the mention sent once the channel was linked", got)
	}
	var got []string
	for _, p := range e.mm.snapshot()[n:] {
		got = append(got, p.ChannelID+"|"+p.Message)
	}
	if want := []string{"ch1|" + catalog["en"]["prompt.late"]}; !slices.Equal(got, want) {
		t.Fatalf("answers = %q, want %q", got, want)
	}
}

func TestSettingsPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("MM_URL=https://env.example\nMM_BOT_TOKEN=env-tok\nMM_USER=env-user\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"token":"saved-tok","user":"old-user"}`), 0o600) // user is no longer read
	a, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if a.mmURL != "https://env.example" || a.token != "saved-tok" {
		t.Fatalf("saved settings win, .env fills the rest: %q %q", a.mmURL, a.token)
	}

	var out strings.Builder
	in := bufio.NewReader(strings.NewReader("https://saved.example/\nnew-secret\n"))
	if err := a.editSettings(in, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "new-secret") || strings.Contains(out.String(), "saved-tok") {
		t.Fatalf("the token must never be printed: %q", out.String())
	}
	if strings.Contains(out.String(), "username") {
		t.Fatalf("the settings no longer ask for a username: %q", out.String())
	}
	if fi, err := os.Stat(a.settingsPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode = %v %v", fi, err)
	}
	if a, err = load(); err != nil || a.mmURL != "https://saved.example" || a.token != "new-secret" {
		t.Fatalf("after editing: %q %q %v", a.mmURL, a.token, err)
	}
	if b, _ := os.ReadFile(a.settingsPath); strings.Contains(string(b), "user") {
		t.Fatalf("the old username is dropped on save: %s", b)
	}
}

func TestExecAndHelp(t *testing.T) {
	e := mirroredEnv(t)
	e.setAgent(t, "idle")
	e.a.withState(func(panes map[string]*pane) error {
		panes["w1:p2"] = &pane{RootID: "root2", ChannelID: "ch2", Channel: "Ops", Status: "idle", Agent: "claude"}
		return nil
	})
	os.WriteFile(filepath.Join(e.a.stateDir, "whitelists.json"), []byte(`{"ch1":[{"id":"alice-id","username":"alice"}],"ch2":[{"id":"alice-id","username":"alice"}]}`), 0o600)
	help, usage, ack, list := catalog["en"]["help"], catalog["en"]["exec.usage"], catalog["en"]["prompt.received"], e.a.listPanes("ch1")
	list2 := e.a.listPanes("ch2")
	if !strings.Contains(list, "/_redirect/pl/root1") || strings.Contains(list, "/_redirect/pl/root2") ||
		!strings.Contains(list2, "/_redirect/pl/root2") || strings.Contains(list2, "/_redirect/pl/root1") {
		t.Fatalf("list = %q, %q", list, list2)
	}
	for _, c := range []struct {
		p    post
		want string // channel|root|answer
	}{
		{post{ID: "c1", ChannelID: "ch1", RootID: "root1", Message: "@herdr #exec /clear"}, "ch1|root1|" + ack},
		{post{ID: "c2", ChannelID: "ch1", RootID: "root1", Message: "@herdr #exec"}, "ch1|root1|" + usage},
		{post{ID: "c3", ChannelID: "ch1", Message: "@herdr HELP"}, "ch1|c3|" + help},
		{post{ID: "c4", ChannelID: "ch1", RootID: "root1", Message: "@herdr list"}, "ch1|root1|" + list},
		{post{ID: "c5", ChannelID: "ch2", Message: "@herdr #exec /compact"}, "ch2|c5|" + ack},
		{post{ID: "c6", ChannelID: "ch2", RootID: "c5", Message: "@herdr help"}, "ch2|c5|" + help},
		{post{ID: "c12", ChannelID: "ch2", Message: "@herdr list"}, "ch2|c12|" + list2},
		{post{ID: "c7", ChannelID: "ch1", Message: "help"}, ""},        // no mention in a channel: ignored
		{post{ID: "c8", ChannelID: "ch3", Message: "@herdr help"}, ""}, // no pane in ch3: help and list too are ignored
		{post{ID: "c9", ChannelID: "ch3", RootID: "x", Message: "@herdr List"}, ""},
		{post{ID: "c10", ChannelID: "ch3", Message: "@herdr fix it"}, ""},
		{post{ID: "c11", ChannelID: "ch3", UserID: "bob-id", Message: "@herdr help"}, ""},
	} {
		if c.p.UserID == "" {
			c.p.UserID = "alice-id"
		}
		c.p.CreateAt = int64(len(e.mm.snapshot())+1) * 10
		n := len(e.mm.snapshot())
		if err := e.a.handleEvent(posted(c.p)); err != nil {
			t.Fatal(err)
		}
		var got string
		if posts := e.mm.snapshot()[n:]; len(posts) > 0 {
			got = posts[0].ChannelID + "|" + posts[0].RootID + "|" + posts[0].Message
		}
		if got != c.want {
			t.Errorf("%q: answer %q, want %q", c.p.Message, got, c.want)
		}
	}
	if got := e.prompts(); got != "w1:p1|/clear\nw1:p2|/compact\n" {
		t.Fatalf("prompts = %q", got)
	}
}

func TestChannelCapture(t *testing.T) {
	if _, err := loadFonts(); err != nil {
		t.Skip(err)
	}
	fastCapture(t)
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	os.WriteFile(filepath.Join(e.herdrDir, "screen.ansi"), []byte("Context 12%\r\n"), 0o644)
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	n := len(e.mm.snapshot())
	for _, p := range []post{
		{ID: "c1", ChannelID: "ch1", UserID: "alice-id", Message: "#capture /context", CreateAt: 101},           // no mention: ignored
		{ID: "c2", ChannelID: "ch1", RootID: "t1", UserID: "bob-id", Message: "@herdr #capture", CreateAt: 102}, // refused
		{ID: "c3", ChannelID: "ch1", UserID: "alice-id", Message: "@herdr #capture /context", CreateAt: 103},
	} {
		if err := e.a.handleEvent(posted(p)); err != nil {
			t.Fatal(err)
		}
	}
	shot := e.waitPost(t, n+3)
	if got := e.prompts(); got != "w1:p1|/context\n" {
		t.Fatalf("prompts = %q", got)
	}
	posts := e.mm.snapshot()
	if posts[n].RootID != "t1" || posts[n].Message != "@bob You are not allowed to call me" ||
		posts[n+1].ChannelID != "ch1" || posts[n+1].RootID != "c3" || !strings.HasPrefix(posts[n+1].Message, "📥") {
		t.Fatalf("answers = %+v", posts[n:])
	}
	if shot.ChannelID != "ch1" || shot.RootID != "c3" || len(shot.FileIDs) != 1 {
		t.Fatalf("screenshot post = %+v", shot)
	}
	e.mm.mu.Lock()
	files := e.mm.files
	e.mm.mu.Unlock()
	if len(files) != 1 || !strings.HasPrefix(files[0], "ch1|screen.png|") {
		t.Fatalf("uploads = %.40q", files)
	}

	e.a.handleEvent(posted(post{ID: "c4", ChannelID: "ch1", RootID: "c3", UserID: "alice-id", Message: "@herdr #capture", CreateAt: 104}))
	if shot := e.waitPost(t, n+4); shot.RootID != "c3" || len(shot.FileIDs) != 1 || e.prompts() != "w1:p1|/context\n" {
		t.Fatalf("bare #capture in a thread = %+v, prompts %q", shot, e.prompts())
	}
}

func TestPaneWithoutAgentKeepsItsChannelRow(t *testing.T) {
	e := newTestEnv(t)
	os.WriteFile(filepath.Join(e.herdrDir, "agents.json"), []byte(`{"result":{"agents":[{"pane_id":"w1:p1","agent":"claude","agent_status":"idle"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "panes.json"), []byte(`{"result":{"panes":[{"pane_id":"w1:p1","workspace_id":"w1"},{"pane_id":"w1:p2","workspace_id":"w1"},{"pane_id":"w2:p1","workspace_id":"w2"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "workspaces.json"), []byte(`{"result":{"workspaces":[{"workspace_id":"w1","label":"api","tab_count":1},{"workspace_id":"w2","label":"└ worker","tab_count":1}]}}`), 0o644)
	// w1:p2 is a shell that ran Claude, w2:p1 a hidden worker pane and w9:p1 a closed pane.
	os.WriteFile(filepath.Join(e.a.stateDir, "targets.json"), []byte(`{"w1:p2":{"ID":"ch1","Name":"Dev"},"w2:p1":{"ID":"ch2","Name":"Ops"},"w9:p1":{"ID":"ch3","Name":"Old"}}`), 0o600)
	rows, err := e.a.rows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].ID != "w1:p2" || rows[1].Name != "api #2" || rows[1].Status != "noagent" || rows[1].Target.ID != "ch1" {
		t.Fatalf("rows = %+v", rows)
	}
	var out strings.Builder
	e.a.status(&out, rows, 1, 200)
	if !strings.Contains(out.String(), "api #2") || !strings.Contains(out.String(), "no agent") || !regexp.MustCompile(`# Dev +alice\n`).MatchString(out.String()) {
		t.Fatalf("popup:\n%s", out.String())
	}
	os.WriteFile(filepath.Join(e.a.stateDir, "whitelists.json"), []byte("{"), 0o600)
	out.Reset()
	if err := e.a.status(&out, rows, 1, 200); err != nil || !strings.Contains(out.String(), "api #2") || !regexp.MustCompile(`# Dev +\?\n`).MatchString(out.String()) {
		t.Fatalf("popup with an unreadable whitelists.json: %v\n%s", err, out.String())
	}
	if err := e.a.retarget(rows[1].ID, target{}); err != nil {
		t.Fatal(err)
	}
	if opts, _ := e.a.targetOptions("w1:p1"); !slices.Contains(opts, target{ID: "ch1", Name: "Dev"}) {
		t.Fatalf("freed channel not offered: %+v", opts)
	}
	if rows, _ = e.a.rows(); len(rows) != 1 {
		t.Fatalf("pane with no agent and no channel still listed: %+v", rows)
	}
}

func TestFreeChannelOfMirroredPaneWithoutAgent(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(e.herdrDir, "agent.json")) // Claude exits, the pane stays open as a shell
	if err := e.a.retarget("w1:p1", target{}); err != nil {
		t.Fatalf("freeing the channel = %v", err)
	}
	panes, _ := e.a.readPanes()
	targets, _ := e.a.readTargets()
	if panes["w1:p1"] != nil || targets["w1:p1"].ID != "" {
		t.Fatalf("the pane must be off and unlinked: panes %+v, targets %+v", panes, targets)
	}
}

func TestUnlinkStopsMirroring(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	if err := e.a.retarget("w1:p1", target{}); err != nil {
		t.Fatal(err)
	}
	panes, _ := e.a.readPanes()
	targets, _ := e.a.readTargets()
	if len(panes) != 0 || len(targets) != 0 {
		t.Fatalf("unlinking must stop mirroring and forget the channel: panes %+v, targets %+v", panes, targets)
	}
	posts := e.mm.snapshot()
	if len(posts) != 2 || !strings.Contains(posts[0].Message, "Mirroring stopped") ||
		posts[1].ChannelID != "ch1" || posts[1].RootID != "" || !strings.HasPrefix(posts[1].Message, "⚪ Mirroring stopped for") {
		t.Fatalf("unlinking marks the root post and posts the stop notice in the old channel, and starts no thread: %+v", posts)
	}
}

func TestToggleNeedsChannel(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	os.Remove(filepath.Join(e.a.stateDir, "targets.json"))
	if err := e.a.toggle("w1:p1"); err == nil || err.Error() != catalog["en"]["toggle.nochannel"] {
		t.Fatalf("toggle without a channel = %v", err)
	}
	if posts := e.mm.snapshot(); len(posts) != 0 {
		t.Fatalf("posted without a channel: %+v", posts)
	}
}

// TestDMPaneSwitchedOff reads a pane mirrored to the DM, as saved before channels were the only place,
// as off, without posting anything.
func TestDMPaneSwitchedOff(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	os.WriteFile(filepath.Join(e.a.stateDir, "panes.json"), []byte(`{"w1:p1":{"root_id":"old","channel_id":"dm","status":"idle"},
		"w1:p2":{"root_id":"r2","channel_id":"ch2","channel":"Ops","status":"idle"}}`), 0o600)
	panes, err := e.a.readPanes()
	if err != nil || len(panes) != 1 || panes["w1:p2"] == nil {
		t.Fatalf("panes = %+v %v", panes, err)
	}
	if err := e.a.toggle("w1:p1"); err != nil { // switches it on, in its channel
		t.Fatal(err)
	}
	if posts := e.mm.snapshot(); len(posts) != 1 || posts[0].ChannelID != "ch1" || posts[0].RootID != "" {
		t.Fatalf("posts = %+v", posts)
	}
}

func TestDMRefused(t *testing.T) {
	e := mirroredEnv(t)
	refused := catalog["en"]["dm.refused"]
	for _, p := range []post{
		{ID: "d1", ChannelID: "dm", UserID: "alice-id", Message: "fix the bug", CreateAt: 1},
		{ID: "d2", ChannelID: "dm", RootID: "d1", UserID: "alice-id", Message: "@herdr list", CreateAt: 2},
		{ID: "d3", ChannelID: "dm2", UserID: "bob-id", Message: "help", CreateAt: 3},
	} {
		e.a.handleEvent(postedIn(p, "D"))
		e.a.handleEvent(postedIn(p, "D")) // delivered twice, answered once
	}
	var got []string
	for _, p := range e.mm.snapshot() {
		got = append(got, p.ChannelID+"|"+p.RootID+"|"+p.Message)
	}
	if want := []string{"dm||" + refused, "dm|d1|" + refused, "dm2||" + refused}; !slices.Equal(got, want) {
		t.Fatalf("answers = %q, want %q", got, want)
	}
	if got := e.prompts(); got != "" {
		t.Fatalf("a DM was typed into an agent: %q", got)
	}
}

// TestPopupPicksChannelBeforeMirroring drives the status popup: Enter on a pane with no channel opens the
// picker, which lists only the bot's channels, labelled, and mirrors the pane once one is picked; t then
// offers the unlink line, which stops mirroring.
func TestPopupPicksChannelBeforeMirroring(t *testing.T) {
	e := newTestEnv(t)
	t.Cleanup(func() { e.a.stop() }) // the toggle starts a daemon; stopped before the fake Mattermost closes
	e.setAgent(t, "idle")
	os.Remove(filepath.Join(e.a.stateDir, "targets.json"))
	os.WriteFile(filepath.Join(e.herdrDir, "agents.json"), []byte(`{"result":{"agents":[{"pane_id":"w1:p1","agent":"claude","agent_status":"idle"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "panes.json"), []byte(`{"result":{"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "workspaces.json"), []byte(`{"result":{"workspaces":[{"workspace_id":"w1","label":"api","tab_count":1}]}}`), 0o644)
	os.WriteFile(e.a.envPath, []byte("MM_URL="+e.a.mmURL+"\nMM_BOT_TOKEN=tok\nMM_USER=alice\n"), 0o600)
	home := t.TempDir()
	cmd := exec.Command(os.Args[0], "status")
	cmd.Env = append(os.Environ(), "HERDR_MM_MAIN=1", "HERDR_PLUGIN_CONFIG_DIR="+filepath.Dir(e.a.envPath), "HERDR_PLUGIN_STATE_DIR="+e.a.stateDir,
		"HERDR_BIN_PATH="+e.a.herdrBin, "CLAUDE_CONFIG_DIR="+e.a.claudeDir, "HOME="+home, "XDG_CONFIG_HOME="+home, "XDG_STATE_HOME="+home, "XDG_DATA_HOME="+home)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	var mu sync.Mutex
	var out strings.Builder
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := stdout.Read(b)
			mu.Lock()
			out.Write(b[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	from := 0
	// screen waits until the output since the last screen holds want, and returns it.
	screen := func(want string) string {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			s := out.String()[from:]
			mu.Unlock()
			if i := strings.Index(s, want); i >= 0 {
				from += i + len(want)
				return s[:i+len(want)]
			}
		}
		t.Fatalf("popup never showed %q: %q", want, out.String()[from:])
		return ""
	}
	wait := func(what string, ok func(map[string]*pane, map[string]target) bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			panes, _ := e.a.readPanes()
			targets, _ := e.a.readTargets()
			if ok(panes, targets) {
				return
			}
		}
		t.Fatalf("%s never happened", what)
	}

	screen("q or Esc: close")
	stdin.Write([]byte("\r"))
	picker := screen("q or Esc: cancel")
	if !strings.Contains(picker, "Pick the channel to mirror api") || !strings.Contains(picker, "> # Dev") || !strings.Contains(picker, "  🔒 Ops") ||
		strings.Contains(picker, "Town Square") || strings.Contains(picker, "Off-Topic") || strings.Contains(picker, "alice") || strings.Contains(picker, "Unlink") {
		t.Fatalf("picker = %q", picker)
	}
	stdin.Write([]byte("\r"))
	if s := screen("> alice\x1b7\x1b8"); !strings.Contains(s, "Linking api to # Dev...") || !strings.Contains(s, "Whitelist of # Dev") || !strings.Contains(s, "Esc: back.") {
		t.Fatalf("whitelist prompt after the pick, prefilled with the current list = %q", s)
	}
	stdin.Write([]byte(strings.Repeat("\x7f", 5) + "@Bob, carol bob\r"))
	wait("mirroring in Dev", func(panes map[string]*pane, targets map[string]target) bool {
		return panes["w1:p1"] != nil && panes["w1:p1"].ChannelID == "ch1" && targets["w1:p1"].ID == "ch1"
	})
	if s := screen("q or Esc: close"); !regexp.MustCompile(`# Dev +bob \+1\n`).MatchString(s) || !strings.Contains(s, "Whitelist: bob, carol\n") || !strings.Contains(s, "- w: edit its channel's whitelist") {
		t.Fatalf("popup after linking = %q", s)
	}
	if lists, _ := e.a.readWhitelists(); !slices.Equal(lists["ch1"], []member{{"bob-id", "bob"}, {"carol-id", "carol"}}) {
		t.Fatalf("saved whitelist = %+v", lists)
	}
	stdin.Write([]byte("w"))
	screen("Whitelist of # Dev, the Mattermost usernames the bot obeys there, comma or space separated.\nEnter: save (an empty line empties it), Esc: back.\n\n> bob, carol\x1b7\x1b8")
	stdin.Write([]byte("\x7f\x1b"))
	if s := screen("q or Esc: close"); strings.Contains(s, "Whitelist saved.") || !strings.Contains(s, "Whitelist: bob, carol\n") {
		t.Fatalf("popup after Esc on the whitelist = %q", s)
	}
	stdin.Write([]byte("w"))
	screen("> bob, carol\x1b7\x1b8")
	stdin.Write([]byte(strings.Repeat("\x7f", 10) + "\r"))
	if s := screen("q or Esc: close"); !strings.Contains(s, "Whitelist saved.") || !regexp.MustCompile(`# Dev +empty\n`).MatchString(s) {
		t.Fatalf("popup after emptying the whitelist = %q", s)
	}

	stdin.Write([]byte("t"))
	if picker = screen("q or Esc: cancel"); !strings.Contains(picker, "> # Dev") || !strings.Contains(picker, "  Unlink the channel (stops mirroring)") {
		t.Fatalf("picker of a linked pane = %q", picker)
	}
	stdin.Write([]byte("jj\r"))
	if s := screen("Unlinking the channel of api..."); strings.Contains(s, "Linking api to") {
		t.Fatalf("unlinking shows the linking line: %q", s)
	}
	wait("unlinking", func(panes map[string]*pane, targets map[string]target) bool {
		return len(panes) == 0 && len(targets) == 0
	})
	if !slices.ContainsFunc(e.mm.snapshot(), func(p post) bool {
		return p.ChannelID == "ch1" && p.RootID == "" && strings.HasPrefix(p.Message, "⚪ Mirroring stopped for")
	}) {
		t.Fatalf("no stop notice in the old channel: %+v", e.mm.snapshot())
	}
	stdin.Write([]byte("q"))
	if err := cmd.Wait(); err != nil {
		t.Fatalf("popup exit = %v", err)
	}
}
