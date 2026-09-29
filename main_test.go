package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeMM is a Mattermost server that keeps posts in memory.
type fakeMM struct {
	mu    sync.Mutex
	posts []*post
}

func (f *fakeMM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, `{"message":"invalid token"}`, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
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
		p.ID = fmt.Sprintf("post%d", len(f.posts)+1)
		f.posts = append(f.posts, p)
		json.NewEncoder(w).Encode(p)
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
	// The real rule: every non-alphanumeric character of the cwd becomes '-'.
	transcript := filepath.Join(claude, "projects", "-work-my-proj-x", "sess-1.jsonl")
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

	e.appendTranscript(t,
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
	if len(posts) != 2 || !strings.Contains(posts[0].Message, "done") {
		t.Fatalf("want root edited to done plus one reply, got %+v", posts)
	}
	if posts[1].RootID != posts[0].ID || posts[1].Message != "Final answer.\n\nSecond block." {
		t.Fatalf("reply = %+v", posts[1])
	}

	e.setAgent(t, "blocked")
	screen := "❯ earlier conversation\n\n" + strings.Repeat("─", 40) + "\n Bash command\n\n   rm -rf build\n\n Do you want to proceed?\n ❯ 1. Yes\n   2. No\n\n"
	os.WriteFile(filepath.Join(e.herdrDir, "screen.txt"), []byte(screen), 0o644)
	e.event(t, "pane.agent_status_changed", "w1:p1")
	e.event(t, "pane.agent_status_changed", "w1:p1")
	posts = e.mm.snapshot()
	if len(posts) != 3 {
		t.Fatalf("want one dialog post, got %+v", posts)
	}
	d := posts[2]
	if d.RootID != posts[0].ID || !strings.HasPrefix(d.Message, "@alice") || !strings.Contains(d.Message, "Do you want to proceed?") || strings.Contains(d.Message, "earlier conversation") {
		t.Fatalf("dialog post = %q", d.Message)
	}

	if err := e.a.toggle("w1:p1"); err != nil { // switch off
		t.Fatal(err)
	}
	e.event(t, "pane.agent_status_changed", "w1:p1")
	posts = e.mm.snapshot()
	if len(posts) != 3 || !strings.Contains(posts[0].Message, "Mirroring stopped") {
		t.Fatalf("unshare should only mark the root post: %+v", posts)
	}
}

func TestPaneClosedStopsMirroring(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	e.event(t, "pane.closed", "w1:p1")
	if posts := e.mm.snapshot(); !strings.Contains(posts[0].Message, "Pane closed") {
		t.Fatalf("root = %q", posts[0].Message)
	}
	e.a.withState(func(panes map[string]*pane) error {
		if len(panes) != 0 {
			t.Errorf("closed pane still mirrored: %v", panes)
		}
		return nil
	})
}

func TestLastReplyAndTruncate(t *testing.T) {
	if text, uuid, err := lastReply(""); text != "" || uuid != "" || err != nil {
		t.Fatalf("no transcript: %q %q %v", text, uuid, err)
	}
	e := newTestEnv(t)
	e.appendTranscript(t, `{"type":"user","message":{"content":"a plain string"}}`, assistant("u1", "m1", "text", "hi", false), "not json")
	if text, uuid, err := lastReply(e.transcript); text != "hi" || uuid != "u1" || err != nil {
		t.Fatalf("got %q %q %v", text, uuid, err)
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
	e.a.handleEvent(posted(post{ID: "r1", ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "--fix the bug"}))
	if got := e.prompts(); got != "w1:p1|--fix the bug\n" {
		t.Fatalf("prompts = %q", got)
	}
	if posts := e.mm.snapshot(); len(posts) != 0 {
		t.Fatalf("a delivered prompt needs no answer: %+v", posts)
	}

	os.WriteFile(filepath.Join(e.herdrDir, "blocked"), nil, 0o644)
	e.a.handleEvent(posted(post{ID: "r2", ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "yes"}))
	posts := e.mm.snapshot()
	if len(posts) != 1 || posts[0].RootID != "root1" || !strings.Contains(posts[0].Message, "Approve or answer it on the machine") {
		t.Fatalf("blocked answer = %+v", posts)
	}

	e.a.handleEvent(posted(post{ID: "r3", ChannelID: "dm", UserID: "alice-id", Message: " List "}))
	posts = e.mm.snapshot()
	if len(posts) != 2 || posts[1].RootID != "" || !strings.Contains(posts[1].Message, "w1:p1") || !strings.Contains(posts[1].Message, "/_redirect/pl/root1") {
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
		{ChannelID: "dm", RootID: "other-thread", UserID: "alice-id", Message: "not mirrored"},
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
