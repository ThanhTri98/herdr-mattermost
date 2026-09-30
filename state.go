package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"syscall"
)

// pane is what is remembered about a mirrored pane, keyed by pane id in panes.json.
type pane struct {
	RootID     string `json:"root_id"`
	ChannelID  string `json:"channel_id"`
	Status     string `json:"status"`
	Agent      string `json:"agent"`
	Cwd        string `json:"cwd"`
	Name       string `json:"name,omitempty"`        // the pane's label, kept by sync for the stop notice
	LastReply  string `json:"last_reply,omitempty"`  // uuid of the transcript entry last handled, posted or not
	LastDialog string `json:"last_dialog,omitempty"` // dialog last posted while blocked
	Prompted   recent `json:"prompted,omitempty"`    // posts last typed into the agent
	Channel    string `json:"channel,omitempty"`     // name of the channel the thread is in; "" was the DM, no longer supported
	ReplyRoot  string `json:"reply_root,omitempty"`  // the thread of the last question typed in
}

// recent is the last few posts typed into a pane, oldest first. panes.json written before
// it was a list holds a single string.
type recent []string

func (r *recent) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*r = recent{s}
		return nil
	}
	return json.Unmarshal(b, (*[]string)(r))
}

// withState runs fn on the mirrored panes under an exclusive file lock, then saves them. The lock
// serializes the event hooks herdr runs concurrently, the toggle action and the daemon.
func (a *app) withState(fn func(map[string]*pane) error) error {
	lock, err := os.OpenFile(filepath.Join(a.stateDir, "state.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	panes, err := a.readPanes()
	if err != nil {
		return err
	}
	fnErr := fn(panes) // saved even on error: fn only records what already happened
	return errors.Join(fnErr, writeFile(filepath.Join(a.stateDir, "panes.json"), panes))
}

// target is a channel a pane is linked to, by id, with its name and whether it is private for the popup.
// A pane with none is not mirrored.
type target struct {
	ID, Name string
	Private  bool `json:",omitempty"`
}

// readTargets loads targets.json, which maps pane ids to their channel. A channel holds one pane:
// when several are linked to it, the first in pane order keeps it and the others lose their channel.
// Writes go through writeTargets under state.lock.
func (a *app) readTargets() (map[string]target, error) {
	path := filepath.Join(a.stateDir, "targets.json")
	all := map[string]target{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &all); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	out, taken := map[string]target{}, map[string]bool{}
	for _, id := range slices.SortedFunc(maps.Keys(all), paneOrder) {
		if t := all[id]; t.ID != "" && !taken[t.ID] {
			out[id], taken[t.ID] = t, true
		}
	}
	return out, nil
}

func (a *app) writeTargets(t map[string]target) error {
	return writeFile(filepath.Join(a.stateDir, "targets.json"), t)
}

// freeChannels leaves out of chans the channels linked to a pane other than id.
func freeChannels(chans []target, targets map[string]target, id string) []target {
	return slices.DeleteFunc(slices.Clone(chans), func(c target) bool {
		for other, t := range targets {
			if other != id && t.ID == c.ID {
				return true
			}
		}
		return false
	})
}

// retarget links a pane to a channel, or unlinks it for a target with no id. A mirrored pane's thread
// is stopped where it was and, unless unlinked, a new one started in the new channel; with its agent
// gone it stays off.
func (a *app) retarget(id string, to target) error {
	var changed, mirrored bool
	err := a.withState(func(panes map[string]*pane) error {
		targets, err := a.readTargets()
		if err != nil {
			return err
		}
		if to.ID != "" && len(freeChannels([]target{to}, targets, id)) == 0 {
			return fmt.Errorf(a.t("target.taken"), to.Name)
		}
		changed, mirrored = targets[id].ID != to.ID, panes[id] != nil
		if to.ID == "" {
			delete(targets, id)
		} else {
			targets[id] = to
		}
		return a.writeTargets(targets)
	})
	if err != nil || !changed || !mirrored {
		return err
	}
	stop := a.toggle(id)
	if to.ID == "" {
		return stop
	}
	start := a.toggle(id)
	if he := (*herdrError)(nil); errors.As(start, &he) && he.Code == "agent_not_found" {
		start = nil
	}
	return errors.Join(stop, start)
}

// readPanes loads panes.json. withState replaces it by rename, so it is never read half written. A pane
// mirrored to the DM, before channels were the only place, is left out, so it reads as off.
func (a *app) readPanes() (map[string]*pane, error) {
	path := filepath.Join(a.stateDir, "panes.json")
	panes := map[string]*pane{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &panes); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	maps.DeleteFunc(panes, func(_ string, p *pane) bool { return p.Channel == "" })
	return panes, nil
}

func (a *app) toggle(id string) error {
	if id == "" {
		return errors.New("HERDR_PANE_ID is not set; run the action on a pane")
	}
	if err := a.requireMM(); err != nil {
		return err
	}
	return a.withState(func(panes map[string]*pane) error {
		if p := panes[id]; p != nil {
			a.label(id, p, panes)
			delete(panes, id)
			p.Status = "off"
			return errors.Join(a.patchPost(p.RootID, a.rootMessage(p)), a.postStopped(p))
		}
		targets, err := a.readTargets()
		if err != nil {
			return err
		}
		t := targets[id]
		if t.ID == "" {
			return errors.New(a.t("toggle.nochannel"))
		}
		if err := a.connect(); err != nil {
			return err
		}
		info, err := a.agent(id)
		if err != nil {
			return err
		}
		p := &pane{ChannelID: t.ID, Channel: t.Name, Status: info.Status, Agent: info.Agent, Cwd: info.Cwd}
		a.label(id, p, panes)
		_, p.LastReply, _, _, _ = lastReply(a.transcript(info)) // only turns after sharing are posted
		if p.RootID, err = a.createPost(p.ChannelID, "", a.rootMessage(p)); err != nil {
			return err
		}
		panes[id] = p
		return a.sync(id, panes)
	})
}
