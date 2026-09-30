package main

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// herdrPane, herdrTab and herdrWorkspace are the parts of herdr's pane, tab and workspace lists that name a pane.
type herdrPane struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string
}

type herdrTab struct {
	TabID string `json:"tab_id"`
	Label string
}

type herdrWorkspace struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string
	TabCount    int `json:"tab_count"`
}

// labels names every agent pane herdr reports, every open pane linked to a channel and every mirrored
// pane, numbered so a pane has the same label in the popup and in every post; a mirrored pane herdr no
// longer lists keeps its last label. It also returns herdr's agents, plus a noagent entry for each open
// pane linked to a channel with no agent, and marks the panes to leave out of the popup.
func (a *app) labels(mirrored map[string]*pane) (map[string]string, map[string]bool, []listedAgent, error) {
	var r struct {
		Result struct {
			Agents     []listedAgent
			Panes      []herdrPane
			Tabs       []herdrTab
			Workspaces []herdrWorkspace
		}
	}
	for _, kind := range []string{"agent", "pane", "tab", "workspace"} {
		out, err := a.herdr(kind, "list")
		if err == nil {
			err = json.Unmarshal(out, &r)
		}
		if err != nil {
			return nil, nil, nil, err
		}
	}
	names, hidden := paneNames(r.Result.Panes, r.Result.Tabs, r.Result.Workspaces)
	set := map[string]string{}
	for _, ag := range r.Result.Agents {
		set[ag.PaneID] = names[ag.PaneID]
	}
	for id, p := range mirrored {
		set[id] = cmp.Or(names[id], p.Name)
	}
	// An open pane still linked to a channel is listed even with no agent, so the channel can be freed.
	targets, err := a.readTargets()
	if err != nil {
		return nil, nil, nil, err
	}
	agents := r.Result.Agents
	for id := range targets {
		if _, open := names[id]; open && !slices.ContainsFunc(agents, func(ag listedAgent) bool { return ag.PaneID == id }) {
			set[id] = names[id]
			agents = append(agents, listedAgent{agentInfo{Status: "noagent"}, id})
		}
	}
	return numbered(set), hidden, agents, nil
}

// label sets p.Name to the pane's label; a failed lookup keeps the last one.
func (a *app) label(id string, p *pane, mirrored map[string]*pane) {
	if labels, _, _, err := a.labels(mirrored); err == nil && labels[id] != "" {
		p.Name = labels[id]
	}
}

// paneNames names a pane by its own label when renamed, otherwise by its workspace's label, followed
// by the tab's label when the workspace has more than one tab. It marks hidden the panes of the
// workspaces firstmate opens for its workers, whose labels start with "└ ".
func paneNames(panes []herdrPane, tabs []herdrTab, workspaces []herdrWorkspace) (map[string]string, map[string]bool) {
	tabLabel := map[string]string{}
	for _, t := range tabs {
		tabLabel[t.TabID] = t.Label
	}
	ws := map[string]herdrWorkspace{}
	for _, w := range workspaces {
		ws[w.WorkspaceID] = w
	}
	names, hidden := map[string]string{}, map[string]bool{}
	for _, p := range panes {
		w := ws[p.WorkspaceID]
		hidden[p.PaneID] = strings.HasPrefix(w.Label, "└ ")
		switch {
		case p.Label != "":
			names[p.PaneID] = p.Label
		case w.TabCount > 1:
			names[p.PaneID] = w.Label + " / " + tabLabel[p.TabID]
		default:
			names[p.PaneID] = w.Label
		}
	}
	return names, hidden
}

// numbered maps pane ids to their names, with " #2", " #3" added to the second and later, in pane id
// order, of those that share a name.
func numbered(names map[string]string) map[string]string {
	seen, out := map[string]int{}, map[string]string{}
	for _, id := range slices.SortedFunc(maps.Keys(names), paneOrder) {
		n := names[id]
		if seen[n]++; n != "" && seen[n] > 1 {
			n += " #" + strconv.Itoa(seen[n])
		}
		out[id] = n
	}
	return out
}

// paneOrder orders pane ids like wV:p3 or w13:p2 by workspace, then pane. herdr counts each part up
// like a number with digits 1-9, then A-Z, then 0, so a shorter part comes first and 0 ranks after Z.
func paneOrder(x, y string) int {
	xw, xp, _ := strings.Cut(x, ":")
	yw, yp, _ := strings.Cut(y, ":")
	count := func(a, b string) int {
		a, b = strings.ReplaceAll(a, "0", "~"), strings.ReplaceAll(b, "0", "~")
		return cmp.Or(cmp.Compare(len(a), len(b)), cmp.Compare(a, b))
	}
	return cmp.Or(count(xw, yw), count(xp, yp))
}
