package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

var retryDelay = 5 * time.Second // first wait before reconnecting to Mattermost

// post is the subset of a Mattermost post the bot reads and writes.
type post struct {
	ID        string         `json:"id,omitempty"`
	ChannelID string         `json:"channel_id,omitempty"`
	RootID    string         `json:"root_id,omitempty"`
	UserID    string         `json:"user_id,omitempty"`
	CreateAt  int64          `json:"create_at,omitempty"`
	DeleteAt  int64          `json:"delete_at,omitempty"`
	Message   string         `json:"message"`
	Type      string         `json:"type,omitempty"`
	Props     map[string]any `json:"props,omitempty"`
	FileIDs   []string       `json:"file_ids,omitempty"`
}

// apiError is a Mattermost REST call answered with an error status.
type apiError struct {
	status  int
	message string
}

func (e *apiError) Error() string { return fmt.Sprintf("%d %s", e.status, e.message) }

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
	req.Header.Set("Content-Type", "application/json")
	return a.send(req, out)
}

// uploadFile uploads a file into a channel and returns its id, for a post's file_ids.
func (a *app) uploadFile(channelID, name string, data []byte) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	w.WriteField("channel_id", channelID)
	fw, err := w.CreateFormFile("files", name)
	if err == nil {
		_, err = fw.Write(data)
	}
	if err = errors.Join(err, w.Close()); err != nil {
		return "", err
	}
	req, err := http.NewRequest(http.MethodPost, a.mmURL+"/api/v4/files", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	var r struct {
		FileInfos []struct{ ID string } `json:"file_infos"`
	}
	if err := a.send(req, &r); err != nil {
		return "", err
	}
	if len(r.FileInfos) != 1 {
		return "", fmt.Errorf("POST /files: %d file infos returned", len(r.FileInfos))
	}
	return r.FileInfos[0].ID, nil
}

// send makes a REST call as the bot and decodes the response into out (if not nil).
func (a *app) send(req *http.Request, out any) error {
	method, path := req.Method, strings.TrimPrefix(req.URL.RequestURI(), "/api/v4")
	req.Header.Set("Authorization", "Bearer "+a.token)
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
		return fmt.Errorf("%s %s: %w", method, path, &apiError{resp.StatusCode, e.Message})
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// connect checks the bot token, looks up MM_USER and opens the bot's DM with them. lastPost starts at
// the DM's newest post.
func (a *app) connect() error {
	var me, user struct{ ID, Username string }
	var dm struct {
		ID         string
		LastPostAt int64 `json:"last_post_at"`
	}
	if err := a.api(http.MethodGet, "/users/me", nil, &me); err != nil {
		return fmt.Errorf(a.t("err.login")+": %w", err)
	}
	if err := a.api(http.MethodGet, "/users/username/"+url.PathEscape(a.user), nil, &user); err != nil {
		return fmt.Errorf("MM_USER %q: %w", a.user, err)
	}
	if err := a.api(http.MethodPost, "/channels/direct", []string{me.ID, user.ID}, &dm); err != nil {
		return fmt.Errorf(a.t("err.dm")+": %w", a.user, err)
	}
	a.botID, a.botName, a.userID, a.dmID, a.lastPost = me.ID, me.Username, user.ID, dm.ID, dm.LastPostAt
	return nil
}

// channels lists the public and private channels the bot is in, across its teams, by name.
func (a *app) channels() ([]target, error) {
	var teams []struct {
		ID          string
		DisplayName string `json:"display_name"`
	}
	if err := a.api(http.MethodGet, "/users/me/teams", nil, &teams); err != nil {
		return nil, err
	}
	var out []target
	for _, t := range teams {
		var chans []struct {
			ID, Type    string
			DisplayName string `json:"display_name"`
		}
		if err := a.api(http.MethodGet, "/users/me/teams/"+url.PathEscape(t.ID)+"/channels", nil, &chans); err != nil {
			return nil, err
		}
		for _, c := range chans {
			if c.Type == "O" || c.Type == "P" { // not DMs or group messages
				name := c.DisplayName
				if len(teams) > 1 {
					name = t.DisplayName + " / " + name
				}
				out = append(out, target{c.ID, name})
			}
		}
	}
	slices.SortFunc(out, func(x, y target) int { return strings.Compare(x.Name, y.Name) })
	return out, nil
}

// connectRetry calls connect until Mattermost answers: herdr starts the daemon once, maybe before the
// network is up. An HTTP 4xx answer means the config is wrong, so it is returned.
func (a *app) connectRetry() error {
	for wait := retryDelay; ; wait = min(2*wait, 5*time.Minute) {
		err := a.connect()
		if ae := (*apiError)(nil); err == nil || errors.As(err, &ae) && ae.status < 500 {
			return err
		}
		log.Printf("%v; retrying in %s", err, wait)
		time.Sleep(wait)
	}
}

func (a *app) createPost(channelID, rootID, message string) (string, error) {
	var p post
	err := a.api(http.MethodPost, "/posts", post{ChannelID: channelID, RootID: rootID, Message: a.truncate(message, maxPost)}, &p)
	return p.ID, err
}

func (a *app) patchPost(id, message string) error {
	return a.api(http.MethodPut, "/posts/"+id+"/patch", map[string]string{"message": a.truncate(message, maxPost)}, nil)
}

// listen keeps a WebSocket open to Mattermost to hear DM and linked channel messages. Reconnects until the plugin is off.
func (a *app) listen() error {
	for {
		err := a.listenOnce()
		if errors.Is(err, errPluginOff) {
			return err
		}
		log.Printf("websocket: %v, reconnecting in %s", err, retryDelay)
		time.Sleep(retryDelay)
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
	if err := a.catchUp(); err != nil {
		return err
	}
	// ponytail: one post per connect, so a flapping network posts on every reconnect; rate limit if that bites.
	a.say("", a.connectedMessage(a.connected))
	a.connected = true

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
		if err := a.handleEvent(msg); err != nil { // in order: handlePost dedupes by create_at
			return err
		}
	}
}

// catchUp handles the posts sent in the DM and the linked channels while the WebSocket was down, oldest first.
func (a *app) catchUp() error {
	panes, err := a.readPanes()
	if err != nil {
		return err
	}
	roots := map[string]string{a.dmID: ""} // channel id -> root post of the pane linked to it, "" for the DM
	for _, p := range panes {
		if p.ChannelID != a.dmID {
			roots[p.ChannelID] = p.RootID
		}
	}
	var posts []post
	for id, root := range roots {
		got, err := a.postsSince(id, root)
		if ae := (*apiError)(nil); id != a.dmID && errors.As(err, &ae) && ae.status < 500 {
			log.Printf("catch-up skips channel %s: %v", id, err) // the bot left it, or it or the pane's thread was deleted
			continue
		}
		if err != nil {
			return err
		}
		posts = append(posts, got...)
	}
	slices.SortFunc(posts, func(x, y post) int { return cmp.Compare(x.CreateAt, y.CreateAt) })
	for _, p := range posts {
		if err := a.handlePost(p, true); err != nil {
			return err
		}
	}
	return nil
}

// postsSince fetches a channel's posts sent after the last one handled. In a linked channel, those sent
// before the pane's root post were ignored when sent, as the channel was not linked yet, so they are left out.
func (a *app) postsSince(channelID, rootID string) ([]post, error) {
	since := a.lastPost
	if rootID != "" {
		var root post
		if err := a.api(http.MethodGet, "/posts/"+rootID, nil, &root); err != nil {
			return nil, err
		}
		since = max(since, root.CreateAt)
	}
	var list struct{ Posts map[string]post }
	if err := a.api(http.MethodGet, fmt.Sprintf("/channels/%s/posts?since=%d", channelID, since), nil, &list); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Collect(maps.Values(list.Posts)), func(p post) bool { return p.CreateAt <= since }), nil
}

