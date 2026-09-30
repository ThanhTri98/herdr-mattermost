package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestStatusToPostFlow(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	e.appendTranscript(t, assistant("u1", "m1", "text", "old answer", false))
	os.WriteFile(filepath.Join(e.herdrDir, "panes.json"), []byte(`{"result":{"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "workspaces.json"), []byte(`{"result":{"workspaces":[{"workspace_id":"w1","label":"api","tab_count":1}]}}`), 0o644)

	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	posts := e.mm.snapshot()
	if len(posts) != 1 || posts[0].ChannelID != "ch1" || posts[0].RootID != "" {
		t.Fatalf("want one root post in the channel, got %+v", posts)
	}
	for _, want := range []string{"idle", "claude", "my_proj.x"} {
		if !strings.Contains(posts[0].Message, want) {
			t.Errorf("root post %q lacks %q", posts[0].Message, want)
		}
	}
	if strings.Contains(posts[0].Message, "w1:p1") {
		t.Errorf("root post %q shows the pane id", posts[0].Message)
	}

	e.setAgent(t, "working")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	if posts = e.mm.snapshot(); len(posts) != 1 || !strings.Contains(posts[0].Message, "working") {
		t.Fatalf("root post should be edited to working in place: %+v", posts)
	}

	e.appendTranscript(t, typed("t1", "typed in the terminal"), assistant("t2", "mt", "text", "terminal answer", false))
	e.setAgent(t, "idle")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	if posts = e.mm.snapshot(); len(posts) != 1 || !strings.Contains(posts[0].Message, "idle") {
		t.Fatalf("a turn typed in the terminal must not be posted: %+v", posts)
	}

	screen := "❯ earlier conversation\n\n" + strings.Repeat("─", 40) + "\n Bash command\n\n   rm -rf build\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n"
	os.WriteFile(filepath.Join(e.herdrDir, "screen.txt"), []byte(screen), 0o644)
	e.appendTranscript(t, typed("t3", "clean the build"), assistant("t4", "mb", "tool_use", "", false))
	e.setAgent(t, "blocked")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	if posts = e.mm.snapshot(); len(posts) != 1 || !strings.Contains(posts[0].Message, "blocked") {
		t.Fatalf("a dialog in a turn typed in the terminal must not be posted: %+v", posts)
	}

	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: posts[0].ID, UserID: "alice-id", Message: "@herdr fix the\nbug", CreateAt: 100}))
	e.setAgent(t, "working")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	e.appendTranscript(t,
		typed("u1", "fix the\nbug"),
		assistant("u2", "m2", "text", "Let me look.", false),
		assistant("u3", "m2", "tool_use", "", false),
		`{"type":"user","uuid":"u4","message":{"role":"user","content":[{"type":"tool_result","content":"`+strings.Repeat("x", 100000)+`"}]}}`,
		assistant("u5", "m3", "text", "Final answer.", false),
		assistant("u6", "m3", "text", "Second block.", false),
		assistant("u7", "m9", "text", "subagent chatter", true),
	)
	e.setAgent(t, "done")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	e.event(t, "pane.agent_status_changed", "w1:p1") // a late duplicate hook must not repost
	posts = e.mm.snapshot()
	if len(posts) != 3 || !strings.Contains(posts[0].Message, "done") || !strings.Contains(posts[1].Message, "Received") {
		t.Fatalf("want root edited to done, the acknowledgement and one reply, got %+v", posts)
	}
	if posts[2].RootID != posts[0].ID || posts[2].Message != "@alice Final answer.\n\nSecond block." {
		t.Fatalf("reply = %+v", posts[2])
	}

	e.setAgent(t, "blocked")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	posts = e.mm.snapshot()
	if len(posts) != 4 {
		t.Fatalf("want one dialog post, got %+v", posts)
	}
	d := posts[3]
	if d.RootID != posts[0].ID || !strings.HasPrefix(d.Message, "@alice") || !strings.Contains(d.Message, "Do you want to proceed?") || strings.Contains(d.Message, "earlier conversation") {
		t.Fatalf("dialog post = %q", d.Message)
	}

	if err := e.a.toggle("w1:p1"); err != nil { // switch off
		t.Fatal(err)
	}
	e.event(t, "pane.agent_status_changed", "w1:p1")
	posts = e.mm.snapshot()
	if len(posts) != 5 || !strings.Contains(posts[0].Message, "Mirroring stopped") {
		t.Fatalf("unshare should mark the root post and post one notice: %+v", posts)
	}
	want := "⚪ Mirroring stopped for **api** · claude · `my_proj.x` · [thread](" + e.a.mmURL + "/_redirect/pl/" + posts[0].ID + ")"
	if n := posts[4]; n.RootID != "" || n.ChannelID != "ch1" || n.Message != want {
		t.Fatalf("stop notice = %+v, want top-level %q", n, want)
	}
}

func TestReplyPostedWhenRootEditFails(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "working")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	e.mm.mu.Lock()
	e.mm.failPatch = true
	e.mm.mu.Unlock()
	path := filepath.Join(e.a.stateDir, "panes.json")
	var state map[string]map[string]any
	b, _ := os.ReadFile(path)
	json.Unmarshal(b, &state)
	state["w1:p1"]["prompted"] = "go" // a single reply, as saved before prompted became a list
	b, _ = json.Marshal(state)
	os.WriteFile(path, b, 0o600)
	e.appendTranscript(t, typed("u0", "go"), assistant("u1", "m1", "text", "Done.", false))
	e.setAgent(t, "done")
	if err := e.a.event("pane.agent_status_changed", []byte(`{"data":{"pane_id":"w1:p1"}}`)); err == nil || !strings.Contains(err.Error(), "edit time limit") {
		t.Fatalf("event = %v, want the edit error", err)
	}
	if posts := e.mm.snapshot(); len(posts) != 2 || posts[1].Message != "@alice Done." {
		t.Fatalf("the reply must be posted even when the root edit fails: %+v", posts)
	}
}

func TestPaneMovedKeepsThread(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	raw := `{"event":"pane_moved","data":{"type":"pane_moved","previous_pane_id":"w1:p1","previous_workspace_id":"w1","previous_tab_id":"w1:t1","pane":{"pane_id":"w2:p1","workspace_id":"w2"}}}`
	if err := e.a.event("pane.moved", []byte(raw)); err != nil {
		t.Fatal(err)
	}
	posts := e.mm.snapshot()
	if len(posts) != 1 || strings.Contains(posts[0].Message, "w2:p1") {
		t.Fatalf("root post should not show the pane id: %+v", posts)
	}
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: posts[0].ID, UserID: "alice-id", Message: "@herdr go on", CreateAt: 100}))
	if got := e.prompts(); got != "w2:p1|go on\n" {
		t.Fatalf("prompts = %q", got)
	}
}

func TestPaneClosedStopsMirroring(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	panesJSON := filepath.Join(e.herdrDir, "panes.json")
	os.WriteFile(filepath.Join(e.herdrDir, "workspaces.json"), []byte(`{"result":{"workspaces":[{"workspace_id":"w1","label":"api","tab_count":1}]}}`), 0o644)
	os.WriteFile(panesJSON, []byte(`{"result":{"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1"}]}}`), 0o644)
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(panesJSON, []byte(`{"result":{"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1","label":"web"}]}}`), 0o644)
	e.event(t, "pane.agent_status_changed", "w1:p1") // picks up the rename
	os.Remove(panesJSON)                             // herdr no longer lists a closed pane
	e.event(t, "pane.closed", "w1:p1")
	posts := e.mm.snapshot()
	if !strings.Contains(posts[0].Message, "Pane closed") {
		t.Fatalf("root = %q", posts[0].Message)
	}
	want := "⚫ Pane closed, mirroring stopped for **web** · claude · `my_proj.x` · [thread](" + e.a.mmURL + "/_redirect/pl/" + posts[0].ID + ")"
	if n := posts[len(posts)-1]; len(posts) != 2 || n.RootID != "" || n.ChannelID != "ch1" || n.Message != want {
		t.Fatalf("close notice = %+v, want one top-level %q", posts, want)
	}
	e.a.withState(func(panes map[string]*pane) error {
		if len(panes) != 0 {
			t.Errorf("closed pane still mirrored: %v", panes)
		}
		return nil
	})
}

func TestStopNoticePostedWhenRootEditFails(t *testing.T) {
	for _, stop := range []string{"toggle", "pane.closed"} {
		e := newTestEnv(t)
		e.setAgent(t, "idle")
		if err := e.a.toggle("w1:p1"); err != nil {
			t.Fatal(err)
		}
		e.mm.mu.Lock()
		e.mm.failPatch = true
		e.mm.mu.Unlock()
		var err error
		if stop == "toggle" {
			err = e.a.toggle("w1:p1")
		} else {
			err = e.a.event(stop, []byte(`{"data":{"pane_id":"w1:p1"}}`))
		}
		if err == nil || !strings.Contains(err.Error(), "edit time limit") {
			t.Fatalf("%s = %v, want the edit error", stop, err)
		}
		if posts := e.mm.snapshot(); len(posts) != 2 || posts[1].RootID != "" || !strings.Contains(posts[1].Message, "topped for ") || strings.Contains(posts[1].Message, "w1:p1") {
			t.Fatalf("%s: the stop notice must be posted even when the root edit fails: %+v", stop, posts)
		}
	}
}

// TestPromptedFollowsThread posts turns resumed by a background task and every recent thread reply's
// turn, and keeps a failed prompt or a terminal prompt with an image from taking over the thread's.
func TestPromptedFollowsThread(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "working")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	root := e.mm.snapshot()[0].ID
	idle := func(want string) {
		t.Helper()
		e.setAgent(t, "idle")
		e.event(t, "pane.agent_status_changed", "w1:p1")
		e.setAgent(t, "working")
		e.event(t, "pane.agent_status_changed", "w1:p1")
		if posts := e.mm.snapshot(); strings.TrimPrefix(posts[len(posts)-1].Message, "@alice ") != want {
			t.Fatalf("last post = %q, want %q: %+v", posts[len(posts)-1].Message, want, posts)
		}
	}
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr run the tests", CreateAt: 100}))
	os.WriteFile(filepath.Join(e.herdrDir, "blocked"), nil, 0o644)
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr yes", CreateAt: 101}))
	os.Remove(filepath.Join(e.herdrDir, "blocked"))
	e.appendTranscript(t, typed("t1", "run the tests"), assistant("t2", "m1", "text", "Started them in the background.", false))
	idle("Started them in the background.")

	e.appendTranscript(t, `{"type":"user","uuid":"t3","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>done</task-notification>"}}`,
		assistant("t4", "m2", "text", "All tests pass.", false))
	idle("All tests pass.")

	e.appendTranscript(t, typed("t5", "yes"), assistant("t6", "m3", "text", "terminal answer", false))
	idle("All tests pass.")

	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr check the logs", CreateAt: 102}))
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr and the config", CreateAt: 103}))
	e.appendTranscript(t, typed("t7", "check the logs"), assistant("t8", "m4", "text", "Logs are clean.", false))
	idle("Logs are clean.")
	e.appendTranscript(t, typed("t9", "and the config"), assistant("t10", "m5", "text", "Config is fine.", false))
	idle("Config is fine.")

	e.appendTranscript(t, `{"type":"user","uuid":"t11","imagePasteIds":[1],"message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}},{"type":"text","text":"why does this fail?"}]}}`,
		assistant("t12", "m6", "text", "It fails because of the image.", false))
	idle("Config is fine.")

	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr queued from the thread", CreateAt: 104}))
	e.appendTranscript(t, typed("t13", "typed in the terminal"), assistant("t14", "m7", "text", "terminal answer", false), typed("t15", "queued from the thread"))
	idle("📥 Received - the agent is working on it.")
	e.appendTranscript(t, assistant("t16", "m8", "text", "Thread answer.", false))
	idle("Thread answer.")

	os.WriteFile(filepath.Join(e.herdrDir, "screen.txt"), []byte(strings.Repeat("─", 40)+"\n Do you want to proceed?\n ❯ 1. Yes\n"), 0o644)
	e.appendTranscript(t, typed("t17", "clean the build"), assistant("t18", "m9", "tool_use", "", false))
	e.setAgent(t, "blocked")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	if posts := e.mm.snapshot(); posts[len(posts)-1].Message != "@alice Thread answer." {
		t.Fatalf("a dialog in a turn typed in the terminal must not be posted: %+v", posts)
	}
}

