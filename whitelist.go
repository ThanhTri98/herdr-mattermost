package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// member is a Mattermost user on a channel's whitelist: the bot obeys them in that channel.
type member struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

// readWhitelists loads whitelists.json, which maps channel ids to their whitelist. Keyed by channel, a
// whitelist follows its channel from pane to pane. Only the popup writes it; the daemon reads it for
// every post, so an edit applies at once.
func (a *app) readWhitelists() (map[string][]member, error) {
	path := filepath.Join(a.stateDir, "whitelists.json")
	lists := map[string][]member{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &lists); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	return lists, nil
}

// whitelist is the channel's whitelist. One that cannot be read obeys nobody.
func (a *app) whitelist(channelID string) []member {
	lists, err := a.readWhitelists()
	if err != nil {
		log.Printf("whitelist: %v", err)
	}
	return lists[channelID]
}

// usernames lists a whitelist's usernames, the first max of them followed by how many more there are,
// or all of them for max 0.
func usernames(ms []member, max int) string {
	var names []string
	for _, m := range ms {
		names = append(names, m.Username)
	}
	if max > 0 && len(names) > max {
		return strings.Join(names[:max], ", ") + fmt.Sprintf(" +%d", len(names)-max)
	}
	return strings.Join(names, ", ")
}

// resolveUsers looks up the usernames typed in the popup, rejecting the ones Mattermost does not know.
func (a *app) resolveUsers(names []string) ([]member, error) {
	if err := a.requireMM(); err != nil {
		return nil, err
	}
	var out []member
	var unknown []string
	for _, n := range names {
		var u struct{ ID, Username string }
		err := a.api(http.MethodGet, "/users/username/"+url.PathEscape(n), nil, &u)
		if ae := (*apiError)(nil); errors.As(err, &ae) && (ae.status == http.StatusNotFound || ae.status == http.StatusBadRequest) {
			unknown = append(unknown, "@"+n)
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, member{u.ID, u.Username})
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf(a.t("whitelist.unknown"), strings.Join(unknown, ", "))
	}
	return out, nil
}

// editWhitelist asks on w for the whitelist of a channel and reads it from r, comma or space separated,
// then saves it. An empty answer keeps the current list and "-" empties it.
func (a *app) editWhitelist(r *bufio.Reader, w io.Writer, t target) error {
	lists, err := a.readWhitelists()
	if err != nil {
		return err
	}
	current := usernames(lists[t.ID], 0)
	if current == "" {
		current = a.t("whitelist.none")
	}
	fmt.Fprintf(w, a.t("whitelist.prompt"), current, a.targetName(t))
	line, err := r.ReadString('\n')
	if err != nil && (line == "" || !errors.Is(err, io.EOF)) {
		return err
	}
	switch line = strings.TrimSpace(line); line {
	case "":
		return nil
	case "-":
		delete(lists, t.ID)
	default:
		names := strings.FieldsFunc(strings.ToLower(line), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		for i, n := range names {
			names[i] = strings.TrimPrefix(n, "@")
		}
		slices.Sort(names)
		ms, err := a.resolveUsers(slices.Compact(slices.DeleteFunc(names, func(n string) bool { return n == "" })))
		if err != nil {
			return err
		}
		lists[t.ID] = ms
	}
	return writeFile(filepath.Join(a.stateDir, "whitelists.json"), lists)
}

// whitelistScreen asks for a channel's whitelist in the popup and returns the message to show.
func (a *app) whitelistScreen(t target) string {
	fmt.Print("\x1b[H\x1b[2J")
	stty("icanon", "echo")
	defer stty("-icanon", "-echo", "min", "1")
	if err := a.editWhitelist(bufio.NewReader(os.Stdin), os.Stdout, t); err != nil {
		return err.Error()
	}
	return a.t("whitelist.saved")
}

// refusal answers someone not on the channel's whitelist who mentioned the bot, mentioning them.
func (a *app) refusal(userID string) string {
	msg := a.t("channel.refused")
	var u struct{ Username string }
	if err := a.api(http.MethodGet, "/users/"+url.PathEscape(userID), nil, &u); err != nil || u.Username == "" {
		log.Printf("refusal: user %s: %v", userID, err)
		return msg
	}
	return "@" + u.Username + " " + msg
}
