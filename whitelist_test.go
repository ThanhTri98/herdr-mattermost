package main

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
)

func TestWhitelistEdit(t *testing.T) {
	e := newTestEnv(t)
	dev, ops := target{ID: "ch1", Name: "Dev"}, target{ID: "ch2", Name: "Ops", Private: true}
	edit := func(ch target, keys string) (string, bool, error) {
		var out strings.Builder
		saved, err := e.a.editWhitelist(bufio.NewReader(strings.NewReader(keys)), &out, ch)
		return out.String(), saved, err
	}
	if out, saved, err := edit(ops, "@Bob, carol  bob\r"); err != nil || !saved || !strings.Contains(out, "Whitelist of 🔒 Ops") || !strings.Contains(out, "Esc: back.\n\n> \x1b7\x1b8") {
		t.Fatalf("edit = %q, %v, %v", out, saved, err)
	}
	if out, saved, err := edit(ops, "\x1b"); err != nil || saved || !strings.HasSuffix(out, "> bob, carol\x1b7\x1b8") {
		t.Fatalf("Esc = %q, %v, %v: the current list is prefilled and kept", out, saved, err)
	}
	if _, _, err := edit(ops, " dave, @eve\r"); err == nil || err.Error() != "unknown Mattermost user @dave, @eve, the whitelist was not saved" {
		t.Fatalf("unknown users = %v", err)
	}
	// "bob, carol": back 7 to after "bob", erase it, type alice, then to the end, clamped, and add bob.
	if _, _, err := edit(ops, strings.Repeat("\x1b[D", 7)+"\x7f\x7f\x7falice"+strings.Repeat("\x1b[C", 20)+" bob\r"); err != nil {
		t.Fatal(err)
	}
	lists, _ := e.a.readWhitelists()
	if want := []member{{"alice-id", "alice"}, {"bob-id", "bob"}, {"carol-id", "carol"}}; !slices.Equal(lists["ch2"], want) || !slices.Equal(lists["ch1"], []member{{"alice-id", "alice"}}) {
		t.Fatalf("whitelists = %+v", lists)
	}
	if _, _, err := edit(dev, strings.Repeat("\x7f", 5)+"\r"); err != nil {
		t.Fatal(err)
	}
	if lists, _ = e.a.readWhitelists(); len(lists["ch1"]) != 0 || len(lists["ch2"]) != 3 {
		t.Fatalf("an empty line empties only its channel: %+v", lists)
	}
	if got := usernames([]member{{"a", "a"}, {"b", "b"}, {"c", "c"}, {"d", "d"}, {"e", "e"}}, 3); got != "a, b, c +2" {
		t.Fatalf("usernames = %q", got)
	}
}

func TestReadLine(t *testing.T) {
	// Left, é typed, Delete ignored, Right, Backspace, x typed, then Enter.
	if line, ok, err := readLine(bufio.NewReader(strings.NewReader("\x1b[Dé\x1b[3~\x1b[C\x7fx\rnext")), io.Discard, "> ", "ab"); line != "aéx" || !ok || err != nil {
		t.Fatalf("readLine = %q, %v, %v", line, ok, err)
	}
	if line, ok, err := readLine(bufio.NewReader(strings.NewReader("abc\x1b")), io.Discard, "> ", ""); line != "" || ok || err != nil {
		t.Fatalf("Esc = %q, %v, %v", line, ok, err)
	}
}

func TestEscAfterPick(t *testing.T) {
	e := newTestEnv(t)
	defer func(k *bufio.Reader) { keyboard = k }(keyboard)
	// One byte per read, as typed: Enter picks Ops, the only free channel, then Esc leaves its whitelist.
	keyboard = bufio.NewReader(iotest.OneByteReader(strings.NewReader("\r\x1b")))
	if msg := e.a.pickTarget(row{ID: "w9:p1", Name: "new"}, false); msg != "" {
		t.Fatalf("pick then Esc = %q, want no message", msg)
	}
	targets, _ := e.a.readTargets()
	lists, _ := e.a.readWhitelists()
	if targets["w9:p1"].ID != "ch2" || len(lists["ch2"]) != 0 || len(lists["ch1"]) != 1 {
		t.Fatalf("the pick is kept and the whitelists unchanged: targets %+v, whitelists %+v", targets, lists)
	}
}