// TestQueuedThreadRepliesAllPosted posts the reply to every thread turn since the last idle, oldest first,
// keeps terminal turns off, and considers only the newest reply when the last one handled is unknown.
func TestQueuedThreadRepliesAllPosted(t *testing.T) {
	e, send, idle := threadTurns(t, typed("t0", "before sharing"), assistant("r0", "m0", "text", "old answer", false))
	send("first", 100)
	send("second", 101)
	e.appendTranscript(t, typed("t1", "first"), assistant("t2", "m1", "text", "Working on it.", false), assistant("t3", "m2", "text", "First answer.", false),
		typed("t4", "second"), assistant("t5", "m3", "text", "Second answer.", false))
	idle("First answer.", "Second answer.")

	send("third", 102)
	e.appendTranscript(t, typed("t6", "third"), assistant("t7", "m4", "text", "Third answer.", false),
		typed("t8", "typed in the terminal"), assistant("t9", "m5", "text", "terminal answer", false))
	idle("Third answer.")

	e.a.withState(func(panes map[string]*pane) error { panes["w1:p1"].LastReply = "gone"; return nil })
	e.appendTranscript(t, typed("t10", "first"), assistant("t11", "m6", "text", "Old again.", false), typed("t12", "second"), assistant("t13", "m7", "text", "Newest.", false))
	idle("Newest.")
}

