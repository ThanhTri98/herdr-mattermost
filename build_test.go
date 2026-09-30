package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildScript runs build.sh with stub go, uname, git and curl on PATH:
// Go 1.25+ builds, anything else downloads this commit's binary for the OS/arch.
func TestBuildScript(t *testing.T) {
	script, err := filepath.Abs("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	const sha = "0123456789abcdef0123456789abcdef01234567"
	const base = "https://github.com/ThanhTri98/herdr-mattermost/releases/download/build-" + sha + "/herdr-mm-"
	for _, c := range []struct {
		name, goVersion, os, arch string
		curlFails                 bool
		want                      string // log line, or "error: <substring of stderr>"
	}{
		{"go 1.25 builds", "go1.25.0", "Linux", "x86_64", false, "go build -o herdr-mm ."},
		{"go 1.26 builds", "go1.26.1", "Linux", "x86_64", false, "go build -o herdr-mm ."},
		{"go 1.24 downloads", "go1.24.3", "Linux", "x86_64", false, "curl " + base + "linux-amd64"},
		{"no go, linux arm", "", "Linux", "aarch64", false, "curl " + base + "linux-arm64"},
		{"no go, mac intel", "", "Darwin", "x86_64", false, "curl " + base + "darwin-amd64"},
		{"no go, mac arm", "", "Darwin", "arm64", false, "curl " + base + "darwin-arm64"},
		{"unsupported os", "", "FreeBSD", "x86_64", false, "error: unsupported OS FreeBSD"},
		{"unsupported arch", "", "Linux", "riscv64", false, "error: unsupported architecture riscv64"},
		{"missing release", "", "Linux", "x86_64", true, "error: no prebuilt binary for commit " + sha},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir, bin := t.TempDir(), t.TempDir()
			stub := func(name, body string) {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if c.goVersion == "" {
				stub("go", "exit 127")
			} else {
				stub("go", `[ "$1" = version ] && { echo "go version `+c.goVersion+` linux/amd64"; exit; }; echo "go $*" >> log`)
			}
			stub("uname", `[ "$1" = -s ] && echo `+c.os+` || echo `+c.arch)
			stub("git", "echo "+sha)
			if c.curlFails {
				stub("curl", "exit 22")
			} else {
				stub("curl", `echo "curl $4" >> log; echo binary > "$3"`)
			}
			cmd := exec.Command("sh", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_STATE_HOME="+dir)
			out, err := cmd.CombinedOutput()
			if want, ok := strings.CutPrefix(c.want, "error: "); ok {
				if err == nil || !strings.Contains(string(out), want) {
					t.Fatalf("err %v, output %q, want failure mentioning %q", err, out, want)
				}
				if _, err := os.Stat(filepath.Join(dir, "herdr-mm")); err == nil {
					t.Fatal("herdr-mm exists after a failed build")
				}
				return
			}
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			log, _ := os.ReadFile(filepath.Join(dir, "log"))
			if got := strings.TrimSpace(string(log)); got != c.want {
				t.Fatalf("ran %q, want %q", got, c.want)
			}
			if strings.HasPrefix(c.want, "curl ") {
				if fi, err := os.Stat(filepath.Join(dir, "herdr-mm")); err != nil || fi.Mode()&0o100 == 0 {
					t.Fatalf("herdr-mm not an executable download: %v", err)
				}
			}
		})
	}
}
