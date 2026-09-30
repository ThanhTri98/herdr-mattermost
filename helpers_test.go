package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	now       int64  // create_at of the newest post
	failPatch bool   // answer post edits with 500
	failLogin int    // answer this many logins with 503
	pinged    func() // runs each time the WebSocket client answers a ping

	files []string // "channel_id|name|content" of each upload
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
	if r.URL.Path == "/api/v4/websocket" { // reads the authentication challenge, pings twice, then drops the connection
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.ReadMessage()
		pong, closed := make(chan struct{}), make(chan struct{})
		conn.SetPongHandler(func(string) error { pong <- struct{}{}; return nil })
		go func() { conn.ReadMessage(); close(closed) }()
		for range 2 {
			conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(time.Second))
			select {
			case <-pong:
			case <-closed:
				return
			}
			if f.pinged != nil {
				f.pinged()
			}
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
		fmt.Fprint(w, `{"id":"bot","username":"herdr"}`)
	case r.Method == "GET" && path == "/users/me/teams":
		fmt.Fprint(w, `[{"id":"t1","name":"team","display_name":"Team"}]`)
	case r.Method == "GET" && path == "/users/me/teams/t1/channels":
		fmt.Fprint(w, `[{"id":"dm","type":"D","name":"bot__alice","display_name":"alice"},{"id":"ch2","type":"P","name":"ops","display_name":"Ops"},
			{"id":"ch1","type":"O","name":"dev","display_name":"Dev"},{"id":"ts","type":"O","name":"town-square","display_name":"Town Square"},
			{"id":"ot","type":"O","name":"off-topic","display_name":"Off-Topic"}]`)
	case r.Method == "GET" && path == "/teams/t1/channels/name/town-square":
		fmt.Fprint(w, `{"id":"ts"}`)
	case r.Method == "GET" && strings.HasPrefix(path, "/users/username/") && fakeUsers[strings.TrimPrefix(path, "/users/username/")]:
		name := strings.TrimPrefix(path, "/users/username/")
		fmt.Fprintf(w, `{"id":"%s-id","username":%q}`, name, name)
	case r.Method == "GET" && strings.HasPrefix(path, "/users/") && strings.HasSuffix(path, "-id") && fakeUsers[strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "-id")]:
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "-id")
		fmt.Fprintf(w, `{"id":"%s-id","username":%q}`, name, name)
	case r.Method == "POST" && path == "/posts":
		p := &post{}
		json.NewDecoder(r.Body).Decode(p)
		f.now++
		p.ID, p.UserID, p.CreateAt = fmt.Sprintf("post%d", len(f.posts)+1), "bot", f.now
		f.posts = append(f.posts, p)
		json.NewEncoder(w).Encode(p)
	case r.Method == "POST" && path == "/files":
		file, h, err := r.FormFile("files")
		if err != nil {
			http.Error(w, `{"message":"no file"}`, http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(file)
		f.files = append(f.files, r.FormValue("channel_id")+"|"+h.Filename+"|"+string(b))
		fmt.Fprintf(w, `{"file_infos":[{"id":"file%d"}]}`, len(f.files))
	case r.Method == "GET" && strings.HasPrefix(path, "/channels/") && strings.HasSuffix(path, "/posts"):
		since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
		list := map[string]map[string]*post{"posts": {}}
		for _, p := range f.posts {
			if "/channels/"+p.ChannelID+"/posts" == path && p.CreateAt > since {
				list["posts"][p.ID] = p
			}
		}
		json.NewEncoder(w).Encode(list)
	case r.Method == "GET" && strings.HasPrefix(path, "/posts/"):
		for _, p := range f.posts {
			if "/posts/"+p.ID == path {
				json.NewEncoder(w).Encode(p)
				return
			}
		}
		http.NotFound(w, r)
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

// fakeUsers are the users the fake Mattermost knows, with ids "<username>-id".
var fakeUsers = map[string]bool{"alice": true, "bob": true, "carol": true}

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
"agent get") cat "$d/agent.json" 2>/dev/null || { echo '{"error":{"code":"agent_not_found","message":"no agent"},"id":"x"}' >&2; exit 1; } ;;
"agent read") cat "$d/screen.txt" ;;
"pane read") [ "$4 $5 $6 $7" = "--source visible --format ansi" ] && cat "$d/screen.ansi" ;;
"agent list") cat "$d/agents.json" 2>/dev/null || echo '{"result":{"agents":[]}}' ;;
"pane list"|"tab list"|"workspace list") cat "$d/$1s.json" 2>/dev/null || echo '{"result":{}}' ;;
"status server") [ -e "$d/stopped" ] && echo '{"running":false}' || echo '{"running":true}' ;;
"plugin list")
  [ -e "$d/disabled" ] && on=false || on=true
  [ "$3 $4 $5" = "--plugin herdr-mattermost --json" ] && echo "{\"result\":{\"plugins\":[{\"plugin_id\":\"$4\",\"enabled\":$on}]}}" || echo '{"result":{"plugins":[]}}' ;;
