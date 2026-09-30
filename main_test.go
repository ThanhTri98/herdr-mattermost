package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReplyPromptsAgent(t *testing.T) {
	e := mirroredEnv(t)
	e.a.handleEvent(posted(post{ID: "r1", ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr --fix the bug", CreateAt: 1}))
	if got := e.prompts(); got != "w1:p1|--fix the bug\n" {
		t.Fatalf("prompts = %q", got)
	}
	if posts := e.mm.snapshot(); len(posts) != 1 || posts[0].RootID != "root1" || posts[0].Message != "📥 Received - the agent is working on it." {
		t.Fatalf("a delivered prompt gets one acknowledgement: %+v", posts)
	}

	os.WriteFile(filepath.Join(e.herdrDir, "blocked"), nil, 0o644)
	e.a.handleEvent(posted(post{ID: "r2", ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr yes", CreateAt: 2}))
	posts := e.mm.snapshot()
	if len(posts) != 2 || posts[1].RootID != "root1" || !strings.Contains(posts[1].Message, "Approve or answer it on the machine") {
		t.Fatalf("blocked answer = %+v", posts)
	}

	e.a.withState(func(panes map[string]*pane) error {
		panes["w1:p1"].Name = "web"
		panes["w2:p1"] = &pane{RootID: "root2", ChannelID: "ch2", Channel: "Ops", Status: "idle", Agent: "claude", Name: "web"}
		panes["w3:p1"] = &pane{RootID: "root3", ChannelID: "ch3", Channel: "QA", Status: "idle", Agent: "claude"}
		return nil
	})
	os.WriteFile(filepath.Join(e.a.stateDir, "whitelists.json"), []byte(`{"ch1":[{"id":"alice-id","username":"alice"}],"ch2":[{"id":"alice-id","username":"alice"}]}`), 0o600)
	e.a.handleEvent(posted(post{ID: "r3", ChannelID: "ch1", UserID: "alice-id", Message: "@herdr  List ", CreateAt: 3}))
	posts = e.mm.snapshot()
	m := posts[len(posts)-1].Message
	if len(posts) != 3 || posts[2].RootID != "r3" || strings.Contains(m, "w1:p1") || strings.Contains(m, "pane `") ||
		m != "- 🟢 **idle** · **web** · claude · `proj` · [thread]("+e.a.mmURL+"/_redirect/pl/root1)" {
		t.Fatalf("list answer in ch1 = %+v", posts)
	}
	e.a.handleEvent(posted(post{ID: "r4", ChannelID: "ch2", UserID: "alice-id", Message: "@herdr list", CreateAt: 4}))
	if m = e.mm.snapshot()[3].Message; m != "- 🟢 **idle** · **web #2** · claude · `?` · [thread]("+e.a.mmURL+"/_redirect/pl/root2)" {
		t.Fatalf("list answer in ch2 = %q", m)
	}
}

func TestIgnoresEveryoneButMMUser(t *testing.T) {
	e := mirroredEnv(t)
	for _, p := range []post{
		{ChannelID: "ch1", RootID: "root1", UserID: "bot", Message: "@herdr echo"},
		{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr hook", Props: map[string]any{"from_webhook": "true"}},
		{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr bot", Props: map[string]any{"from_bot": "true"}},
		{ChannelID: "town-square", RootID: "root1", UserID: "alice-id", Message: "@herdr wrong channel"},
		{ChannelID: "ch1", RootID: "other-thread", UserID: "alice-id", Message: "no mention", CreateAt: 1},
		{ChannelID: "ch2", UserID: "bob-id", Message: "@herdr list", CreateAt: 2},
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
	e.mm.add(post{ChannelID: "ch1", Message: "the pane's root post"})
	e.a.withState(func(panes map[string]*pane) error { panes["w1:p1"].RootID = "post1"; return nil })
	e.mm.add(post{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr handled before"})
	e.a.lastPost = 2
	e.mm.add(post{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr missed 1"})
	e.mm.add(post{ChannelID: "ch1", RootID: "root1", UserID: "bob-id", Message: "@herdr not alice"})
	e.mm.add(post{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr ", DeleteAt: 9})
	e.mm.add(post{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr missed 2"})
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

	missed2 := e.mm.snapshot()[5]
	e.a.handleEvent(posted(missed2)) // the WebSocket delivering it too must not prompt twice
	if err := e.a.catchUp(); err != nil {
		t.Fatal(err)
	}
	e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr live", CreateAt: 100}))
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
		if p.ChannelID != "ts" || p.RootID != "" {
			t.Fatalf("a connect notice goes top-level into Town Square only: %+v", p)
		}
		got = append(got, p.Message)
	}
	if want := []string{"🔌 Connected: the herdr-mm daemon started.", "🔌 Reconnected after the connection dropped."}; !slices.Equal(got, want) {
		t.Fatalf("Town Square posts = %q, want %q", got, want)
	}
}

func TestPluginOffStopsDaemon(t *testing.T) {
	for _, flag := range []string{"disabled", "stopped"} {
		e := mirroredEnv(t)
		os.WriteFile(filepath.Join(e.herdrDir, flag), nil, 0o644)
		err := e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: "root1", UserID: "alice-id", Message: "@herdr hi", CreateAt: 1}))
		if !errors.Is(err, errPluginOff) {
			t.Fatalf("%s: handleEvent = %v", flag, err)
		}
		if got := e.prompts(); got != "" {
			t.Fatalf("%s: prompted %q", flag, got)
		}
		if posts := e.mm.snapshot(); len(posts) != 1 || posts[0].RootID != "root1" || !strings.HasPrefix(posts[0].Message, "@all ⚪ herdr is not running") {
			t.Fatalf("%s: answer = %+v", flag, posts)
		}
	}
}

func TestConnectRetriesUntilMattermostAnswers(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = time.Millisecond
	e := newTestEnv(t)
	e.mm.failLogin = 2
	if err := e.a.connectRetry(); err != nil || e.a.botName != "herdr" || e.mm.failLogin != 0 {
		t.Fatalf("connectRetry = %v, bot %q, logins left to fail %d", err, e.a.botName, e.mm.failLogin)
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
	e.a.token = ""
	if err := e.a.requireMM(); err == nil || !strings.Contains(err.Error(), "missing MM_BOT_TOKEN: set it with s in the Mattermost status popup, or in ") {
		t.Fatalf("requireMM = %v", err)
	}
	e.a.token = "wrong"
	if err := e.a.connect(); err == nil || !strings.Contains(err.Error(), "mattermost login failed") {
		t.Fatalf("connect = %v", err)
	}
	os.WriteFile(e.a.envPath, []byte("# comment\nexport MM_URL=\"https://mm.example/\"\nMM_BOT_TOKEN='t=1'\n\nMM_USER=@alice\n"), 0o600)
	env, err := readEnv(e.a.envPath)
	if err != nil || env["MM_URL"] != "https://mm.example/" || env["MM_BOT_TOKEN"] != "t=1" || env["MM_USER"] != "@alice" {
		t.Fatalf("readEnv = %v %v", env, err)
	}
}

func TestCatalogComplete(t *testing.T) {
	for key := range catalog["en"] {
		if catalog["vi"][key] == "" {
			t.Errorf("vi lacks %q", key)
		}
	}
	for key := range catalog["vi"] {
		if catalog["en"][key] == "" {
			t.Errorf("en lacks %q", key)
		}
	}
	for status := range emoji {
		if catalog["en"][status] == "" {
			t.Errorf("no text for status %q", status)
		}
	}
	verbs := regexp.MustCompile(`%[a-z]`)
	for key, en := range catalog["en"] {
		if vi := catalog["vi"][key]; !slices.Equal(verbs.FindAllString(vi, -1), verbs.FindAllString(en, -1)) {
			t.Errorf("%q takes different arguments in vi %q and en %q", key, vi, en)
		}
	}
}

func TestLanguageSwitch(t *testing.T) {
	e := newTestEnv(t)
	os.Remove(filepath.Join(e.a.stateDir, "lang"))
	e.setAgent(t, "blocked")
	if err := e.a.toggle("w1:p1"); err != nil { // Vietnamese by default
		t.Fatal(err)
	}
	if m := e.mm.snapshot()[0].Message; m != "✋ **đang chờ bạn** · claude · `my_proj.x`\n_Nhắc (@mention) bot trong kênh này để gửi lệnh cho agent._" {
		t.Fatalf("vi root = %q", m)
	}
	if err := e.a.switchLang(); err != nil {
		t.Fatal(err)
	}
	e.setAgent(t, "idle")
	e.event(t, "pane.agent_status_changed", "w1:p1") // the next status update switches the root post
	if m := e.mm.snapshot()[0].Message; m != "🟢 **idle** · claude · `my_proj.x`\n_@mention the bot in this channel to prompt the agent._" {
		t.Fatalf("en root = %q", m)
	}
	e.a.switchLang()
	if got := e.a.listPanes("ch1"); !strings.HasPrefix(got, "- 🟢 **rảnh** · claude") {
		t.Fatalf("vi list = %q", got)
	}
	if got := e.a.truncate(strings.Repeat("x", 100), 50); !strings.HasSuffix(got, "\n… (đã cắt bớt)") {
		t.Fatalf("vi truncate = %q", got)
	}
	e.a.token = ""
	if got := e.a.popupToggle(row{ID: "w1:p1", Name: "web"}); got != "Không bật/tắt được web: thiếu MM_BOT_TOKEN: nhập bằng phím s trong popup trạng thái Mattermost, hoặc trong "+e.a.envPath {
		t.Fatalf("vi toggle failure = %q", got)
	}
	e.a.token = "wrong"
	if err := e.a.connect(); err == nil || !strings.HasPrefix(err.Error(), "đăng nhập Mattermost thất bại") {
		t.Fatalf("vi connect = %v", err)
	}
}
