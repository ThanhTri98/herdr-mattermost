package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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
				e.a.handleEvent(posted(post{ChannelID: "ch1", RootID: root, UserID: "alice-id", Message: "@herdr " + m, CreateAt: int64(100 + i)}))
			}
			e.appendTranscript(t, append(c.lines, assistant("r1", "m9", "text", "All done.", false))...)
			e.setAgent(t, "idle")
			e.event(t, "pane.agent_status_changed", "w1:p1")
			if posts := e.mm.snapshot(); posts[len(posts)-1].Message != "@alice All done." || posts[len(posts)-1].RootID != root {
				t.Fatalf("the reply to a thread prompt must be posted: %+v", posts)
			}
		})
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
	if got := e.a.truncate(long, maxPost); len([]rune(got)) != maxPost || !strings.HasSuffix(got, "(truncated)") {
		t.Fatalf("truncate: %d runes", len([]rune(got)))
	}
}
