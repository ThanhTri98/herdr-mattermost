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
