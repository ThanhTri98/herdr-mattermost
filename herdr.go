package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

type herdrError struct{ Code, Message string }

func (e *herdrError) Error() string { return e.Code + ": " + e.Message }

// herdr runs the herdr CLI; a JSON error on stderr comes back as *herdrError.
func (a *app) herdr(args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(a.herdrBin, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var e struct{ Error herdrError }
		if json.Unmarshal(stderr.Bytes(), &e) == nil && e.Error.Code != "" {
			return out, &e.Error
		}
		return out, fmt.Errorf("herdr %s: %v: %s", strings.Join(args[:2], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

type agentInfo struct {
	Status  string `json:"agent_status"`
	Agent   string
	Cwd     string
	Session *struct{ Kind, Value string } `json:"agent_session"`
}

func (a *app) agent(id string) (agentInfo, error) {
	var r struct{ Result struct{ Agent agentInfo } }
	out, err := a.herdr("agent", "get", id)
	if err == nil {
		err = json.Unmarshal(out, &r)
	}
	return r.Result.Agent, err
}