// connectedMessage announces a WebSocket connection: the daemon's first, or a reconnect after the
// connection dropped.
func (a *app) connectedMessage(reconnect bool) string {
	if reconnect {
		return a.t("reconnected")
	}
	return a.t("connected")
}

func (a *app) handleEvent(raw []byte) error {
	var ev struct {
		Event string
		Data  struct{ Post string } // the post is JSON encoded inside a string
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Event != "posted" {
		return nil
	}
	var p post
	if json.Unmarshal([]byte(ev.Data.Post), &p) != nil {
		return nil
	}
	return a.handlePost(p, false)
}

var errPluginOff = errors.New("herdr is not running or the plugin is disabled")

// handlePost obeys a post from MM_USER. In the bot's DM a thread reply prompts that thread's pane and a
// top-level "list" lists the mirrored panes. In a channel linked to a pane, a post that @mentions the bot,
// top-level or in any thread, prompts that pane and is answered in its thread; anyone else who mentions
// the bot there is told only MM_USER is obeyed. late marks a post sent while the WebSocket was down.
func (a *app) handlePost(p post, late bool) error {
	// Bot and webhook posts can carry a human's user id, so they are dropped: no reply loops, no remote
	// control by integrations. System posts, such as a channel header change, are dropped too: they carry
	// the id of the member who made the change, and their text can hold a mention of the bot.
	if p.UserID == a.botID || p.Type != "" || fmt.Sprint(p.Props["from_bot"]) == "true" || fmt.Sprint(p.Props["from_webhook"]) == "true" {
		return nil
	}
	if p.CreateAt <= a.lastPost || p.DeleteAt != 0 {
		return nil // already handled, or deleted before it was caught up
	}
	inDM, root, text := p.ChannelID == a.dmID, p.RootID, p.Message
	if inDM && p.UserID != a.userID {
		return nil
	}
	if !inDM {
		var mentioned bool
		if text, mentioned = a.stripMention(p.Message); !mentioned || !a.linked(p.ChannelID) {
			return nil
		}
		root = cmp.Or(p.RootID, p.ID)
	}
	a.lastPost = p.CreateAt
	if err := os.WriteFile(filepath.Join(a.stateDir, "last_post"), []byte(strconv.FormatInt(p.CreateAt, 10)), 0o600); err != nil {
		log.Printf("last_post: %v", err)
	}
	if p.UserID != a.userID {
		a.sayIn(p.ChannelID, root, fmt.Sprintf(a.t("channel.refused"), a.user))
		return nil
	}
	if !a.pluginOn() {
		a.sayIn(p.ChannelID, root, a.t("plugin.off"))
		return errPluginOff
	}
	if inDM && root == "" {
		a.say("", a.topLevel(text))
		return nil
	}

	if cmd, ok := captureCmd(text); ok && inDM { // the rest is typed like any reply, then the screen is posted once it settles
		defer func() { go a.capture(root, cmd) }()
		if cmd == "" {
			return nil
		}
		text = cmd
	}
	if text == "" {
		return nil // only the mention
	}
	var paneID string
	var prev recent
	a.withState(func(panes map[string]*pane) error {
		for _, id := range slices.SortedFunc(maps.Keys(panes), paneOrder) {
			if pp := panes[id]; paneID == "" && (inDM && pp.RootID == root || !inDM && pp.ChannelID == p.ChannelID) {
				paneID, prev = id, pp.Prompted
				// Recorded before typing so the turn's end cannot beat it; marks the turn for posting.
				pp.Prompted = append(pp.Prompted, text)
				pp.Prompted = pp.Prompted[max(0, len(pp.Prompted)-5):]
				if !inDM {
					// ponytail: the reply goes to the thread of the latest question, so a turn still
					// answering an earlier question in another thread is posted there; record one per prompt if that bites.
					pp.ReplyRoot = root
				}
			}
		}
		return nil
	})
	if paneID == "" {
		return nil // not a mirrored pane's thread, or mirroring was switched off
	}
	log.Printf("prompt %s: %q", paneID, text)
	if _, err := a.herdr("agent", "prompt", paneID, text); err != nil {
		a.withState(func(panes map[string]*pane) error {
			if pp := panes[paneID]; pp != nil && len(pp.Prompted) > 0 && pp.Prompted[len(pp.Prompted)-1] == text {
				pp.Prompted = prev
			}
			return nil
		})
		msg := a.t("prompt.failed") + err.Error()
		if he := (*herdrError)(nil); errors.As(err, &he) && he.Code == "agent_blocked" {
			msg = a.t("prompt.blocked")
		}
		a.sayIn(p.ChannelID, root, msg)
	} else if late {
		a.sayIn(p.ChannelID, root, a.t("prompt.late"))
	} else {
		a.sayIn(p.ChannelID, root, a.t("prompt.received"))
	}
	return nil
}

// linked reports whether a mirrored pane's thread is in the channel.
func (a *app) linked(channelID string) bool {
	panes, _ := a.readPanes()
	for _, p := range panes {
		if p.ChannelID == channelID {
			return true
		}
	}
	return false
}

// stripMention removes the @mentions of the bot from a message and reports whether it had one.
func (a *app) stripMention(msg string) (string, bool) {
	if a.botName == "" {
		return msg, false
	}
	// Mattermost usernames hold letters, digits, ".", "-" and "_"; a trailing "." ends the sentence.
	re := regexp.MustCompile(`(?i)(^|[^\w.@-])@` + regexp.QuoteMeta(a.botName) + `\.?([^\w.-]|$)`)
	out := msg
	for next := re.ReplaceAllString(out, "$1$2"); next != out; next = re.ReplaceAllString(out, "$1$2") {
		out = next
	}
	return strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(out), ",:")), out != msg
}

// topLevel answers a DM message outside any thread.
func (a *app) topLevel(msg string) string {
	if !strings.EqualFold(strings.TrimSpace(msg), "list") {
		return a.t("help")
	}
	var lines []string
	a.withState(func(panes map[string]*pane) error {
		labels, _, _, _ := a.labels(panes)
		for id, p := range panes {
			lines = append(lines, fmt.Sprintf("- %s **%s** · %s%s · `%s` · [thread](%s/_redirect/pl/%s)",
				emoji[p.Status], a.t(p.Status), bold(cmp.Or(labels[id], p.Name)), p.Agent, baseName(p.Cwd), a.mmURL, p.RootID))
		}
		return nil
	})
	if len(lines) == 0 {
		return a.t("list.none")
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// say posts into the DM.
func (a *app) say(rootID, msg string) { a.sayIn(a.dmID, rootID, msg) }

func (a *app) sayIn(channelID, rootID, msg string) {
	if _, err := a.createPost(channelID, rootID, msg); err != nil {
		log.Printf("post: %v", err)
	}
}