func TestWhitelistControl(t *testing.T) {
	e := mirroredEnv(t)
	e.setAgent(t, "idle")
	e.a.withState(func(panes map[string]*pane) error {
		panes["w1:p2"] = &pane{RootID: "root2", ChannelID: "ch2", Channel: "Ops", Status: "idle", Agent: "claude"}
		panes["w1:p3"] = &pane{RootID: "root3", ChannelID: "ch3", Channel: "QA", Status: "idle", Agent: "claude"}
		return nil
	})
	os.WriteFile(filepath.Join(e.a.stateDir, "whitelists.json"), []byte(`{"ch1":[{"id":"alice-id","username":"alice"}],"ch2":[{"id":"bob-id","username":"bob"}]}`), 0o600)
	refused := "You are not allowed to call me"
	ack := catalog["en"]["prompt.received"]
	for i, c := range []struct {
		ch, user, msg, want string
	}{
		{"ch1", "alice-id", "@herdr fix it", ack},
		{"ch1", "bob-id", "@herdr fix it", "@bob " + refused}, // whitelisted in ch2 only
		{"ch2", "bob-id", "@herdr go", ack},
		{"ch2", "alice-id", "@herdr help", "@alice " + refused},
		{"ch2", "alice-id", "@herdr #exec /clear", "@alice " + refused},
		{"ch2", "alice-id", "@herdr #capture /context", "@alice " + refused},
		{"ch3", "alice-id", "@herdr list", "@alice " + refused},
		{"ch3", "ghost-id", "@herdr go", refused}, // the author cannot be looked up: no mention
	} {
		n := len(e.mm.snapshot())
		e.a.handleEvent(posted(post{ID: "p" + string(rune('a'+i)), ChannelID: c.ch, UserID: c.user, Message: c.msg, CreateAt: int64(i + 1)}))
		if posts := e.mm.snapshot()[n:]; len(posts) != 1 || posts[0].Message != c.want {
			t.Errorf("%s %s %q: answers %+v, want %q", c.ch, c.user, c.msg, posts, c.want)
		}
	}
	if got := e.prompts(); got != "w1:p1|fix it\nw1:p2|go\n" {
		t.Fatalf("prompts = %q", got)
	}
	panes, _ := e.a.readPanes()
	if panes["w1:p1"].Asker != "alice" || panes["w1:p2"].Asker != "bob" {
		t.Fatalf("askers = %q %q", panes["w1:p1"].Asker, panes["w1:p2"].Asker)
	}
}

func TestReplyMentionsAsker(t *testing.T) {
	e, send, idle := threadTurns(t)
	os.WriteFile(filepath.Join(e.a.stateDir, "whitelists.json"), []byte(`{"ch1":[{"id":"alice-id","username":"alice"},{"id":"bob-id","username":"bob"}]}`), 0o600)
	root := e.mm.snapshot()[0].ID
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "bob-id", Message: "@herdr hi", CreateAt: 1}))
	e.appendTranscript(t, typed("u1", "hi"), assistant("a1", "m1", "text", "Hello.", false))
	n := len(e.mm.snapshot())
	idle("@bob Hello.") // threadTurns strips only "@alice "
	if p := e.mm.snapshot()[n]; p.Message != "@bob Hello." {
		t.Fatalf("reply = %q", p.Message)
	}
	send("again", 2)
	e.appendTranscript(t, typed("u2", "again"), assistant("a2", "m2", "text", "Sure.", false))
	idle("Sure.")
	if last := e.mm.snapshot()[len(e.mm.snapshot())-1]; last.Message != "@alice Sure." {
		t.Fatalf("reply = %q", last.Message)
	}
}

func TestProblemsTagAll(t *testing.T) {
	e := mirroredEnv(t)
	os.WriteFile(filepath.Join(e.herdrDir, "blocked"), nil, 0o644)
	e.a.handleEvent(posted(post{ID: "r1", ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr yes", CreateAt: 1}))
	os.Remove(filepath.Join(e.herdrDir, "blocked"))
	os.WriteFile(filepath.Join(e.herdrDir, "fail"), nil, 0o644)
	e.a.handleEvent(posted(post{ID: "r2", ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr go", CreateAt: 2}))
	posts := e.mm.snapshot()
	if len(posts) != 2 || !strings.HasPrefix(posts[0].Message, "@all ✋ The agent is waiting on a dialog") ||
		!strings.HasPrefix(posts[1].Message, "@all ❌ Could not prompt the agent: ") || posts[1].ChannelID != "ch1" {
		t.Fatalf("problem answers = %+v", posts)
	}
	e.a.capture("ch1", "root1", "") // no screen to read
	if posts = e.mm.snapshot(); len(posts) != 3 || !strings.HasPrefix(posts[2].Message, "@all ❌ Could not read the pane: ") || posts[2].ChannelID != "ch1" {
		t.Fatalf("capture failure = %+v", posts)
	}
	for _, key := range []string{"dialog", "prompt.failed", "prompt.blocked", "plugin.off"} {
		for _, l := range []string{"en", "vi"} {
			if !strings.HasPrefix(catalog[l][key], "@all ") {
				t.Errorf("%s %s does not tag @all: %q", l, key, catalog[l][key])
			}
		}
	}
	for _, key := range []string{"notice.started", "notice.reconnected", "prompt.received", "prompt.late"} {
		if strings.Contains(catalog["en"][key]+catalog["vi"][key], "@") {
			t.Errorf("%s must not tag anyone", key)
		}
	}
}