// TestQueuedThreadRepliesInNewTranscript posts every queued thread turn of a transcript that had no turns
// yet, whether the pane was shared before its first prompt, a new agent started in it or /clear started a
// new one with no event, and reposts only a turn that went on after a usage limit.
func TestQueuedThreadRepliesInNewTranscript(t *testing.T) {
	e, send, idle := threadTurns(t)
	send("first", 100)
	send("second", 101)
	e.appendTranscript(t, typed("t1", "first"), assistant("t2", "m1", "text", "First answer.", false),
		typed("t3", "second"), assistant("t4", "m2", "text", "Second answer.", false))
	idle("First answer.", "Second answer.")

	os.Remove(e.transcript)
	idle()
	send("again one", 102)
	send("again two", 103)
	e.appendTranscript(t, typed("b1", "again one"), assistant("b2", "mb1", "text", "Again one answer.", false),
		typed("b3", "again two"), assistant("b4", "mb2", "text", "Again two answer.", false))
	idle("Again one answer.", "Again two answer.")

	os.WriteFile(e.transcript, nil, 0o644)
	e.appendTranscript(t, `{"type":"user","isMeta":true,"uuid":"c0","message":{"role":"user","content":"<local-command-caveat>Caveat</local-command-caveat>"}}`,
		typed("c1", "<command-name>/clear</command-name>\n            <command-message>clear</command-message>\n            <command-args></command-args>"),
		`{"type":"system","subtype":"local_command","uuid":"c2","content":"<local-command-stdout></local-command-stdout>"}`)
	send("third", 104)
	send("fourth", 105)
	e.appendTranscript(t, typed("t5", "third"), assistant("t6", "m3", "text", "Third answer.", false),
		typed("t7", "fourth"), assistant("t8", "m4", "text", "Fourth answer.", false))
	idle("Third answer.", "Fourth answer.")

	send("fifth", 106)
	e.appendTranscript(t, typed("t9", "fifth"), assistant("t10", "m5", "text", "You've hit your session limit · resets 12:20am", false))
	idle("You've hit your session limit · resets 12:20am")
	e.appendTranscript(t, `{"type":"user","isMeta":true,"origin":{"kind":"auto-continuation"},"uuid":"t11","message":{"role":"user","content":"Your claude.ai usage limit has reset. Continue the task you were working on when the limit was reached; do not repeat work."}}`,
		assistant("t12", "m6", "text", "Fifth answer.", false))
	idle("Fifth answer.")
}

