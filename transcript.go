package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// transcript finds the agent's Claude session log, or "" when there is none.
func (a *app) transcript(info agentInfo) string {
	s := info.Session
	if info.Agent != "claude" || s == nil || s.Value == "" {
		return ""
	}
	if s.Kind == "path" {
		return s.Value
	}
	// Session ids are unique, and the pane's cwd need not be the directory Claude started in.
	if m, _ := filepath.Glob(filepath.Join(a.claudeDir, "projects", "*", s.Value+".jsonl")); len(m) > 0 {
		return m[0]
	}
	return ""
}

// fromThread reports whether one of a turn's prompts is a recent thread reply typed into the pane.
func (p *pane) fromThread(turn []string) bool {
	return slices.ContainsFunc(turn, func(s string) bool {
		return strings.TrimSpace(s) != "" && slices.ContainsFunc(p.Prompted, func(q string) bool { return sameText(s, q) })
	})
}

// sameText compares prompts ignoring whitespace differences typing may introduce.
func sameText(a, b string) bool {
	return strings.Join(strings.Fields(a), " ") == strings.Join(strings.Fields(b), " ")
}

// pasteMarker matches the tags Claude wraps pasted text in when it records a prompt.
var pasteMarker = regexp.MustCompile(`</?pasted_content id="[^"]*">`)

// block is a content block of a transcript message.
type block struct{ Type, Text string }

// tag returns the text inside the first <name>...</name> in s.
func tag(s, name string) string {
	_, v, _ := strings.Cut(s, "<"+name+">")
	v, _, _ = strings.Cut(v, "</"+name+">")
	return v
}

// lastReply returns the text of the newest main-thread assistant message in a Claude transcript, the
// uuid of its last entry, the prompts typed in its turn and those of the turn in effect at the end of
// the transcript.
func lastReply(path string) (text, uuid string, prompts, turn []string, err error) {
	rs, turn, _, _, err := replies(path)
	if len(rs) == 0 {
		return "", "", nil, turn, err
	}
	r := rs[len(rs)-1]
	return r.text, r.uuid, r.prompts, turn, err
}

// reply is the newest assistant text message of a turn, with the uuid of its last entry, those of every
// text entry in the turn and the prompts typed in it.
type reply struct {
	text, uuid     string
	uuids, prompts []string
}

// replies returns the reply of every main-thread turn in a Claude transcript, oldest first, the prompts
// of the turn in effect at its end, whether its first typed prompt is /clear, which starts a new
// transcript, and whether it ends in a tool call or result with no text after it. A turn starts at a
// typed prompt or a task notification, which keeps the prompts of the turn it resumes. Claude writes one
// entry per content block, so text is gathered by message id.
func replies(path string) (rs []reply, turn []string, cleared, open bool, err error) {
	if path == "" {
		return nil, nil, false, false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false, false, err
	}
	defer f.Close()
	var msgID, uuid string
	var texts, uuids, prompts []string
	endTurn := func() {
		if uuid != "" {
			rs = append(rs, reply{strings.Join(texts, "\n\n"), uuid, uuids, prompts})
		}
		msgID, uuid, texts, uuids, prompts = "", "", nil, nil, nil
	}
	r := bufio.NewReader(f) // not a Scanner: tool results make lines longer than its buffer
	for {
		line, readErr := r.ReadBytes('\n')
		var e struct {
			Type             string
			IsSidechain      bool
			IsMeta           bool
			IsCompactSummary bool
			Origin           struct{ Kind string }
			UUID             string
			Attachment       struct{ CommandMode, Prompt string }
			Message          struct {
				ID      string
				Content json.RawMessage
			}
		}
		// A typed prompt starts a turn as a user entry whose content is a string or blocks without a
		// tool_result (compaction summaries and task notifications are not typed); a prompt typed while
		// the agent works is queued into the running turn as an attachment.
		if bytes.Contains(line, []byte(`"user"`)) && json.Unmarshal(line, &e) == nil && e.Type == "user" && !e.IsSidechain && !e.IsMeta && e.Origin.Kind == "task-notification" {
			endTurn()
		}
		if bytes.Contains(line, []byte(`"user"`)) && json.Unmarshal(line, &e) == nil && e.Type == "user" && !e.IsSidechain && !e.IsMeta && !e.IsCompactSummary && e.Origin.Kind != "task-notification" {
			var s string
			var blocks []block
			json.Unmarshal(e.Message.Content, &blocks)
			open = slices.ContainsFunc(blocks, func(b block) bool { return b.Type == "tool_result" })
			if json.Unmarshal(e.Message.Content, &s) == nil {
				if name := tag(s, "command-name"); strings.HasPrefix(s, "<command-") && name != "" {
					s = name + " " + tag(s, "command-args") // a slash command is recorded as tags
				}
				cleared = cleared || turn == nil && strings.TrimSpace(s) == "/clear"
				endTurn()
				turn = []string{pasteMarker.ReplaceAllString(s, "")}
			} else if blocks != nil && !open {
				var t []string // a prompt with a pasted image is recorded as blocks
				for _, b := range blocks {
					if b.Type == "text" {
						t = append(t, b.Text)
					}
				}
				endTurn()
				turn = []string{strings.Join(t, "\n")}
			}
		}
		if bytes.Contains(line, []byte(`"queued_command"`)) && json.Unmarshal(line, &e) == nil && e.Attachment.CommandMode == "prompt" && !e.IsSidechain {
			turn = append(turn, e.Attachment.Prompt)
		}
		if bytes.Contains(line, []byte(`"assistant"`)) && json.Unmarshal(line, &e) == nil && e.Type == "assistant" && !e.IsSidechain {
			var content []block
			json.Unmarshal(e.Message.Content, &content)
			var t []string
			for _, c := range content {
				if c.Type == "tool_use" {
					open = true
				} else if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					t, open = append(t, c.Text), false
				}
			}
			if len(t) > 0 {
				if e.Message.ID != msgID {
					msgID, texts = e.Message.ID, nil
				}
				texts, uuid, uuids, prompts = append(texts, t...), e.UUID, append(uuids, e.UUID), turn
			}
		}
		if readErr == io.EOF {
			endTurn()
			return rs, turn, cleared, open, nil
		}
		if readErr != nil {
			return nil, nil, false, false, readErr
		}
	}
}
