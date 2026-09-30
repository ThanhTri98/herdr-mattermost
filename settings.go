package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// settings are the values entered in the popup's settings screen, saved in settings.json.
type settings struct {
	URL   string `json:"url,omitempty"`
	Token string `json:"token,omitempty"`
}

func readSettings(path string) (settings, error) {
	var s settings
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// writeFile replaces path by rename, so it is never read half written, with mode 0600.
func writeFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err == nil {
		err = os.WriteFile(path+".tmp", b, 0o600)
	}
	if err == nil {
		err = os.Rename(path+".tmp", path)
	}
	return err
}

// editSettings asks for each setting on w and reads the answers from r, then saves them. An empty
// answer keeps the saved value, so a field left unsaved still comes from .env.
func (a *app) editSettings(r *bufio.Reader, w io.Writer) error {
	s, err := readSettings(a.settingsPath)
	if err != nil {
		return err
	}
	ask := func(prompt string) (string, error) {
		fmt.Fprint(w, prompt)
		line, err := r.ReadString('\n')
		return strings.TrimSpace(line), err
	}
	fmt.Fprint(w, a.t("settings.title")+"\n\n")
	v, err := ask(fmt.Sprintf(a.t("settings.url"), a.mmURL))
	if err != nil {
		return err
	}
	if v != "" {
		s.URL = strings.TrimRight(v, "/")
	}
	set := a.t("settings.unset")
	if a.token != "" {
		set = a.t("settings.set")
	}
	if v, err = ask(fmt.Sprintf(a.t("settings.token"), set)); err != nil {
		return err
	}
	if v != "" {
		s.Token = v
	}
	if err := writeFile(a.settingsPath, s); err != nil {
		return err
	}
	a.mmURL, a.token = strings.TrimRight(cmp.Or(s.URL, a.mmURL), "/"), cmp.Or(s.Token, a.token)
	return nil
}

// readEnv parses KEY=VALUE lines, ignoring blanks, comments and an "export " prefix.
func readEnv(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	env := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
		k, v, ok := strings.Cut(line, "=")
		if ok && !strings.HasPrefix(line, "#") {
			env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return env, err
}

func (a *app) requireMM() error {
	var missing []string
	for _, kv := range [][2]string{{"MM_URL", a.mmURL}, {"MM_BOT_TOKEN", a.token}} {
		if kv[1] == "" {
			missing = append(missing, kv[0])
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(a.t("err.missing"), strings.Join(missing, ", "), a.envPath)
	}
	return nil
}

// settingsScreen asks for the settings, saves them and restarts the daemon so it uses them.
func (a *app) settingsScreen() string {
	fmt.Print("\x1b[H\x1b[2J")
	stty("icanon", "echo")
	defer stty("-icanon", "-echo", "min", "1")
	err := a.editSettings(bufio.NewReader(os.Stdin), os.Stdout)
	if err == nil {
		err = a.restart() // the next daemon reads the settings afresh
	}
	if err != nil {
		return err.Error()
	}
	return a.t("settings.saved")
}
