package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

func (a *app) event(name string, raw []byte) error {
	var ev struct {
		Event string
		Data  struct {
			PaneID         string `json:"pane_id"`
			PreviousPaneID string `json:"previous_pane_id"` // pane.moved
			Pane           struct {
				PaneID string `json:"pane_id"`
			}
		}
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return fmt.Errorf("HERDR_PLUGIN_EVENT_JSON: %w", err)
	}
	if name == "" {
		name = ev.Event
	}
	name = strings.ReplaceAll(name, ".", "_")
	id, oldID := ev.Data.PaneID, ev.Data.PaneID
	if name == "pane_moved" { // a move to another workspace gives the pane a new id
		id, oldID = ev.Data.Pane.PaneID, ev.Data.PreviousPaneID
	}
	return a.withState(func(panes map[string]*pane) error {
		if targets, err := a.readTargets(); err == nil && targets[oldID].ID != "" && (name == "pane_moved" || name == "pane_closed") {
			t := targets[oldID]
			delete(targets, oldID)
			if name == "pane_moved" {
				targets[id] = t
			}
			if err := a.writeTargets(targets); err != nil {
				return err
			}
		}
		p := panes[oldID]
		if p == nil {
			return nil // not mirrored: the hook fires for every pane
		}
		if err := a.requireMM(); err != nil {
			return err
		}
		switch name {
		case "pane_closed":
			a.label(id, p, panes)
			delete(panes, id)
			p.Status = "closed"
			return errors.Join(a.patchPost(p.RootID, a.rootMessage(p)), a.postStopped(p))
		case "pane_moved":
			delete(panes, oldID)
			panes[id] = p
		}
		return a.sync(id, panes)
	})
}

// sync brings a mirrored pane's thread up to date with the agent's live state. It asks herdr for the
// state instead of trusting the event, and dedupes what it posts, so hooks that run late or out of
// order neither lose nor repeat a reply.
func (a *app) sync(id string, panes map[string]*pane) error {
	p := panes[id]
	info, err := a.agent(id)
	if he := (*herdrError)(nil); errors.As(err, &he) && he.Code == "agent_not_found" {
		info, err = agentInfo{Status: "unknown"}, nil // the agent exited; the pane may get a new one
	}
	if err != nil {
		return err
	}
	prev, prevName := p.Status, p.Name
	p.Status = info.Status
	if info.Agent != "" {
		p.Agent, p.Cwd = info.Agent, info.Cwd
	}
	a.label(id, p, panes)
	var patchErr error
	if p.Status != prev || p.Name != prevName {
		patchErr = a.patchPost(p.RootID, a.rootMessage(p))
	}
	return errors.Join(patchErr, a.postNews(id, p, info))
}

// postNews posts the replies of the turns since the last one handled, or the dialog it is blocked on, into the pane's thread,
// once each. A reply is posted only when its turn was prompted from the thread, and a dialog only when
// the turn in effect is: one of the turn's prompts in the transcript is a recent text the daemon typed.
// Turns typed in the terminal stay off Mattermost.
func (a *app) postNews(id string, p *pane, info agentInfo) error {
	switch p.Status {
	case "idle", "done":
		p.LastDialog = ""
		// Claude can write a turn's final entry a few tens of ms after herdr says idle, so a turn typed
		// from the thread that has no reply yet, or ends in a tool step, is re-read for a short while
		// before giving up.
		path := a.transcript(info)
		rs, turn, cleared, open, err := replies(path)
		for deadline := time.Now().Add(transcriptSettle); err == nil && p.fromThread(turn) && !answered(rs, turn, open) && time.Now().Before(deadline); {
			time.Sleep(transcriptPoll)
			rs, turn, cleared, open, err = replies(path)
		}
		if err != nil {
			return err
		}
		// Every turn since the last one handled is posted, so a thread reply sent while the agent was
		// still answering the previous one doesn't hide that answer. All turns are considered when none
		// was handled yet, as when a pane is shared before its first prompt or a new agent starts in it,
		// or when /clear started the transcript. Otherwise, when the last turn handled is not in it, as
		// after resuming another session, only the newest is considered so old history stays out.
		// ponytail: resuming a session /clear started, or one resumed before a new agent's first turn,
		// considers all its turns; record the session with LastReply if old thread-prompted answers ever
		// get reposted.
		if len(rs) == 0 {
			p.LastReply = ""
			return nil
		}
		next := rs
		if i := slices.IndexFunc(rs, func(r reply) bool { return slices.Contains(r.uuids, p.LastReply) }); i >= 0 {
			next = rs[i:] // the turn handled gained text since, as when it is auto-continued after a usage limit
			if rs[i].uuid == p.LastReply {
				next = rs[i+1:]
			}
		} else if p.LastReply != "" && !cleared {
			next = rs[len(rs)-1:]
		}
		for _, r := range next {
			// ponytail: Prompted is kept after posting so a turn a background task resumes, which inherits
			// the thread prompt, is posted too; a terminal prompt identical to a recent thread reply is
			// posted as well. Clear it per turn if that ever bites. A resumed turn inherits the latest typed
			// prompt, not the one that started the task, so a task started in the terminal that finishes
			// after a thread reply is posted by mistake, and one started from the thread that finishes after
			// a terminal prompt is kept off. Record which turn started each background task if that bites.
			if p.fromThread(r.prompts) {
				text := "@" + a.user + " " + r.text // answers the asker, who can only be MM_USER
				if _, err := a.createPost(p.ChannelID, cmp.Or(p.ReplyRoot, p.RootID), text); err != nil {
					return err
				}
			}
			p.LastReply = r.uuid
		}
	case "blocked":
		_, _, _, turn, err := lastReply(a.transcript(info))
		if err != nil || !p.fromThread(turn) {
			return err
		}
		screen, err := a.herdr("agent", "read", id, "--source", "detection")
		if err != nil {
			return err
		}
		if d := dialog(string(screen)); d != p.LastDialog {
			msg := fmt.Sprintf(a.t("dialog")+"\n```\n%s\n```", a.user, p.Agent, a.truncate(d, maxPost-500))
			if _, err := a.createPost(p.ChannelID, cmp.Or(p.ReplyRoot, p.RootID), msg); err != nil {
				return err
			}
			p.LastDialog = d
		}
	default:
		p.LastDialog = ""
	}
	return nil
}

