package main

import (
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureCmd(t *testing.T) {
	for msg, want := range map[string]string{
		"@capture /context":    "/context",
		"  @capture  fix it  ": "fix it",
		"@capture":             "",
		"@capture\n/context":   "/context",
	} {
		if got, ok := captureCmd(msg); !ok || got != want {
			t.Errorf("captureCmd(%q) = %q, %v; want %q", msg, got, ok, want)
		}
	}
	for _, msg := range []string{"@capturex", "please @capture", "/context", ""} {
		if _, ok := captureCmd(msg); ok {
			t.Errorf("captureCmd(%q) matched", msg)
		}
	}
}

func TestParseANSI(t *testing.T) {
	g := parseANSI("\x1b[0m\x1b[1m\x1b[38;5;2mok\x1b[0m \x1b[31mĐỏ\x1b[39m\r\n中x\x1b]8;;http://x\x1b\\é\x1b[48;2;1;2;3m \x1b[7mr\r\n\ttab")
	if len(g) != 3 {
		t.Fatalf("rows = %d", len(g))
	}
	if c := g[0][0]; c.text != "o" || !c.bold || c.fg != color256(2) || c.bg != defaultBG {
		t.Errorf("bold 256-colour cell = %+v", c)
	}
	if c := g[0][2]; c.text != "" || c.bold || c.fg != defaultFG {
		t.Errorf("reset blank = %+v", c)
	}
	if c := g[0][3]; c.text != "Đ" || c.fg != ansi16[1] || g[0][4].text != "ỏ" {
		t.Errorf("Vietnamese red cells = %+v %+v", c, g[0][4])
	}
	if c := g[1][0]; c.text != "中" || !c.wide || g[1][1].text != "" || g[1][2].text != "x" {
		t.Errorf("wide cell = %+v, then %+v %+v", c, g[1][1], g[1][2])
	}
	if c := g[1][3]; c.text != "é" || c.wide {
		t.Errorf("combining mark joins its character, hyperlink skipped: %+v", c)
	}
	if c := g[1][4]; c.bg != (color.RGBA{1, 2, 3, 0xff}) || !g[1][5].reversed {
		t.Errorf("truecolour background, reverse = %+v %+v", c, g[1][5])
	}
	if row := g[2]; len(row) != 11 || row[7].text != "" || row[8].text != "t" {
		t.Errorf("tab row = %+v", row)
	}
}

// cellsDiffer reports whether one-row images a and b differ anywhere in columns [from, to).
func cellsDiffer(a, b image.Image, from, to int) bool {
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := from; x < to; x++ {
			if a.At(x, y) != b.At(x, y) {
				return true
			}
		}
	}
	return false
}

func TestRenderKeepsGlyphsInTheirCells(t *testing.T) {
	faces, err := loadFonts()
	if err != nil {
		t.Skip(err)
	}
	adv, _ := faces[0].GlyphAdvance('M')
	cw := adv.Ceil()
	row := func(s string) image.Image { return render(parseANSI(s), faces) }

	plain, marked := row("ex"), row("e\u0301x")
	if !cellsDiffer(plain, marked, pad, pad+cw) {
		t.Error("the combining acute accent is not drawn in its cell")
	}
	if cellsDiffer(plain, marked, pad+cw, plain.Bounds().Max.X) {
		t.Error("the combining acute accent is drawn outside its cell")
	}

	for s, cells := range map[string]int{"⛀": 1, "⛁": 1, "中": 2} { // 中 is a box without a CJK font
		blank, img := row(strings.Repeat(" ", cells)), row(s)
		if !cellsDiffer(blank, img, pad, pad+cells*cw) || cellsDiffer(blank, img, pad+cells*cw, img.Bounds().Max.X) {
			t.Errorf("%s is not drawn within its cell", s)
		}
	}

	has := func(r rune) bool {
		for _, f := range faces {
			if _, ok := f.GlyphAdvance(r); ok {
				return true
			}
		}
		return false
	}
	for from, to := range substitutes {
		if a := row(string(from)); !has(from) && has(to) && cellsDiffer(a, row(string(to)), 0, a.Bounds().Max.X) {
			t.Errorf("%c, which no font has, is not drawn as %c", from, to)
		}
	}
}

