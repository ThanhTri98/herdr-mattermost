package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestMain runs the real main in a child process started with HERDR_MM_MAIN=1.
func TestMain(m *testing.M) {
	if os.Getenv("HERDR_MM_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// fakeMM is a Mattermost server that keeps posts in memory.
type fakeMM struct {
	mu        sync.Mutex
	posts     []*post
	now       int64 // create_at of the newest post
	failPatch bool  // answer post edits with 500
	failLogin int   // answer this many logins with 503
}

// add stores a post as if someone else sent it.
func (f *fakeMM) add(p post) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now++
	p.ID, p.CreateAt = fmt.Sprintf("post%d", len(f.posts)+1), f.now
	f.posts = append(f.posts, &p)
}

func (f *fakeMM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/api/v4/websocket" { // reads the authentication challenge, then drops the connection
		if conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil); err == nil {
			conn.ReadMessage()
			conn.Close()
		}
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, `{"message":"invalid token"}`, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	if f.failLogin > 0 && path == "/users/me" {
		f.failLogin--
		http.Error(w, `{"message":"unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == "GET" && path == "/users/me":
		fmt.Fprint(w, `{"id":"bot"}`)
	case r.Method == "GET" && path == "/users/username/alice":
		fmt.Fprint(w, `{"id":"alice-id"}`)
	case r.Method == "POST" && path == "/channels/direct":
		fmt.Fprint(w, `{"id":"dm"}`)
	case r.Method == "POST" && path == "/posts":
		p := &post{}
		json.NewDecoder(r.Body).Decode(p)
		f.now++
		p.ID, p.UserID, p.CreateAt = fmt.Sprintf("post%d", len(f.posts)+1), "bot", f.now
		f.posts = append(f.posts, p)
		json.NewEncoder(w).Encode(p)
	case r.Method == "GET" && path == "/channels/dm/posts":
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		list := map[string]map[string]*post{"posts": {}}
		for _, p := range f.posts {
			if p.ChannelID == "dm" && p.CreateAt > since {
				list["posts"][p.ID] = p
			}
		}
		json.NewEncoder(w).Encode(list)
	case r.Method == "PUT" && strings.HasSuffix(path, "/patch") && f.failPatch:
		http.Error(w, `{"message":"edit time limit"}`, http.StatusInternalServerError)
	case r.Method == "PUT" && strings.HasSuffix(path, "/patch"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/posts/"), "/patch")
		for _, p := range f.posts {
			if p.ID == id {
				json.NewDecoder(r.Body).Decode(p)
				fmt.Fprint(w, `{}`)
				return
			}
		}
		http.NotFound(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeMM) snapshot() []post {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []post
	for _, p := range f.posts {
		out = append(out, *p)
	}
	return out
}

// fakeHerdr answers agent get/read from files in its directory and logs prompts.
const fakeHerdr = `#!/bin/sh
d=$(dirname "$0")
case "$1 $2" in
"agent get") cat "$d/agent.json" ;;
"agent read") cat "$d/screen.txt" ;;
"agent list") cat "$d/agents.json" 2>/dev/null || echo '{"result":{"agents":[]}}' ;;
"pane list"|"tab list"|"workspace list") cat "$d/$1s.json" 2>/dev/null || echo '{"result":{}}' ;;
"status server") [ -e "$d/stopped" ] && echo '{"running":false}' || echo '{"running":true}' ;;
"plugin list")
  [ -e "$d/disabled" ] && on=false || on=true
  [ "$3 $4 $5" = "--plugin herdr-mattermost --json" ] && echo "{\"result\":{\"plugins\":[{\"plugin_id\":\"$4\",\"enabled\":$on}]}}" || echo '{"result":{"plugins":[]}}' ;;
"agent prompt")
  printf '%s|%s\n' "$3" "$4" >> "$d/prompts.log"
  if [ -e "$d/blocked" ]; then echo '{"error":{"code":"agent_blocked","message":"blocked"},"id":"x"}' >&2; exit 1; fi
  echo '{"id":"x","result":{"type":"agent_prompted"}}' ;;