// transcriptSettle bounds how long an idle hook waits for the reply of a turn typed from the thread.
var transcriptSettle, transcriptPoll = 2 * time.Second, 100 * time.Millisecond

// answered reports whether the newest reply belongs to the turn in effect and no tool step follows it.
func answered(rs []reply, turn []string, open bool) bool {
	return !open && len(rs) > 0 && slices.Equal(rs[len(rs)-1].prompts, turn)
}

var emoji = map[string]string{"idle": "🟢", "done": "✅", "working": "⏳", "blocked": "✋", "unknown": "❔", "noagent": "➖", "off": "⚪", "closed": "⚫"}

func (a *app) rootMessage(p *pane) string {
	head := fmt.Sprintf("%s **%s** · %s%s · `%s`", emoji[p.Status], a.t(p.Status), bold(p.Name), p.Agent, baseName(p.Cwd))
	switch p.Status {
	case "off":
		return head + "\n" + a.t("root.off")
	case "closed":
		return head + "\n" + a.t("root.closed")
	}
	return head + "\n" + a.t("root.channel")
}

// postStopped posts the top-level notice that mirroring of a pane was switched off or its pane closed;
// the root post is only edited, which notifies nobody.
func (a *app) postStopped(p *pane) error {
	what := a.t("notice.off")
	if p.Status == "closed" {
		what = a.t("notice.closed")
	}
	msg := fmt.Sprintf("%s %s %s%s · `%s` · [thread](%s/_redirect/pl/%s)",
		emoji[p.Status], what, bold(p.Name), p.Agent, baseName(p.Cwd), a.mmURL, p.RootID)
	_, err := a.createPost(p.ChannelID, "", msg)
	return err
}

// bold formats a pane name to lead a " · " list, or returns "" for a pane with no name yet.
func bold(name string) string {
	if name == "" {
		return ""
	}
	return "**" + name + "** · "
}

func baseName(path string) string {
	if path == "" {
		return "?"
	}
	return filepath.Base(path)
}

const maxPost = 16383 // Mattermost's post length limit, in characters

func (a *app) truncate(s string, max int) string {
	note := a.t("truncated")
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	return string([]rune(s)[:max-utf8.RuneCountInString(note)]) + note
}

// dialog cuts a Claude screen snapshot down to the box under its last horizontal rule, where
// approval and question dialogs render.
func dialog(screen string) string {
	lines := strings.Split(strings.TrimRight(screen, " \n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		t := strings.TrimSpace(lines[i])
		rest := strings.Join(lines[i+1:], "\n")
		if t != "" && strings.Trim(t, "─") == "" && strings.TrimSpace(rest) != "" {
			return strings.Trim(rest, "\n")
		}
	}
	return strings.Join(lines, "\n")
}