// fastCapture shortens the settle wait for a test.
func fastCapture(t *testing.T) {
	settle, timeout, poll := captureSettle, captureTimeout, capturePoll
	captureSettle, captureTimeout, capturePoll = 100*time.Millisecond, 2*time.Second, 20*time.Millisecond
	t.Cleanup(func() { captureSettle, captureTimeout, capturePoll = settle, timeout, poll })
}

// waitPost waits for the bot to post n messages and returns the last.
func (e *testEnv) waitPost(t *testing.T, n int) post {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if posts := e.mm.snapshot(); len(posts) >= n {
			return posts[n-1]
		}
	}
	t.Fatalf("posts = %+v, want %d", e.mm.snapshot(), n)
	return post{}
}

func TestCaptureTypesThenUploadsScreenshot(t *testing.T) {
	if _, err := loadFonts(); err != nil {
		t.Skip(err)
	}
	fastCapture(t)
	e := mirroredEnv(t)
	e.setAgent(t, "idle")
	os.WriteFile(filepath.Join(e.herdrDir, "screen.ansi"), []byte("\x1b[1mContext\x1b[0m ⛁ 12%\r\n"), 0o644)

	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "@capture /context", CreateAt: 1}))
	if ack := e.waitPost(t, 1); ack.Message != "📥 Received - the agent is working on it." {
		t.Fatalf("ack = %+v", ack)
	}
	shot := e.waitPost(t, 2)
	if got := e.prompts(); got != "w1:p1|/context\n" {
		t.Fatalf("typed %q, want the text after @capture", got)
	}
	if shot.RootID != "root1" || shot.ChannelID != "dm" || len(shot.FileIDs) != 1 || shot.FileIDs[0] != "file1" || !strings.Contains(shot.Message, "w1:p1") {
		t.Fatalf("screenshot post = %+v", shot)
	}
	e.mm.mu.Lock()
	files := e.mm.files
	e.mm.mu.Unlock()
	if len(files) != 1 || !strings.HasPrefix(files[0], "dm|screen.png|\x89PNG") {
		t.Fatalf("uploads = %.40q", files)
	}
	panes, _ := e.a.readPanes()
	if p := panes["w1:p1"]; len(p.Prompted) != 1 || p.Prompted[0] != "/context" {
		t.Fatalf("Prompted = %q, want the typed text so the reply is still posted", p.Prompted)
	}

	e.a.handleEvent(posted(post{ChannelID: "dm", RootID: "root1", UserID: "alice-id", Message: "@capture", CreateAt: 2}))
	if shot := e.waitPost(t, 3); len(shot.FileIDs) != 1 || shot.FileIDs[0] != "file2" {
		t.Fatalf("bare @capture post = %+v", shot)
	}
	if got := e.prompts(); got != "w1:p1|/context\n" {
		t.Fatalf("bare @capture typed something: %q", got)
	}
}

func TestCaptureTimeoutAndNoFont(t *testing.T) {
	fastCapture(t)
	captureTimeout = 300 * time.Millisecond
	e := mirroredEnv(t)
	e.setAgent(t, "working")
	os.WriteFile(filepath.Join(e.herdrDir, "screen.ansi"), []byte("\x1b[32mstill going\x1b[0m\r\n"), 0o644)

	fonts := fontPaths
	fontPaths = nil
	e.a.capture("root1")
	fontPaths = fonts
	posts := e.mm.snapshot()
	if len(posts) != 1 || len(posts[0].FileIDs) != 0 || !strings.HasPrefix(posts[0].Message, "❌ Could not post the screenshot: no monospace font found") {
		t.Fatalf("no-font posts = %+v", posts)
	}

	if _, err := loadFonts(); err != nil {
		t.Skip(err)
	}
	e.a.capture("root1")
	posts = e.mm.snapshot()
	if len(posts) != 2 || len(posts[1].FileIDs) != 1 || !strings.Contains(posts[1].Message, "still changing") {
		t.Fatalf("timed-out posts = %+v", posts)
	}
}
