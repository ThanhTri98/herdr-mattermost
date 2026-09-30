package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

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

func TestStatus(t *testing.T) {
	e := newTestEnv(t)
	var out strings.Builder
	if err := e.a.status(&out, nil, 0, 80); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "daemon: not running") || !strings.Contains(s, "No agent panes") {
		t.Fatalf("status = %q", s)
	}
	os.WriteFile(filepath.Join(e.a.stateDir, "daemon.lock"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	os.WriteFile(filepath.Join(e.herdrDir, "agents.json"), []byte(`{"id":"cli:agent:list","result":{"type":"agent_list","agents":[
		{"agent":"claude","agent_status":"idle","cwd":"/src/app","pane_id":"w1:p2"},
		{"agent":"codex","agent_status":"working","cwd":"/src/b","pane_id":"w1:p1"},
		{"agent":"claude","agent_status":"idle","cwd":"/src/c","pane_id":"w2:p1"},
		{"agent":"claude","agent_status":"idle","cwd":"/src/d","pane_id":"w3:p1"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "panes.json"), []byte(`{"result":{"panes":[{"pane_id":"w1:p2","tab_id":"w1:t1","workspace_id":"w1","label":"web"},
		{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1","label":"zed"},
		{"pane_id":"w2:p1","tab_id":"w2:t1","workspace_id":"w2","label":"web"},
		{"pane_id":"w3:p1","tab_id":"w3:t1","workspace_id":"w3"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "workspaces.json"), []byte(`{"result":{"workspaces":[{"workspace_id":"w1","label":"a","tab_count":1},
		{"workspace_id":"w2","label":"b","tab_count":1},{"workspace_id":"w3","label":"└ worker","tab_count":1}]}}`), 0o644)
	rows, err := e.a.rows()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rows {
		names = append(names, r.Name+"="+r.ID)
	}
	if want := []string{"web=w1:p2", "web #2=w2:p1", "zed=w1:p1"}; !slices.Equal(names, want) {
		t.Fatalf("rows = %q, want %q", names, want)
	}
	out.Reset()
	if err := e.a.status(&out, rows, 0, 80); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"daemon: running (pid " + strconv.Itoa(os.Getpid()) + ")", "> ", "🟢 idle", "claude", "CONNECTION", "  Not connected  "} {
		if !strings.Contains(s, want) {
			t.Fatalf("status = %q, want %q", s, want)
		}
	}
	if strings.Contains(s, "w1:p2") || strings.Contains(s, "/src/app") || strings.Contains(s, "DIRECTORY") || strings.Contains(s, "PANE") {
		t.Fatalf("status shows an id or directory: %q", s)
	}
	lines := strings.Split(s, "\n")
	head, row := lines[2], lines[3]
	for col, cell := range map[string]string{"NAME": "web", "AGENT": "claude", "STATUS": "🟢"} {
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
	for !strings.Contains(string(out), "q or Esc: close") {
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

func TestCellsCutWrap(t *testing.T) {
	for s, want := range map[string]int{"abc": 3, "🔒 dev": 6, "Tiếng Việt": 10, "Tie\u0302\u0301ng": 5, "✅ xong": 7} {
		if got := cells(s); got != want {
			t.Errorf("cells(%q) = %d, want %d", s, got, want)
		}
	}
	for _, c := range []struct {
		s    string
		n    int
		want string
	}{
		{"🔒 herdr-mattermost", 10, "🔒 herdr-…"},
		{"🔒 herdr", 2, "…"}, // the lock does not fit next to …
		{"Trạng thái", 6, "Trạng…"},
		{"short", 5, "short"},
		{"x", 0, ""},
	} {
		if got := cut(c.s, c.n); got != c.want || cells(got) > c.n {
			t.Errorf("cut(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
	}
	got := wrap("tri_165139, an.nguyen, binh.tran, 🔒 herdr-mattermost-plugins-rat-dai", 16)
	want := []string{"tri_165139,", "an.nguyen,", "binh.tran, 🔒", "herdr-mattermost", "-plugins-rat-dai"}
	if !slices.Equal(got, want) {
		t.Fatalf("wrap = %q, want %q", got, want)
	}
}

func TestStatusFitsWidth(t *testing.T) {
	e := newTestEnv(t)
	os.WriteFile(filepath.Join(e.a.stateDir, "whitelists.json"), []byte(`{"ch1":[{"ID":"1","Username":"tri_165139"},{"ID":"2","Username":"an.nguyen"},{"ID":"3","Username":"binh.tran"}]}`), 0o600)
	e.a.switchLang() // Vietnamese, with its accented header
	rows := []row{
		{Name: "api", Agent: "claude", Status: "done", Mirrored: true, Target: target{ID: "ch1", Name: "herdr-mattermost-plugins-rat-dai", Private: true}},
		{Name: "web", Agent: "claude", Status: "working"},
	}
	for _, width := range []int{60, 72, 200} {
		var out strings.Builder
		if err := e.a.status(&out, rows, 0, width); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(out.String(), "\n")
		for _, l := range lines {
			if cells(l) > width {
				t.Errorf("width %d: %q is %d cells", width, l, cells(l))
			}
		}
		head, api := lines[2], lines[3]
		if !strings.HasPrefix(head, "   TÊN") || !strings.Contains(api, "tri_165139 +2") || !strings.Contains(out.String(), "Kênh:      🔒 herdr-mattermost-plugins-rat-dai") || !strings.Contains(out.String(), "Whitelist: tri_165139, an.nguyen, binh.tran") {
			t.Fatalf("width %d:\n%s", width, out.String())
		}
		if strings.Contains(api, "…") != (width < 100) {
			t.Fatalf("width %d cut = %v:\n%s", width, strings.Contains(api, "…"), out.String())
		}
	}
	var out strings.Builder
	e.a.status(&out, rows, 1, 80)
	if strings.Contains(out.String(), "Kênh:") {
		t.Fatalf("detail block for a row with no channel:\n%s", out.String())
	}
}