"agent send-keys") printf '%s|%s\n' "$3" "$4" >> "$d/keys.log" ;;
"agent prompt")
  printf '%s|%s\n' "$3" "$4" >> "$d/prompts.log"
  if [ -e "$d/fail" ]; then echo '{"error":{"code":"pane_gone","message":"pane is gone"},"id":"x"}' >&2; exit 1; fi
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
	os.WriteFile(filepath.Join(state, "lang"), []byte("en\n"), 0o600) // most tests check the English texts
	os.WriteFile(filepath.Join(state, "targets.json"), []byte(`{"w1:p1":{"ID":"ch1","Name":"Dev"}}`), 0o600)
	os.WriteFile(filepath.Join(state, "whitelists.json"), []byte(`{"ch1":[{"id":"alice-id","username":"alice"}]}`), 0o600)
	claude := filepath.Join(dir, "claude")
	// Not the pane's cwd: Claude may have been started in another directory.
	transcript := filepath.Join(claude, "projects", "-somewhere-else", "sess-1.jsonl")
	os.MkdirAll(filepath.Dir(transcript), 0o755)
	a := &app{mmURL: srv.URL, token: "tok", envPath: filepath.Join(dir, ".env"),
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

// threadTurns shares the pane over a transcript holding history and returns functions that send a thread
// reply and that check the posts, acknowledgements aside, an idle then working round makes.
func threadTurns(t *testing.T, history ...string) (e *testEnv, send func(string, int64), idle func(...string)) {
	e = newTestEnv(t)
	e.setAgent(t, "working")
	e.appendTranscript(t, history...)
	if err := e.a.toggle("w1:p1"); err != nil {
		t.Fatal(err)
	}
	root := e.mm.snapshot()[0].ID
	send = func(m string, at int64) {
		e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr " + m, CreateAt: at}))
	}
	idle = func(want ...string) {
		t.Helper()
		before := len(e.mm.snapshot())
		e.setAgent(t, "idle")
		e.event(t, "pane.agent_status_changed", "w1:p1")
		e.setAgent(t, "working")
		e.event(t, "pane.agent_status_changed", "w1:p1")
		var got []string
		for _, p := range e.mm.snapshot()[before:] {
			if !strings.HasPrefix(p.Message, "📥") {
				got = append(got, strings.TrimPrefix(p.Message, "@alice "))
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("posted %q, want %q", got, want)
		}
	}
	return e, send, idle
}

func posted(p post) []byte { return postedIn(p, "O") }

// postedIn is the WebSocket event of a post in a channel of the given type, such as "D" for a DM.
func postedIn(p post, channelType string) []byte {
	b, _ := json.Marshal(p)
	ev, _ := json.Marshal(map[string]any{"event": "posted", "data": map[string]string{"post": string(b), "channel_type": channelType}})
	return ev
}

func mirroredEnv(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.a.botID, e.a.botName = "bot", "herdr"
	e.a.withState(func(panes map[string]*pane) error {
		panes["w1:p1"] = &pane{RootID: "root1", ChannelID: "ch1", Channel: "Dev", Status: "idle", Agent: "claude", Cwd: "/work/proj"}
		return nil
	})
	return e
}

func (e *testEnv) prompts() string {
	b, _ := os.ReadFile(filepath.Join(e.herdrDir, "prompts.log"))
	return string(b)
}
