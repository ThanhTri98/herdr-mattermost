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

// connect checks the bot token and looks up the bot's own user.
func (a *app) connect() error {
	var me struct{ ID, Username string }
	if err := a.api(http.MethodGet, "/users/me", nil, &me); err != nil {
		return fmt.Errorf(a.t("err.login")+": %w", err)
	}
	a.botID, a.botName = me.ID, me.Username
	return nil
}

// team is a team the bot is in.
type team struct {
	ID, Name    string
	DisplayName string `json:"display_name"`
}

func (a *app) teams() ([]team, error) {
	var teams []team
	err := a.api(http.MethodGet, "/users/me/teams", nil, &teams)
	slices.SortFunc(teams, func(x, y team) int { return strings.Compare(x.Name, y.Name) })
	return teams, err
}

// defaultChannels are the channels Mattermost makes in every team, by their stable name.
var defaultChannels = map[string]bool{"town-square": true, "off-topic": true}

// channels lists the public and private channels the bot is in, across its teams, by name, leaving out
// the team default channels.
func (a *app) channels() ([]target, error) {
	teams, err := a.teams()
	if err != nil {
		return nil, err
	}
	var out []target
	for _, t := range teams {
		var chans []struct {
			ID, Type, Name string
			DisplayName    string `json:"display_name"`
		}
		if err := a.api(http.MethodGet, "/users/me/teams/"+url.PathEscape(t.ID)+"/channels", nil, &chans); err != nil {
			return nil, err
		}
		for _, c := range chans {
			if (c.Type == "O" || c.Type == "P") && !defaultChannels[c.Name] { // not DMs or group messages
				name := c.DisplayName
				if len(teams) > 1 {
					name = t.DisplayName + " / " + name
				}
				out = append(out, target{c.ID, name, c.Type == "P"})
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

// listen keeps a WebSocket open to Mattermost to hear channel messages and DMs. Reconnects until the plugin is off.
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
	if a.connected {
		a.notice(a.reconnectedNotice(a.downAt, now()))
	} else {
		a.notice(a.startedNotice())
	}
	a.connected = true
	defer func() { a.downAt = now() }()

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

// catchUp handles the posts sent in the linked channels while the WebSocket was down, oldest first.
func (a *app) catchUp() error {
	panes, err := a.readPanes()
	if err != nil {
		return err
	}
	var posts []post
	for _, p := range panes {
		got, err := a.postsSince(p.ChannelID, p.RootID)
		if ae := (*apiError)(nil); errors.As(err, &ae) && ae.status < 500 {
			log.Printf("catch-up skips channel %s: %v", p.ChannelID, err) // the bot left it, or it or the pane's thread was deleted
			continue
		}
		if err != nil {
			return err
		}
		posts = append(posts, got...)
	}
	slices.SortFunc(posts, func(x, y post) int { return cmp.Compare(x.CreateAt, y.CreateAt) })
	for _, p := range posts {
		if err := a.handlePost(p, false, true); err != nil {
			return err
		}
	}
	return nil
}

// postsSince fetches a linked channel's posts sent after the last one handled. Those sent before the
// pane's root post were ignored when sent, as the channel was not linked yet, so they are left out.
func (a *app) postsSince(channelID, rootID string) ([]post, error) {
	var root post
	if err := a.api(http.MethodGet, "/posts/"+rootID, nil, &root); err != nil {
		return nil, err
	}
	since := max(a.lastPost, root.CreateAt)
	var list struct{ Posts map[string]post }
	if err := a.api(http.MethodGet, fmt.Sprintf("/channels/%s/posts?since=%d", channelID, since), nil, &list); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Collect(maps.Values(list.Posts)), func(p post) bool { return p.CreateAt <= since }), nil
}

var now = time.Now // replaced by tests

// noticePost builds a daemon notice: a colored attachment with a bold title and short fields, whose
// fallback of title and hostname stands in for clients and notifications that do not render attachments.
func (a *app) noticePost(color, titleKey string, fields ...string) post {
	host, err := os.Hostname()
	if err != nil {
		host = "?"
	}
	title := a.t(titleKey)
	fs := []map[string]any{{"title": a.t("notice.host"), "value": "`" + host + "`", "short": true}}
	for i := 0; i+1 < len(fields); i += 2 {
		fs = append(fs, map[string]any{"title": a.t(fields[i]), "value": fields[i+1], "short": true})
	}
	return post{Props: map[string]any{"attachments": []map[string]any{{"color": color, "title": title, "fallback": title + " · " + host, "fields": fs}}}}
}

func (a *app) startedNotice() post {
	panes, _ := a.readPanes()
	return a.noticePost("#2eb67d", "notice.started", "notice.panes", fmt.Sprintf(a.t("notice.panecount"), len(panes)))
}

func (a *app) reconnectedNotice(down, up time.Time) post {
	const clock = "15:04:05 MST"
	d := up.Sub(down).Round(time.Second)
	return a.noticePost("#ecb22e", "notice.reconnected", "notice.down", fmt.Sprintf("%s → %s (%s)", down.Format(clock), up.Format(clock), d))
}

func (a *app) handleEvent(raw []byte) error {
	var ev struct {
		Event string
		Data  struct {
			Post        string // the post is JSON encoded inside a string
			ChannelType string `json:"channel_type"`
		}
	}
	if json.Unmarshal(raw, &ev) != nil || ev.Event != "posted" {
		return nil
	}
	var p post
	if json.Unmarshal([]byte(ev.Data.Post), &p) != nil {
		return nil
	}
	return a.handlePost(p, ev.Data.ChannelType == "D", false)
}

var errPluginOff = errors.New("herdr is not running or the plugin is disabled")

// handlePost obeys a post that @mentions the bot in a channel linked to a pane from a user on that
// channel's whitelist. The post, top-level or in any thread, prompts that pane and is answered in its
// thread; anyone else who mentions the bot there is told they are not allowed, and an empty whitelist
// obeys nobody. There, the mention followed by "help" gets the command list and followed by "list" the
// panes mirrored into that channel. A direct message, dm, gets one line saying the bot works only in
// channels. late marks a post sent while the WebSocket was down.
func (a *app) handlePost(p post, dm, late bool) error {
	// Bot and webhook posts can carry a human's user id, so they are dropped: no reply loops, no remote
	// control by integrations. System posts, such as a channel header change, are dropped too: they carry
	// the id of the member who made the change, and their text can hold a mention of the bot.
	if p.UserID == a.botID || p.Type != "" || fmt.Sprint(p.Props["from_bot"]) == "true" || fmt.Sprint(p.Props["from_webhook"]) == "true" {
		return nil
	}
	if p.CreateAt <= a.lastPost || p.DeleteAt != 0 {
		return nil // already handled, or deleted before it was caught up
	}
	text, mentioned := a.stripMention(p.Message)
	if !dm && (!mentioned || !a.linked(p.ChannelID)) {
		return nil
	}
	root := cmp.Or(p.RootID, p.ID)
	a.lastPost = p.CreateAt
	if err := os.WriteFile(filepath.Join(a.stateDir, "last_post"), []byte(strconv.FormatInt(p.CreateAt, 10)), 0o600); err != nil {
		log.Printf("last_post: %v", err)
	}
	if dm {
		a.sayIn(p.ChannelID, p.RootID, a.t("dm.refused"))
		return nil
	}
	list := a.whitelist(p.ChannelID)
	i := slices.IndexFunc(list, func(m member) bool { return m.ID == p.UserID })
	if i < 0 {
		a.sayIn(p.ChannelID, root, a.refusal(p.UserID, len(list) == 0))
		return nil
	}
	asker := list[i].Username
	if !a.pluginOn() {
		a.sayIn(p.ChannelID, root, a.t("plugin.off"))
		return errPluginOff
	}
	if strings.EqualFold(text, "help") {
		a.sayIn(p.ChannelID, root, a.t("help"))
		return nil
	}
	if strings.EqualFold(text, "list") {
		a.sayIn(p.ChannelID, root, a.listPanes(p.ChannelID))
		return nil
	}

	if cmd, ok := captureCmd(text); ok { // the rest is typed like any reply, then the screen is posted once it settles
		defer func() { go a.capture(p.ChannelID, root, cmd) }()
		if cmd == "" {
			return nil
		}
		text = cmd
	} else if cmd, ok := execCmd(text); ok {
		if cmd == "" {
			a.sayIn(p.ChannelID, root, a.t("exec.usage"))
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
			if pp := panes[id]; paneID == "" && pp.ChannelID == p.ChannelID {
				paneID, prev = id, pp.Prompted
				// Recorded before typing so the turn's end cannot beat it; marks the turn for posting.
				pp.Prompted = append(pp.Prompted, text)
				pp.Prompted = pp.Prompted[max(0, len(pp.Prompted)-5):]
				// ponytail: the reply goes to the thread of the latest question and mentions its asker, so a
				// turn still answering an earlier question is posted there, tagging the latest asker; record
				// them per prompt if that bites.
				pp.ReplyRoot, pp.Asker = root, asker
			}
		}
		return nil
	})
	if paneID == "" {
		return nil // mirroring was switched off
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

// listPanes lists the panes mirrored into the channel, with a link to each one's thread.
func (a *app) listPanes(channelID string) string {
	var lines []string
	a.withState(func(panes map[string]*pane) error {
		labels, _, _, _ := a.labels(panes)
		for id, p := range panes {
			if p.ChannelID != channelID {
				continue
			}
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

// notice posts into the Town Square of the bot's first team by name, where the daemon announces that it
// connected.
func (a *app) notice(p post) {
	teams, err := a.teams()
	var ch struct{ ID string }
	if err == nil && len(teams) == 0 {
		err = errors.New("the bot is in no team")
	}
	if err == nil {
		err = a.api(http.MethodGet, "/teams/"+url.PathEscape(teams[0].ID)+"/channels/name/town-square", nil, &ch)
	}
	if err != nil {
		log.Printf("notice: %v", err)
		return
	}
	p.ChannelID = ch.ID
	if err := a.api(http.MethodPost, "/posts", p, nil); err != nil {
		log.Printf("post: %v", err)
	}
}

func (a *app) sayIn(channelID, rootID, msg string) {
	if _, err := a.createPost(channelID, rootID, msg); err != nil {
		log.Printf("post: %v", err)
	}
}
