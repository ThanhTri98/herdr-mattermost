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
	"unicode"
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

// usernames lists a whitelist's usernames, the first max of them followed by how many more there are.
func usernames(ms []member, max int) string {
	var names []string
	for _, m := range ms {
		names = append(names, m.Username)
	}
	if len(names) > max {
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

// editWhitelist edits on w the whitelist of a channel, prefilled with the current one, with keys read
// from r, and saves the names typed, comma or space separated; an empty line empties it. A name already
// on the list keeps its member, so only added names are looked up. Esc leaves it unchanged and returns
// false.
func (a *app) editWhitelist(r *bufio.Reader, w io.Writer, t target) (bool, error) {
	lists, err := a.readWhitelists()
	if err != nil {
		return false, err
	}
	line, ok, err := readLine(r, w, fmt.Sprintf(a.t("whitelist.prompt"), a.targetName(t)), usernames(lists[t.ID], len(lists[t.ID])))
	if !ok || err != nil {
		return false, err
	}
	names := strings.FieldsFunc(strings.ToLower(line), func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	for i, n := range names {
		names[i] = strings.TrimPrefix(n, "@")
	}
	slices.Sort(names)
	if names = slices.Compact(slices.DeleteFunc(names, func(n string) bool { return n == "" })); len(names) == 0 {
		delete(lists, t.ID)
	} else {
		var ms []member
		var added []string
		for _, n := range names {
			if i := slices.IndexFunc(lists[t.ID], func(m member) bool { return strings.EqualFold(m.Username, n) }); i >= 0 {
				ms = append(ms, lists[t.ID][i])
			} else {
				added = append(added, n)
			}
		}
		if len(added) > 0 {
			found, err := a.resolveUsers(added)
			if err != nil {
				return false, err
			}
			ms = append(ms, found...)
			slices.SortFunc(ms, func(x, y member) int { return strings.Compare(x.Username, y.Username) })
		}
		lists[t.ID] = ms
	}
	return true, writeFile(filepath.Join(a.stateDir, "whitelists.json"), lists)
}

// readLine edits line after prompt on the raw terminal w, with keys read from r: text is typed at the
// cursor, Backspace deletes before it and Left and Right move it. Enter returns the line; Esc returns
// false.
func readLine(r *bufio.Reader, w io.Writer, prompt, line string) (string, bool, error) {
	buf := []rune(line)
	cur := len(buf)
	for {
		// The cursor is saved where it goes and restored after the rest, so a wrapped line still works.
		fmt.Fprintf(w, "\x1b[H\x1b[2J%s%s\x1b7%s\x1b8", prompt, string(buf[:cur]), string(buf[cur:]))
		c, _, err := r.ReadRune()
		if err != nil {
			return "", false, err
		}
		switch {
		case c == '\r' || c == '\n':
			return string(buf), true, nil
		case c == 0x1b:
			// A key's escape sequence arrives in one read; a lone Esc has nothing after it.
			if r.Buffered() == 0 {
				return "", false, nil
			}
			if b, _ := r.Peek(1); b[0] != '[' {
				return "", false, nil
			}
			r.ReadByte()
			var final byte
			for r.Buffered() > 0 && (final < 0x40 || final > 0x7e) {
				final, _ = r.ReadByte()
			}
			switch final {
			case 'C':
				cur = min(cur+1, len(buf))
			case 'D':
				cur = max(cur-1, 0)
			}
		case c == 0x7f || c == '\b':
			if cur > 0 {
				buf = slices.Delete(buf, cur-1, cur)
				cur--
			}
		case unicode.IsPrint(c):
			buf = slices.Insert(buf, cur, c)
			cur++
		}
	}
}

// whitelistScreen edits a channel's whitelist in the popup and returns the message to show: none after
// Esc.
func (a *app) whitelistScreen(t target) string {
	saved, err := a.editWhitelist(keyboard, os.Stdout, t)
	if err != nil {
		return err.Error()
	}
	if !saved {
		return ""
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