func TestReplyWrittenAfterIdleIsPostedForItsTurn(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	root := e.mm.snapshot()[0].ID
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr late one", CreateAt: 100}))
	e.appendTranscript(t, typed("u1", "late one"))
	go func() { // Claude writes the final entry just after herdr reports idle
		time.Sleep(300 * time.Millisecond)
		e.appendTranscript(t, assistant("u2", "m1", "text", "Written late.", false))
	}()
	e.event(t, "pane.agent_status_changed", "w1:p1")
	posts := e.mm.snapshot()
	if last := posts[len(posts)-1]; last.RootID != root || last.Message != "@alice Written late." {
		t.Fatalf("the reply must be posted for its own turn: %+v", posts)
	}

	// Text written before a tool call is not the answer while the turn still ends in the tool step.
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr fix the bug", CreateAt: 101}))
	e.appendTranscript(t, typed("u3", "fix the bug"), assistant("u4", "m2", "text", "Let me look.", false), assistant("u5", "m2", "tool_use", "", false),
		`{"type":"user","uuid":"u6","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`)
	go func() {
		time.Sleep(300 * time.Millisecond)
		e.appendTranscript(t, assistant("u7", "m3", "text", "Fixed.", false))
	}()
	e.event(t, "pane.agent_status_changed", "w1:p1")
	posts = e.mm.snapshot()
	if last := posts[len(posts)-1]; last.RootID != root || last.Message != "@alice Fixed." || slices.ContainsFunc(posts, func(p post) bool { return p.Message == "Let me look." }) {
		t.Fatalf("the final text must be posted as the reply: %+v", posts)
	}

	// A turn typed in the terminal is never waited for.
	e.appendTranscript(t, typed("u8", "typed in the terminal"))
	start := time.Now()
	e.event(t, "pane.agent_status_changed", "w1:p1")
	if d := time.Since(start); d > transcriptSettle/2 {
		t.Fatalf("a terminal turn waited %s", d)
	}
}