*) echo "unexpected $*" >&2; exit 2 ;;
esac
`

type testEnv struct {
	a          *app
	mm         *fakeMM
	herdrDir   string
	transcript string
}

func newTestEnv(t *testing.T) *testEnv {
	mm := &fakeMM{}
	srv := httptest.NewServer(mm)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	herdrDir := filepath.Join(dir, "herdr")
	os.MkdirAll(herdrDir, 0o755)
	os.WriteFile(filepath.Join(herdrDir, "herdr"), []byte(fakeHerdr), 0o755)
	state := filepath.Join(dir, "state")
	os.MkdirAll(state, 0o755)
	claude := filepath.Join(dir, "claude")
	// Not the pane's cwd: Claude may have been started in another directory.
	transcript := filepath.Join(claude, "projects", "-somewhere-else", "sess-1.jsonl")
	os.MkdirAll(filepath.Dir(transcript), 0o755)
	a := &app{mmURL: srv.URL, token: "tok", user: "alice", envPath: filepath.Join(dir, ".env"),
		stateDir: state, herdrBin: filepath.Join(herdrDir, "herdr"), claudeDir: claude}
	return &testEnv{a, mm, herdrDir, transcript}
}

func (e *testEnv) setAgent(t *testing.T, status string) {
	t.Helper()
	info := fmt.Sprintf(`{"id":"cli:agent:get","result":{"type":"agent_info","agent":{"agent":"claude","agent_status":%q,
		"cwd":"/work/my_proj.x","pane_id":"w1:p1","agent_session":{"agent":"claude","kind":"id","source":"herdr:claude","value":"sess-1"}}}}`, status)
	os.WriteFile(filepath.Join(e.herdrDir, "agent.json"), []byte(info), 0o644)
}

func (e *testEnv) appendTranscript(t *testing.T, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(e.transcript, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		fmt.Fprintln(f, l)
	}
}

func assistant(uuid, msgID, blockType, text string, sidechain bool) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": uuid, "isSidechain": sidechain,
		"message": map[string]any{"id": msgID, "role": "assistant", "content": []map[string]string{{"type": blockType, "text": text}}}})
	return string(b)
}

func typed(uuid, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid, "message": map[string]any{"role": "user", "content": text}})
	return string(b)
}

func (e *testEnv) event(t *testing.T, name, pane string) {
	t.Helper()
	raw := fmt.Sprintf(`{"event":%q,"data":{"pane_id":%q,"workspace_id":"w1","agent_status":"idle"}}`, strings.ReplaceAll(name, ".", "_"), pane)
	if err := e.a.event(name, []byte(raw)); err != nil {
		t.Fatal(err)
	}
}

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
	if len(posts) != 1 || posts[0].ChannelID != "dm" || posts[0].RootID != "" {
		t.Fatalf("want one root post in the DM, got %+v", posts)
	}
	for _, want := range []string{"idle", "claude", "my_proj.x", "w1:p1"} {
		if !strings.Contains(posts[0].Message, want) {
			t.Errorf("root post %q lacks %q", posts[0].Message, want)
		}
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

	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: posts[0].ID, UserID: "alice-id", Message: "fix the\nbug", CreateAt: 100}))
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
	if posts[2].RootID != posts[0].ID || posts[2].Message != "Final answer.\n\nSecond block." {
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
	want := "⚪ Mirroring stopped for **api** · pane `w1:p1` · claude · `my_proj.x` · [thread](" + e.a.mmURL + "/_redirect/pl/" + posts[0].ID + ")"
	if n := posts[4]; n.RootID != "" || n.ChannelID != "dm" || n.Message != want {
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
	if posts := e.mm.snapshot(); len(posts) != 2 || posts[1].Message != "Done." {
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
	if len(posts) != 1 || !strings.Contains(posts[0].Message, "`w2:p1`") {
		t.Fatalf("root post should name the new pane id: %+v", posts)
	}
	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: posts[0].ID, UserID: "alice-id", Message: "go on", CreateAt: 100}))
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
	want := "⚫ Pane closed, mirroring stopped for **web** · pane `w1:p1` · claude · `my_proj.x` · [thread](" + e.a.mmURL + "/_redirect/pl/" + posts[0].ID + ")"
	if n := posts[len(posts)-1]; len(posts) != 2 || n.RootID != "" || n.ChannelID != "dm" || n.Message != want {
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
		if posts := e.mm.snapshot(); len(posts) != 2 || posts[1].RootID != "" || !strings.Contains(posts[1].Message, "topped for pane `w1:p1`") {
			t.Fatalf("%s: the stop notice must be posted even when the root edit fails: %+v", stop, posts)
		}
	}
}

// TestThreadPromptTranscriptShapes posts the reply to a thread prompt however Claude records it.
func TestThreadPromptTranscriptShapes(t *testing.T) {
	queued := func(prompt, mode string) string {
		b, _ := json.Marshal(map[string]any{"type": "attachment", "uuid": "q-" + prompt, "attachment": map[string]string{"type": "queued_command", "commandMode": mode, "prompt": prompt}})
		return string(b)
	}
	for name, c := range map[string]struct {
		sent  []string
		lines []string
	}{
		"follow-up queued into the running turn": {[]string{"fix it", "and the docs"},
			[]string{typed("t1", "fix it"), assistant("t2", "m1", "text", "Looking.", false), queued("and the docs", "prompt"), queued("<task-notification>x</task-notification>", "task-notification")}},
		"thread reply queued into a terminal turn": {[]string{"and the docs"},
			[]string{typed("t1", "typed in the terminal"), assistant("t2", "m1", "text", "Looking.", false), queued("and the docs", "prompt")}},
		"compaction summary mid-turn": {[]string{"fix it"},
			[]string{typed("t1", "fix it"), `{"type":"user","uuid":"t2","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued from a previous conversation."}}`}},
		"task notification mid-turn": {[]string{"fix it"},
			[]string{typed("t1", "fix it"), `{"type":"user","uuid":"t2","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>x</task-notification>"}}`}},
		"prompt mentioning a command tag": {[]string{"sao <command-name> không khớp?"},
			[]string{typed("t1", "sao <command-name> không khớp?")}},
		"pasted text": {[]string{"look at this\nline one\nline two"},
			[]string{typed("t1", "look at this\n\n<pasted_content id=\"c737\">\nline one\nline two\n</pasted_content id=\"c737\">\n")}},
		"slash command": {[]string{"/review  foo"},
			[]string{typed("t1", "<command-message>review</command-message>\n<command-name>/review</command-name>\n<command-args>foo</command-args>"),
				`{"type":"user","uuid":"t2","isMeta":true,"message":{"role":"user","content":[{"type":"text","text":"skill body"}]}}`}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			e.setAgent(t, "working")
			if err := e.a.toggle("w1:p1"); err != nil {
				t.Fatal(err)
			}
			root := e.mm.snapshot()[0].ID
			for i, m := range c.sent {
				e.a.handleEvent(posted(post{ChannelID: "dm", RootID: root, UserID: "alice-id", Message: m, CreateAt: int64(100 + i)}))
			}
			e.appendTranscript(t, append(c.lines, assistant("r1", "m9", "text", "All done.", false))...)
			e.setAgent(t, "idle")
			e.event(t, "pane.agent_status_changed", "w1:p1")
			if posts := e.mm.snapshot(); posts[len(posts)-1].Message != "All done." || posts[len(posts)-1].RootID != root {
				t.Fatalf("the reply to a thread prompt must be posted: %+v", posts)
			}
		})
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
		if posts := e.mm.snapshot(); posts[len(posts)-1].Message != want {
			t.Fatalf("last post = %q, want %q: %+v", posts[len(posts)-1].Message, want, posts)
		}
	}
	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: root, UserID: "alice-id", Message: "run the tests", CreateAt: 100}))
	os.WriteFile(filepath.Join(e.herdrDir, "blocked"), nil, 0o644)
	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: root, UserID: "alice-id", Message: "yes", CreateAt: 101}))
	os.Remove(filepath.Join(e.herdrDir, "blocked"))
	e.appendTranscript(t, typed("t1", "run the tests"), assistant("t2", "m1", "text", "Started them in the background.", false))
	idle("Started them in the background.")

	e.appendTranscript(t, `{"type":"user","uuid":"t3","origin":{"kind":"task-notification"},"message":{"role":"user","content":"<task-notification>done</task-notification>"}}`,
		assistant("t4", "m2", "text", "All tests pass.", false))
	idle("All tests pass.")

	e.appendTranscript(t, typed("t5", "yes"), assistant("t6", "m3", "text", "terminal answer", false))
	idle("All tests pass.")

	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: root, UserID: "alice-id", Message: "check the logs", CreateAt: 102}))
	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: root, UserID: "alice-id", Message: "and the config", CreateAt: 103}))
	e.appendTranscript(t, typed("t7", "check the logs"), assistant("t8", "m4", "text", "Logs are clean.", false))
	idle("Logs are clean.")
	e.appendTranscript(t, typed("t9", "and the config"), assistant("t10", "m5", "text", "Config is fine.", false))
	idle("Config is fine.")

	e.appendTranscript(t, `{"type":"user","uuid":"t11","imagePasteIds":[1],"message":{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}},{"type":"text","text":"why does this fail?"}]}}`,
		assistant("t12", "m6", "text", "It fails because of the image.", false))
	idle("Config is fine.")

	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: root, UserID: "alice-id", Message: "queued from the thread", CreateAt: 104}))
	e.appendTranscript(t, typed("t13", "typed in the terminal"), assistant("t14", "m7", "text", "terminal answer", false), typed("t15", "queued from the thread"))
	idle("📥 Received - the agent is working on it.")
	e.appendTranscript(t, assistant("t16", "m8", "text", "Thread answer.", false))
	idle("Thread answer.")

	os.WriteFile(filepath.Join(e.herdrDir, "screen.txt"), []byte(strings.Repeat("─", 40)+"\n Do you want to proceed?\n ❯ 1. Yes\n"), 0o644)
	e.appendTranscript(t, typed("t17", "clean the build"), assistant("t18", "m9", "tool_use", "", false))
	e.setAgent(t, "blocked")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	if posts := e.mm.snapshot(); posts[len(posts)-1].Message != "Thread answer." {
		t.Fatalf("a dialog in a turn typed in the terminal must not be posted: %+v", posts)
	}
}

func TestLastReplyAndTruncate(t *testing.T) {
	if text, uuid, prompts, turn, err := lastReply(""); text != "" || uuid != "" || prompts != nil || turn != nil || err != nil {
		t.Fatalf("no transcript: %q %q %q %q %v", text, uuid, prompts, turn, err)
	}
	e := newTestEnv(t)
	e.appendTranscript(t, `{"type":"user","message":{"content":"a plain string"}}`, assistant("u1", "m1", "text", "hi", false), "not json", typed("u2", "next"))
	if text, uuid, prompts, turn, err := lastReply(e.transcript); text != "hi" || uuid != "u1" || len(prompts) != 1 || prompts[0] != "a plain string" || len(turn) != 1 || turn[0] != "next" || err != nil {
		t.Fatalf("got %q %q %q %q %v", text, uuid, prompts, turn, err)
	}
	long := strings.Repeat("é", maxPost+10)
	if got := truncate(long, maxPost); len([]rune(got)) != maxPost || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("truncate: %d runes", len([]rune(got)))
	}
}

func posted(p post) []byte {
	b, _ := json.Marshal(p)
	ev, _ := json.Marshal(map[string]any{"event": "posted", "data": map[string]string{"post": string(b)}})
	return ev
}

func mirroredEnv(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.a.botID, e.a.userID, e.a.dmID = "bot", "alice-id", "dm"
	e.a.withState(func(panes map[string]*pane) error {
		panes["w1:p1"] = &pane{RootID: "root1", ChannelID: "dm", Status: "idle", Agent: "claude", Cwd: "/work/proj"}
		return nil
	})
	return e
}

func (e *testEnv) prompts() string {
	b, _ := os.ReadFile(filepath.Join(e.herdrDir, "prompts.log"))
	return string(b)
}

func TestReplyPromptsAgent(t *testing.T) {
	e := mirroredEnv(t)
	e.a.handleEvent(posted(post{ID: "r1", ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "--fix the bug", CreateAt: 1}))
	if got := e.prompts(); got != "w1:p1|--fix the bug\n" {
		t.Fatalf("prompts = %q", got)
	}
	if posts := e.mm.snapshot(); len(posts) != 1 || posts[0].RootID != "root1" || posts[0].Message != "📥 Received - the agent is working on it." {
		t.Fatalf("a delivered prompt gets one acknowledgement: %+v", posts)
	}

	os.WriteFile(filepath.Join(e.herdrDir, "blocked"), nil, 0o644)
	e.a.handleEvent(posted(post{ID: "r2", ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "yes", CreateAt: 2}))
	posts := e.mm.snapshot()
	if len(posts) != 2 || posts[1].RootID != "root1" || !strings.Contains(posts[1].Message, "Approve or answer it on the machine") {
		t.Fatalf("blocked answer = %+v", posts)
	}

	e.a.handleEvent(posted(post{ID: "r3", ChannelID: "dm", UserID: "alice-id", Message: " List ", CreateAt: 3}))
	posts = e.mm.snapshot()
	if len(posts) != 3 || posts[2].RootID != "" || !strings.Contains(posts[2].Message, "w1:p1") || !strings.Contains(posts[2].Message, "/_redirect/pl/root1") {
		t.Fatalf("list answer = %+v", posts)
	}
}

func TestIgnoresEveryoneButMMUser(t *testing.T) {
	e := mirroredEnv(t)
	for _, p := range []post{
		{ChannelID: "dm", RootID: "root1", UserID: "bob-id", Message: "rm -rf /"},
		{ChannelID: "dm", RootID: "root1", UserID: "bot", Message: "echo"},
		{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "hook", Props: map[string]any{"from_webhook": "true"}},
		{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "bot", Props: map[string]any{"from_bot": "true"}},
		{ChannelID: "town-square", RootID: "root1", UserID: "alice-id", Message: "wrong channel"},
		{ChannelID: "dm", RootID: "other-thread", UserID: "alice-id", Message: "not mirrored", CreateAt: 1},
		{ChannelID: "dm", UserID: "bob-id", Message: "list"},
	} {
		e.a.handleEvent(posted(p))
	}
	if got := e.prompts(); got != "" {
		t.Fatalf("prompted for an ignored post: %q", got)
	}
	if posts := e.mm.snapshot(); len(posts) != 0 {
		t.Fatalf("answered an ignored post: %+v", posts)
	}
}

func TestCatchUpDeliversMissedReplies(t *testing.T) {
	e := mirroredEnv(t)
	e.mm.add(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "handled before"})
	e.a.lastPost = 1
	e.mm.add(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "missed 1"})
	e.mm.add(post{ChannelID: "dm", RootID: "root1", UserID: "bob-id", Message: "not alice"})
	e.mm.add(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "", DeleteAt: 9})
	e.mm.add(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "missed 2"})
	if err := e.a.catchUp(); err != nil {
		t.Fatal(err)
	}
	if got := e.prompts(); got != "w1:p1|missed 1\nw1:p1|missed 2\n" {
		t.Fatalf("prompts = %q", got)
	}
	var notes, acks int
	for _, p := range e.mm.snapshot() {
		if p.UserID == "bot" && p.RootID == "root1" && strings.Contains(p.Message, "Delivered late") {
			notes++
		}
		if strings.Contains(p.Message, "Received") {
			acks++
		}
	}
	if acks != 0 {
		t.Fatalf("a late reply gets only the late note: %+v", e.mm.snapshot())
	}
	if notes != 2 {
		t.Fatalf("want a late note per caught-up reply, got %d: %+v", notes, e.mm.snapshot())
	}

	missed2 := e.mm.snapshot()[4]
	e.a.handleEvent(posted(missed2)) // the WebSocket delivering it too must not prompt twice
	if err := e.a.catchUp(); err != nil {
		t.Fatal(err)
	}
	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "live", CreateAt: 100}))
	if got := e.prompts(); got != "w1:p1|missed 1\nw1:p1|missed 2\nw1:p1|live\n" {
		t.Fatalf("prompts = %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(e.a.stateDir, "last_post")); string(b) != "100" {
		t.Fatalf("last_post = %q", b)
	}
}

func TestConnectAnnounced(t *testing.T) {
	e := mirroredEnv(t)
	for range 2 {
		if err := e.a.listenOnce(); err == nil {
			t.Fatal("listenOnce returned without the connection dropping")
		}
	}
	var got []string
	for _, p := range e.mm.snapshot() {
		if p.ChannelID == "dm" && p.RootID == "" {
			got = append(got, p.Message)
		}
	}
	if want := []string{"🔌 Connected: the herdr-mm daemon started.", "🔌 Reconnected after the connection dropped."}; !slices.Equal(got, want) {
		t.Fatalf("DM posts = %q, want %q", got, want)
	}
}

func TestPluginOffStopsDaemon(t *testing.T) {
	for _, flag := range []string{"disabled", "stopped"} {
		e := mirroredEnv(t)
		os.WriteFile(filepath.Join(e.herdrDir, flag), nil, 0o644)
		err := e.a.handleEvent(posted(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "hi", CreateAt: 1}))
		if !errors.Is(err, errPluginOff) {
			t.Fatalf("%s: handleEvent = %v", flag, err)
		}
		if got := e.prompts(); got != "" {
			t.Fatalf("%s: prompted %q", flag, got)
		}
		if posts := e.mm.snapshot(); len(posts) != 1 || posts[0].RootID != "root1" || !strings.Contains(posts[0].Message, "not typed into any agent") {
			t.Fatalf("%s: answer = %+v", flag, posts)
		}
	}
}

func TestConnectRetriesUntilMattermostAnswers(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = time.Millisecond
	e := newTestEnv(t)
	e.mm.failLogin = 2
	if err := e.a.connectRetry(); err != nil || e.a.dmID != "dm" || e.mm.failLogin != 0 {
		t.Fatalf("connectRetry = %v, dm %q, logins left to fail %d", err, e.a.dmID, e.mm.failLogin)
	}
	e.a.token = "wrong"
	if err := e.a.connectRetry(); err == nil || !strings.Contains(err.Error(), "mattermost login failed") {
		t.Fatalf("a rejected token must not be retried: %v", err)
	}
}

func TestStopEndsDaemon(t *testing.T) {
	e := newTestEnv(t)
	if err := e.a.stop(); err != nil {
		t.Fatalf("stop with no daemon = %v", err)
	}
	// Mattermost is unreachable, so the daemon sits in its connect retry loop.
	os.WriteFile(e.a.envPath, []byte("MM_URL=http://127.0.0.1:1\nMM_BOT_TOKEN=tok\nMM_USER=alice\n"), 0o600)
	cmd := exec.Command(os.Args[0], "daemon")
	cmd.Env = append(os.Environ(), "HERDR_MM_MAIN=1", "HERDR_PLUGIN_CONFIG_DIR="+filepath.Dir(e.a.envPath), "HERDR_PLUGIN_STATE_DIR="+e.a.stateDir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	for i := 0; ; i++ {
		if b, _ := os.ReadFile(filepath.Join(e.a.stateDir, "daemon.lock")); string(b) == strconv.Itoa(cmd.Process.Pid) {
			break
		}
		if i == 500 {
			t.Fatal("the daemon never took its lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := e.a.stop(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil || !strings.Contains(err.Error(), "terminated") {
		t.Fatalf("daemon exit = %v, want terminated", err)
	}
}

func TestConfigErrors(t *testing.T) {
	e := newTestEnv(t)
	e.a.token, e.a.user = "", ""
	if err := e.a.requireMM(); err == nil || !strings.Contains(err.Error(), "missing MM_BOT_TOKEN, MM_USER in") {
		t.Fatalf("requireMM = %v", err)
	}
	e.a.token, e.a.user = "wrong", "alice"
	if err := e.a.connect(); err == nil || !strings.Contains(err.Error(), "mattermost login failed") {
		t.Fatalf("connect = %v", err)
	}
	os.WriteFile(e.a.envPath, []byte("# comment\nexport MM_URL=\"https://mm.example/\"\nMM_BOT_TOKEN='t=1'\n\nMM_USER=@alice\n"), 0o600)
	env, err := readEnv(e.a.envPath)
	if err != nil || env["MM_URL"] != "https://mm.example/" || env["MM_BOT_TOKEN"] != "t=1" || env["MM_USER"] != "@alice" {
		t.Fatalf("readEnv = %v %v", env, err)
	}
}

func TestListRows(t *testing.T) {
	agents := []listedAgent{
		{agentInfo{Status: "working", Agent: "claude", Cwd: "/b"}, "w1:p3"},
		{agentInfo{Status: "idle", Agent: "codex", Cwd: "/a"}, "w1:p1"},
	}
	panes := map[string]*pane{"w1:p3": {Status: "idle"}, "w1:p2": {Status: "idle", Agent: "claude", Cwd: "/c"}}
	got := listRows(agents, panes)
	want := []row{
		{ID: "w1:p1", Agent: "codex", Cwd: "/a", Status: "idle"},
		{ID: "w1:p3", Agent: "claude", Cwd: "/b", Status: "working", Mirrored: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("listRows = %+v, want %+v", got, want)
	}
}

func TestPaneNames(t *testing.T) {
	panes := []herdrPane{
		{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1"},
		{PaneID: "w2:p1", TabID: "w2:t1", WorkspaceID: "w2"},
		{PaneID: "w2:p2", TabID: "w2:t2", WorkspaceID: "w2"},
		{PaneID: "w2:p3", TabID: "w2:t2", WorkspaceID: "w2", Label: "mybox"},
	}
	tabs := []herdrTab{{"w1:t1", "1"}, {"w2:t1", "1"}, {"w2:t2", "second"}}
	workspaces := []herdrWorkspace{{"w1", "solo", 1}, {"w2", "multi", 2}}
	got := paneNames(panes, tabs, workspaces)
	want := map[string]string{"w1:p1": "solo", "w2:p1": "multi / 1", "w2:p2": "multi / second", "w2:p3": "mybox"}
	if !maps.Equal(got, want) {
		t.Fatalf("paneNames = %v, want %v", got, want)
	}
}

func TestStatus(t *testing.T) {
	e := newTestEnv(t)
	var out strings.Builder
	if err := e.a.status(&out, nil, 0); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "daemon: not running") || !strings.Contains(s, "No agent panes") {
		t.Fatalf("status = %q", s)
	}
	os.WriteFile(filepath.Join(e.a.stateDir, "daemon.lock"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	os.WriteFile(filepath.Join(e.herdrDir, "agents.json"), []byte(`{"id":"cli:agent:list","result":{"type":"agent_list","agents":[
		{"agent":"claude","agent_status":"idle","cwd":"/src/app","pane_id":"w1:p2"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "panes.json"), []byte(`{"result":{"panes":[{"pane_id":"w1:p2","tab_id":"w1:t1","workspace_id":"w1","label":"web"}]}}`), 0o644)
	rows, err := e.a.rows()
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := e.a.status(&out, rows, 0); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"daemon: running (pid " + strconv.Itoa(os.Getpid()) + ")", "> ", "w1:p2", "🟢 idle", "claude", "/src/app"} {
		if !strings.Contains(s, want) {
			t.Fatalf("status = %q, want %q", s, want)
		}
	}
	lines := strings.Split(s, "\n")
	head, row := lines[2], lines[3]
	for col, cell := range map[string]string{"NAME": "web", "PANE": "w1:p2", "AGENT": "claude", "DIRECTORY": "/src/app", "STATUS": "🟢"} {
		if strings.Index(head, col) != strings.Index(row, cell) {
			t.Fatalf("%s is not above %s:\n%s\n%s", col, cell, head, row)
		}
	}

	// A toggle or event hook holds state.lock across Mattermost calls; the popup must not wait for it.
	lock, _ := os.OpenFile(filepath.Join(e.a.stateDir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	defer lock.Close()
	syscall.Flock(int(lock.Fd()), syscall.LOCK_EX)
	done := make(chan error, 1)
	go func() { _, err := e.a.rows(); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rows waited for state.lock")
	}
}

func TestStatusPopupShowsErrorUntilQ(t *testing.T) {
	e := newTestEnv(t)
	os.WriteFile(filepath.Join(e.a.stateDir, "panes.json"), []byte("{"), 0o600)
	cmd := exec.Command(os.Args[0], "status")
	cmd.Env = append(os.Environ(), "HERDR_MM_MAIN=1", "HERDR_PLUGIN_CONFIG_DIR="+filepath.Dir(e.a.envPath), "HERDR_PLUGIN_STATE_DIR="+e.a.stateDir, "HERDR_BIN_PATH="+e.a.herdrBin)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	var out []byte
	for !strings.Contains(string(out), "Press q or Esc to close.") {
		b := make([]byte, 256)
		n, err := stdout.Read(b)
		if err != nil {
			t.Fatalf("popup output ended early: %q %v", out, err)
		}
		out = append(out, b[:n]...)
	}
	if !strings.Contains(string(out), "panes.json") {
		t.Fatalf("popup = %q, want the panes.json error", out)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for _, key := range []string{"\x1b[A", "Q"} { // up arrow starts with Esc
		stdin.Write([]byte(key))
		select {
		case err := <-done:
			t.Fatalf("popup closed on %q: %v", key, err)
		case <-time.After(300 * time.Millisecond):
		}
	}
	stdin.Write([]byte("q"))
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("popup exit = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("q did not close the popup")
	}
}
