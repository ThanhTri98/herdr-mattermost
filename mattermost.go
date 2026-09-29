package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// post is the subset of a Mattermost post the bot reads and writes.
type post struct {
	ID        string         `json:"id,omitempty"`
	ChannelID string         `json:"channel_id,omitempty"`
	RootID    string         `json:"root_id,omitempty"`
	UserID    string         `json:"user_id,omitempty"`
	Message   string         `json:"message"`
	Props     map[string]any `json:"props,omitempty"`
}

// api calls the Mattermost REST API as the bot and decodes the response into out (if not nil).
func (a *app) api(method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.mmURL+"/api/v4"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&e)
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// connect checks the bot token, looks up MM_USER and opens the bot's DM with them.
func (a *app) connect() error {
	var me, user, dm struct{ ID string }
	if err := a.api(http.MethodGet, "/users/me", nil, &me); err != nil {
		return fmt.Errorf("mattermost login failed, check MM_URL and MM_BOT_TOKEN: %w", err)
	}
	if err := a.api(http.MethodGet, "/users/username/"+url.PathEscape(a.user), nil, &user); err != nil {
		return fmt.Errorf("MM_USER %q: %w", a.user, err)
	}
	if err := a.api(http.MethodPost, "/channels/direct", []string{me.ID, user.ID}, &dm); err != nil {
		return fmt.Errorf("open DM with @%s: %w", a.user, err)
	}
	a.botID, a.userID, a.dmID = me.ID, user.ID, dm.ID
	return nil
}

func (a *app) createPost(channelID, rootID, message string) (string, error) {
	var p post
	err := a.api(http.MethodPost, "/posts", post{ChannelID: channelID, RootID: rootID, Message: truncate(message, maxPost)}, &p)
	return p.ID, err
}

func (a *app) patchPost(id, message string) error {
	return a.api(http.MethodPut, "/posts/"+id+"/patch", map[string]string{"message": truncate(message, maxPost)}, nil)
}

// listen keeps a WebSocket open to Mattermost to hear DM messages. Reconnects forever.
func (a *app) listen() {
	for {
		if err := a.listenOnce(); err != nil {
			log.Printf("websocket: %v, reconnecting in 5s", err)
		}
		time.Sleep(5 * time.Second)
	}
}

func (a *app) listenOnce() error {
	wsURL := "ws" + strings.TrimPrefix(a.mmURL, "http") + "/api/v4/websocket" // http->ws, https->wss
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + a.token}})
	if err != nil {
		return err
	}
	defer conn.Close()
	auth := map[string]any{"seq": 1, "action": "authentication_challenge", "data": map[string]string{"token": a.token}}
	if err := conn.WriteJSON(auth); err != nil {
		return err
	}
	log.Print("websocket connected")

	// Mattermost pings about every 60s; 2 minutes of silence means the connection is dead.
	const readWait = 2 * time.Minute
	conn.SetReadDeadline(time.Now().Add(readWait))
	conn.SetPingHandler(func(data string) error {
		conn.SetReadDeadline(time.Now().Add(readWait))
		return conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(10*time.Second))
	})
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		conn.SetReadDeadline(time.Now().Add(readWait))
		go a.handleEvent(msg)
	}
}

// handleEvent obeys a "posted" event from MM_USER in the bot's DM: a thread reply prompts that
// thread's pane, a top-level "list" lists the mirrored panes.
func (a *app) handleEvent(raw []byte) {
	var ev struct {
		Event string
		Data  struct{ Post string } // the post is JSON encoded inside a string
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Event != "posted" {
		return
	}
	var p post
	if json.Unmarshal([]byte(ev.Data.Post), &p) != nil {
		return
	}
	// Only MM_USER is obeyed, which also rules out the bot itself; bot and webhook posts can carry
	// a human's user id, so they are dropped too: no reply loops, no remote control by integrations.
	if p.UserID != a.userID || p.ChannelID != a.dmID || fmt.Sprint(p.Props["from_bot"]) == "true" || fmt.Sprint(p.Props["from_webhook"]) == "true" {
		return
	}
	if p.RootID == "" {
		a.say("", a.topLevel(p.Message))
		return
	}

	var paneID string
	a.withState(func(panes map[string]*pane) error {
		for id, pp := range panes {
			if pp.RootID == p.RootID {
				paneID = id
			}
		}
		return nil
	})
	if paneID == "" {
		return // not a mirrored pane's thread, or mirroring was switched off
	}
	log.Printf("prompt %s: %q", paneID, p.Message)
	if _, err := a.herdr("agent", "prompt", paneID, p.Message); err != nil {
		msg := "❌ Could not prompt the agent: " + err.Error()
		if he := (*herdrError)(nil); errors.As(err, &he) && he.Code == "agent_blocked" {
			msg = "✋ The agent is waiting on a dialog. Approve or answer it on the machine, then reply again."
		}
		a.say(p.RootID, msg)
	}
}

// topLevel answers a DM message outside any thread.
func (a *app) topLevel(msg string) string {
	if !strings.EqualFold(strings.TrimSpace(msg), "list") {
		return "Reply in a pane's thread to prompt its agent, or send `list` to see the mirrored panes."
	}
	var lines []string
	a.withState(func(panes map[string]*pane) error {
		for id, p := range panes {
			lines = append(lines, fmt.Sprintf("- %s **%s** · %s · `%s` · pane `%s` · [thread](%s/_redirect/pl/%s)",
				emoji[p.Status], p.Status, p.Agent, baseName(p.Cwd), id, a.mmURL, p.RootID))
		}
		return nil
	})
	if len(lines) == 0 {
		return "No panes are mirrored. Run the Mattermost toggle action on a herdr pane to mirror it."
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (a *app) say(rootID, msg string) {
	if _, err := a.createPost(a.dmID, rootID, msg); err != nil {
		log.Printf("post: %v", err)
	}
}
