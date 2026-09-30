package main

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPaneNames(t *testing.T) {
	panes := []herdrPane{
		{PaneID: "w1:p1", TabID: "w1:t1", WorkspaceID: "w1"},
		{PaneID: "w2:p1", TabID: "w2:t1", WorkspaceID: "w2"},
		{PaneID: "w2:p2", TabID: "w2:t2", WorkspaceID: "w2"},
		{PaneID: "w2:p3", TabID: "w2:t2", WorkspaceID: "w2", Label: "mybox"},
	}
	tabs := []herdrTab{{"w1:t1", "1"}, {"w2:t1", "1"}, {"w2:t2", "second"}}
	workspaces := []herdrWorkspace{{"w1", "solo", 1}, {"w2", "multi", 2}}
	panes = append(panes, herdrPane{PaneID: "w3:p1", TabID: "w3:t1", WorkspaceID: "w3"})
	workspaces = append(workspaces, herdrWorkspace{"w3", "└ worker", 1})
	got, hidden := paneNames(panes, tabs, workspaces)
	want := map[string]string{"w1:p1": "solo", "w2:p1": "multi / 1", "w2:p2": "multi / second", "w2:p3": "mybox", "w3:p1": "└ worker"}
	if !maps.Equal(got, want) {
		t.Fatalf("paneNames = %v, want %v", got, want)
	}
	if !maps.Equal(hidden, map[string]bool{"w1:p1": false, "w2:p1": false, "w2:p2": false, "w2:p3": false, "w3:p1": true}) {
		t.Fatalf("hidden = %v", hidden)
	}
}

func TestNumbered(t *testing.T) {
	got := numbered(map[string]string{"w1:p1": "a", "w1:p2": "b", "w2:p1": "a", "w3:p1": "", "w4:p1": "a", "w5:p1": ""})
	if want := map[string]string{"w1:p1": "a", "w1:p2": "b", "w2:p1": "a #2", "w3:p1": "", "w4:p1": "a #3", "w5:p1": ""}; !maps.Equal(got, want) {
		t.Fatalf("numbered = %q, want %q", got, want)
	}
	got = numbered(map[string]string{"w12:p1": "a", "w3:p1": "a", "w1:p10": "b", "w1:p2": "b"})
	if want := map[string]string{"w3:p1": "a", "w12:p1": "a #2", "w1:p2": "b", "w1:p10": "b #2"}; !maps.Equal(got, want) {
		t.Fatalf("numbered = %q, want %q", got, want)
	}
	// herdr's ids count 1-9, then A-Z, then 0: wA comes after w9, and w13 after wV.
	got = numbered(map[string]string{"wA:p1": "a", "w9:p1": "a", "w13:p1": "c", "wV:p1": "c", "w1:p10": "d", "w1:pA": "d", "w1:p9": "d"})
	if want := map[string]string{"w9:p1": "a", "wA:p1": "a #2", "wV:p1": "c", "w13:p1": "c #2", "w1:p9": "d", "w1:pA": "d #2", "w1:p10": "d #3"}; !maps.Equal(got, want) {
		t.Fatalf("numbered = %q, want %q", got, want)
	}
	// herdr gives w0 after wZ, then w11.
	got = numbered(map[string]string{"w0:p1": "e", "w11:p1": "e", "wZ:p1": "e", "w1:p1": "e"})
	if want := map[string]string{"w1:p1": "e", "wZ:p1": "e #2", "w0:p1": "e #3", "w11:p1": "e #4"}; !maps.Equal(got, want) {
		t.Fatalf("numbered = %q, want %q", got, want)
	}
}

// TestSameLabelEverywhere numbers panes over every agent pane herdr reports and every mirrored pane,
// so the popup, the list reply, the root post and the stop notice agree.
func TestSameLabelEverywhere(t *testing.T) {
	e := newTestEnv(t)
	e.setAgent(t, "idle")
	agents, panesJSON := filepath.Join(e.herdrDir, "agents.json"), filepath.Join(e.herdrDir, "panes.json")
	os.WriteFile(agents, []byte(`{"result":{"agents":[{"agent":"claude","agent_status":"idle","cwd":"/work/my_proj.x","pane_id":"w1:p1"},
		{"agent":"claude","agent_status":"idle","cwd":"/work/my_proj.x","pane_id":"w1:p2"}]}}`), 0o644)
	os.WriteFile(panesJSON, []byte(`{"result":{"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1"},
		{"pane_id":"w1:p2","tab_id":"w1:t1","workspace_id":"w1"}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.herdrDir, "workspaces.json"), []byte(`{"result":{"workspaces":[{"workspace_id":"w1","label":"api","tab_count":1}]}}`), 0o644)
	os.WriteFile(filepath.Join(e.a.stateDir, "targets.json"), []byte(`{"w1:p2":{"ID":"ch1","Name":"Dev"}}`), 0o600)

	if err := e.a.toggle("w1:p2"); err != nil { // w1:p1 is not mirrored, yet it is the first "api"
		t.Fatal(err)
	}
	if m := e.mm.snapshot()[0].Message; !strings.Contains(m, "· **api #2** · claude") {
		t.Fatalf("root = %q", m)
	}
	rows, err := e.a.rows()
	if err != nil || len(rows) != 2 || rows[0].Name != "api" || rows[1].ID != "w1:p2" || rows[1].Name != "api #2" {
		t.Fatalf("rows = %+v %v", rows, err)
	}
	if got := e.a.listPanes(); !strings.Contains(got, "· **api #2** · claude") {
		t.Fatalf("list = %q", got)
	}
	if err := e.a.toggle("w1:p2"); err != nil {
		t.Fatal(err)
	}
	if n := e.mm.snapshot()[1].Message; !strings.Contains(n, "for **api #2** · claude") {
		t.Fatalf("stop notice = %q", n)
	}

	if err := e.a.toggle("w1:p2"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(agents, []byte(`{"result":{"agents":[{"agent":"claude","agent_status":"idle","cwd":"/work/my_proj.x","pane_id":"w1:p2"}]}}`), 0o644)
	os.WriteFile(panesJSON, []byte(`{"result":{"panes":[{"pane_id":"w1:p2","tab_id":"w1:t1","workspace_id":"w1"}]}}`), 0o644)
	e.event(t, "pane.agent_status_changed", "w1:p2") // the root post drops the number at its next update
	if m := e.mm.snapshot()[2].Message; !strings.Contains(m, "· **api** · claude") {
		t.Fatalf("root after w1:p1 closed = %q", m)
	}
}
