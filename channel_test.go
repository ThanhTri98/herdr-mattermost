package main

import (
	"bufio"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestOnePanePerChannel(t *testing.T) {
	e := newTestEnv(t)
	os.WriteFile(filepath.Join(e.a.stateDir, "targets.json"), []byte(`{"w2:p1":{"ID":"ch1","Name":"Dev"},"w1:p1":{"ID":"ch1","Name":"Dev"},"w1:p2":{"ID":"ch2","Name":"Ops"}}`), 0o600)
	targets, err := e.a.readTargets()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := targets["w2:p1"]; ok || targets["w1:p1"].ID != "ch1" || targets["w1:p2"].ID != "ch2" {
		t.Fatalf("the second pane on a channel must fall back to the DM: %+v", targets)
	}
	opts, err := e.a.targetOptions("w1:p2")
	if err != nil {
		t.Fatal(err)
	}
	if want := []target{{}, {"ch2", "Ops"}}; !slices.Equal(opts, want) {
		t.Fatalf("picker for w1:p2 = %+v, want %+v (DM first, no DMs, not a channel another pane holds)", opts, want)
	}
	if err := e.a.retarget("w3:p1", target{"ch1", "Dev"}); err == nil || !strings.Contains(err.Error(), "channel Dev is linked to another pane") {
		t.Fatalf("retarget to a taken channel = %v", err)
	}
	if err := e.a.retarget("w1:p1", target{}); err != nil { // back to the DM frees the channel
		t.Fatal(err)
	}
	if err := e.a.retarget("w3:p1", target{"ch1", "Dev"}); err != nil {
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
	if err := e.a.retarget("w1:p1", target{"ch1", "Dev"}); err == nil || !strings.Contains(err.Error(), "edit time limit") {
		t.Fatalf("retarget = %v, want the edit error", err)
	}
	panes, err := e.a.readPanes()
	if err != nil {
		t.Fatal(err)
	}
	if p := panes["w1:p1"]; p == nil || p.ChannelID != "ch1" {
		t.Fatalf("the pane must be mirrored in the new channel even when stopping the old thread fails: %+v", panes)
	}
}

func TestChannelControl(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	if err := e.a.retarget("w1:p1", target{"ch1", "Dev"}); err != nil {
		t.Fatal(err)
	}
	posts := e.mm.snapshot()
	if len(posts) != 3 || posts[0].ChannelID != "dm" || !strings.Contains(posts[0].Message, "Mirroring stopped") ||
		posts[1].ChannelID != "dm" || !strings.HasPrefix(posts[1].Message, "⚪ Mirroring stopped for") ||
		posts[2].ChannelID != "ch1" || posts[2].RootID != "" || !strings.Contains(posts[2].Message, "@mention the bot in this channel") {
		t.Fatalf("a retarget stops the DM thread and starts one in the channel: %+v", posts)
	}
	root := posts[2].ID

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
	for _, p := range e.mm.snapshot()[3:] {
		got = append(got, p.ChannelID+"|"+p.RootID+"|"+p.Message)
	}
	ack := "📥 Received - the agent is working on it."
	if want := []string{"ch1|c4|" + ack, "ch1|c4|" + ack, "ch1|" + root + "|" + ack, "ch1|c4|Only @alice can control this agent."}; !slices.Equal(got, want) {
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
	if len(posts) != n+1 || posts[n].Message != "Only @alice can control this agent." {
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
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	handled := e.mm.snapshot()[0].CreateAt
	e.mm.add(post{ChannelID: "ch1", UserID: "alice-id", Message: "@herdr run the migration"}) // ch1 is not linked yet
	e.mm.add(post{ChannelID: "ch1", UserID: "bob-id", Message: "@herdr hi"})
	if err := e.a.retarget("w1:p1", target{"ch1", "Dev"}); err != nil {
		t.Fatal(err)
	}
	e.mm.add(post{ChannelID: "ch1", UserID: "alice-id", Message: "@herdr run the tests"})
	e.mm.add(post{ChannelID: "dm", UserID: "alice-id", Message: "hello"})
	e.a.withState(func(panes map[string]*pane) error {
		panes["w2:p1"] = &pane{ChannelID: "gone", RootID: "deleted"} // the bot cannot read it any more
		return nil
	})
	n := len(e.mm.snapshot())
	e.a.lastPost = handled // all of the above was sent while the WebSocket was down
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
	if want := []string{"ch1|" + catalog["en"]["prompt.late"], "dm|" + catalog["en"]["help"]}; !slices.Equal(got, want) {
		t.Fatalf("answers = %q, want %q", got, want)
	}
}

func TestSettingsPrecedence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", dir)
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("MM_URL=https://env.example\nMM_BOT_TOKEN=env-tok\nMM_USER=env-user\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"token":"saved-tok"}`), 0o600)
	a, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if a.mmURL != "https://env.example" || a.token != "saved-tok" || a.user != "env-user" {
		t.Fatalf("saved settings win, .env fills the rest: %q %q %q", a.mmURL, a.token, a.user)
	}

	var out strings.Builder
	var hidden []bool
	in := bufio.NewReader(strings.NewReader("https://saved.example/\nnew-secret\n\n"))
	if err := a.editSettings(in, &out, func(h bool) { hidden = append(hidden, h) }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "new-secret") || strings.Contains(out.String(), "saved-tok") || !slices.Equal(hidden, []bool{true, false}) {
		t.Fatalf("the token must be hidden and never printed: %q %v", out.String(), hidden)
	}
	if fi, err := os.Stat(a.settingsPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("settings.json mode = %v %v", fi, err)
	}
	if a, err = load(); err != nil || a.mmURL != "https://saved.example" || a.token != "new-secret" || a.user != "env-user" {
		t.Fatalf("after editing: %q %q %q %v", a.mmURL, a.token, a.user, err)
	}
	if s, _ := readSettings(a.settingsPath); s.User != "" {
		t.Fatalf("an empty answer must not save the .env value: %+v", s)
	}
}

func TestExecAndHelp(t *testing.T) {
	e := mirroredEnv(t)
	e.setAgent(t, "idle")
	e.a.withState(func(panes map[string]*pane) error {
		panes["w1:p2"] = &pane{RootID: "root2", ChannelID: "ch1", Status: "idle", Agent: "claude"}
		return nil
	})
	help, usage, ack := catalog["en"]["help"], catalog["en"]["exec.usage"], catalog["en"]["prompt.received"]
	for _, c := range []struct {
		p    post
		want string // channel|root|answer
	}{
		{post{ID: "d1", ChannelID: "dm", RootID: "root1", Message: "#exec /clear"}, "dm|root1|" + ack},
		{post{ID: "d2", ChannelID: "dm", RootID: "root1", Message: "#exec"}, "dm|root1|" + usage},
		{post{ID: "d3", ChannelID: "dm", Message: "HELP"}, "dm||" + help},
		{post{ID: "d4", ChannelID: "dm", Message: "@herdr help"}, "dm||" + help},
		{post{ID: "d5", ChannelID: "dm", RootID: "root1", Message: "help"}, "dm|root1|" + ack}, // a bare help in a thread is a prompt
		{post{ID: "d6", ChannelID: "dm", RootID: "root1", Message: "@herdr HELP"}, "dm|root1|" + help},
		{post{ID: "c1", ChannelID: "ch1", Message: "@herdr #exec /compact"}, "ch1|c1|" + ack},
		{post{ID: "c2", ChannelID: "ch1", RootID: "c1", Message: "@herdr help"}, "ch1|c1|" + help},
		{post{ID: "c3", ChannelID: "ch1", Message: "help"}, ""}, // no mention in a channel: ignored
	} {
		c.p.UserID, c.p.CreateAt = "alice-id", int64(len(e.mm.snapshot())+1)*10
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
	if got := e.prompts(); got != "w1:p1|/clear\nw1:p1|help\nw1:p2|/compact\n" {
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
	if err := e.a.retarget("w1:p1", target{"ch1", "Dev"}); err != nil {
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
	if posts[n].RootID != "t1" || posts[n].Message != "Only @alice can control this agent." ||
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
	e.a.status(&out, rows, 1)
	if !strings.Contains(out.String(), "api #2") || !strings.Contains(out.String(), "no agent") || !strings.Contains(out.String(), "~Dev") {
		t.Fatalf("popup:\n%s", out.String())
	}
	if err := e.a.retarget(rows[1].ID, target{}); err != nil {
		t.Fatal(err)
	}
	if opts, _ := e.a.targetOptions("w1:p1"); !slices.Contains(opts, target{"ch1", "Dev"}) {
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
	if err := e.a.retarget("w1:p1", target{"ch1", "Dev"}); err != nil {
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
