package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/font/sfnt"
	"golang.org/x/image/math/fixed"
	"golang.org/x/text/unicode/norm"
	"golang.org/x/text/width"
)

// captureCmd reports whether a thread reply asks for a screenshot, and the text to type first.
func captureCmd(msg string) (string, bool) { return prefixCmd(msg, "#capture") }

// execCmd reports whether a thread reply asks to type the rest as is, such as a "/clear" that
// Mattermost would otherwise take as its own slash command.
func execCmd(msg string) (string, bool) { return prefixCmd(msg, "#exec") }

// prefixCmd reports whether msg starts with the word prefix, and the text after it.
func prefixCmd(msg, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(msg), prefix)
	if !ok || rest != "" && !unicode.IsSpace(rune(rest[0])) {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

var (
	captureSettle  = 2 * time.Second  // the screen must stay unchanged this long
	captureTimeout = 60 * time.Second // then it is captured as it is
	capturePoll    = 500 * time.Millisecond
)

// escCommands are the slash commands whose panel stays open until Esc.
var escCommands = map[string]bool{"/status": true, "/stats": true, "/usage": true}

// capture posts a screenshot of the pane mirrored in the DM thread rootID, or linked to channelID, into
// that thread once the agent is blocked, or is not working and the screen has settled. After one of
// escCommands it presses Esc to close the panel Claude leaves open, but only when herdr reports the agent
// idle or done.
func (a *app) capture(channelID, rootID, cmd string) {
	panes, err := a.readPanes()
	if err != nil {
		log.Printf("capture: %v", err)
		return
	}
	var id string
	for pid, p := range panes {
		if channelID == a.dmID && p.RootID == rootID || channelID != a.dmID && p.ChannelID == channelID {
			id = pid
		}
	}
	if id == "" {
		return
	}
	screen, settled, err := a.settledScreen(id)
	if err != nil {
		a.sayIn(channelID, rootID, "❌ Could not read the pane: "+err.Error())
		return
	}
	msg := fmt.Sprintf("📸 Screen of pane `%s`", id)
	if !settled {
		msg = fmt.Sprintf("⏱ Pane `%s` was still changing after %s, so this is the screen at that point.", id, captureTimeout)
	}
	if err := a.postScreen(channelID, rootID, msg, parseANSI(string(screen))); err != nil {
		a.sayIn(channelID, rootID, "❌ Could not post the screenshot: "+err.Error())
	}
	if f := strings.Fields(cmd); len(f) == 0 || !escCommands[f[0]] {
		return
	}
	if info, err := a.agent(id); err == nil && (info.Status == "idle" || info.Status == "done") {
		if _, err := a.herdr("agent", "send-keys", id, "esc"); err != nil {
			log.Printf("capture: esc: %v", err)
		}
	}
}

// settledScreen reads the pane's visible screen once it has settled or the agent is blocked. settled is
// false when it timed out.
func (a *app) settledScreen(id string) (screen []byte, settled bool, err error) {
	deadline := time.Now().Add(captureTimeout)
	var since time.Time
	for {
		s, err := a.herdr("pane", "read", id, "--source", "visible", "--format", "ansi")
		if err != nil {
			return nil, false, err
		}
		// A blocked agent waits on the user, so its screen is final even though Claude blinks the
		// pending tool's bullet while a dialog is open.
		info, infoErr := a.agent(id)
		if infoErr == nil && info.Status == "blocked" {
			return s, true, nil
		}
		if !bytes.Equal(s, screen) {
			screen, since = s, time.Now()
		} else if time.Since(since) >= captureSettle && (infoErr != nil || info.Status != "working") {
			return screen, true, nil
		}
		if time.Now().After(deadline) {
			return screen, false, nil
		}
		time.Sleep(capturePoll)
	}
}

// postScreen posts the grid into the thread as a PNG.
func (a *app) postScreen(channelID, rootID, msg string, g [][]cell) error {
	faces, err := loadFonts()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, render(g, faces)); err != nil {
		return err
	}
	id, err := a.uploadFile(channelID, "screen.png", buf.Bytes())
	if err != nil {
		return err
	}
	return a.api(http.MethodPost, "/posts", post{ChannelID: channelID, RootID: rootID, Message: msg, FileIDs: []string{id}}, nil)
}

// cell is one terminal cell; the cell after a wide one is left empty and skipped.
type cell struct {
	text     string // a character and its combining marks, "" for blank
	fg, bg   color.RGBA
	bold     bool
	wide     bool
	reversed bool
}

var (
	defaultFG = color.RGBA{0xd4, 0xd4, 0xd4, 0xff}
	defaultBG = color.RGBA{0x1e, 0x1e, 0x1e, 0xff}
	ansi16    = [16]color.RGBA{
		{0x00, 0x00, 0x00, 0xff}, {0xcd, 0x31, 0x31, 0xff}, {0x0d, 0xbc, 0x79, 0xff}, {0xe5, 0xe5, 0x10, 0xff},
		{0x24, 0x72, 0xc8, 0xff}, {0xbc, 0x3f, 0xbc, 0xff}, {0x11, 0xa8, 0xcd, 0xff}, {0xe5, 0xe5, 0xe5, 0xff},
		{0x66, 0x66, 0x66, 0xff}, {0xf1, 0x4c, 0x4c, 0xff}, {0x23, 0xd1, 0x8b, 0xff}, {0xf5, 0xf5, 0x43, 0xff},
		{0x3b, 0x8e, 0xea, 0xff}, {0xd6, 0x70, 0xd6, 0xff}, {0x29, 0xb8, 0xdb, 0xff}, {0xff, 0xff, 0xff, 0xff},
	}
)

// color256 is xterm's 256-colour palette.
func color256(n int) color.RGBA {
	switch {
	case n < 16:
		return ansi16[n]
	case n < 232:
		n -= 16
		level := func(v int) uint8 {
			if v == 0 {
				return 0
			}
			return uint8(55 + 40*v)
		}
		return color.RGBA{level(n / 36), level(n / 6 % 6), level(n % 6), 0xff}
	}
	g := uint8(8 + 10*(n-232))
	return color.RGBA{g, g, g, 0xff}
}

// parseANSI turns a herdr ANSI snapshot (text, SGR sequences and line breaks) into rows of cells.
// Other escape sequences are skipped.
func parseANSI(s string) [][]cell {
	pen := cell{fg: defaultFG, bg: defaultBG}
	var rows [][]cell
	var row []cell
	for i := 0; i < len(s); {
		switch s[i] {
		case '\x1b':
			i = escape(s, i, &pen)
			continue
		case '\n':
			rows, row = append(rows, row), nil
		case '\t':
			for row = append(row, cell{fg: pen.fg, bg: pen.bg}); len(row)%8 != 0; {
				row = append(row, cell{fg: pen.fg, bg: pen.bg})
			}
		case '\r':
		default:
			r, n := utf8.DecodeRuneInString(s[i:])
			i += n
			if r < ' ' {
				continue
			}
			if unicode.In(r, unicode.Mn, unicode.Me) || r == 0x200d || r == 0xfe0f {
				for j := len(row) - 1; j >= 0; j-- { // joins the character before it
					if row[j].text != "" {
						row[j].text += string(r)
						break
					}
				}
				continue
			}
			c := pen
			c.text, c.wide = string(r), wide(r)
			if r == ' ' {
				c.text = ""
			}
			row = append(row, c)
			if c.wide {
				row = append(row, cell{fg: c.fg, bg: c.bg})
			}
			continue
		}
		i++
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// escape applies the escape sequence at s[i] to pen and returns the index after it.
func escape(s string, i int, pen *cell) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[': // CSI: parameters, then a final byte
		j := i + 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		if j < len(s) && s[j] == 'm' {
			sgr(s[i+2:j], pen)
		}
		return min(j+1, len(s))
	case ']': // OSC, such as a hyperlink: up to BEL or ESC \
		for j := i + 2; j < len(s); j++ {
			if s[j] == '\a' {
				return j + 1
			}
			if s[j] == '\x1b' && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	}
	return i + 2
}

// sgr applies Select Graphic Rendition parameters, such as "1;38;5;2", to pen.
func sgr(params string, pen *cell) {
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	if len(ps) == 0 {
		ps = []string{"0"}
	}
	num := func(k int) int {
		if k >= len(ps) {
			return 0
		}
		n, _ := strconv.Atoi(ps[k])
		return n
	}
	for k := 0; k < len(ps); k++ {
		switch n := num(k); {
		case n == 0:
			*pen = cell{fg: defaultFG, bg: defaultBG}
		case n == 1:
			pen.bold = true
		case n == 22:
			pen.bold = false
		case n == 7:
			pen.reversed = true
		case n == 27:
			pen.reversed = false
		case n >= 30 && n <= 37:
			pen.fg = ansi16[n-30]
		case n >= 90 && n <= 97:
			pen.fg = ansi16[n-90+8]
		case n >= 40 && n <= 47:
			pen.bg = ansi16[n-40]
		case n >= 100 && n <= 107:
			pen.bg = ansi16[n-100+8]
		case n == 39:
			pen.fg = defaultFG
		case n == 49:
			pen.bg = defaultBG
		case n == 38 || n == 48:
			var c color.RGBA
			switch num(k + 1) {
			case 5:
				c, k = color256(num(k+2)&0xff), k+2
			case 2:
				c, k = color.RGBA{uint8(num(k + 2)), uint8(num(k + 3)), uint8(num(k + 4)), 0xff}, k+4
			default:
				continue
			}
			if n == 38 {
				pen.fg = c
			} else {
				pen.bg = c
			}
		}
	}
}

// wide reports whether a terminal draws r two cells wide: East Asian wide and fullwidth characters,
// which include emoji shown as pictures.
func wide(r rune) bool {
	k := width.LookupRune(r).Kind()
	return k == width.EastAsianWide || k == width.EastAsianFullwidth
}

// fontPaths are where common monospace fonts live on Linux and macOS.
var fontPaths = []string{
	"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
	"/usr/share/fonts/dejavu/DejaVuSansMono.ttf",
	"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
	"/usr/share/fonts/dejavu-sans-mono-fonts/DejaVuSansMono.ttf",
	"/usr/share/fonts/truetype/ubuntu/UbuntuMono-R.ttf",
	"/usr/share/fonts/truetype/ubuntu/UbuntuMono[wght].ttf",
	"/usr/share/fonts/truetype/liberation/LiberationMono-Regular.ttf",
	"/usr/share/fonts/liberation-mono/LiberationMono-Regular.ttf",
	"/System/Library/Fonts/Menlo.ttc",
	"/System/Library/Fonts/Monaco.ttf",
}

// fallbackPaths are fonts searched, in order, for characters the monospace font lacks.
var fallbackPaths = []string{
	"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
	"/usr/share/fonts/dejavu/DejaVuSans.ttf",
	"/usr/share/fonts/TTF/DejaVuSans.ttf",
	"/usr/share/fonts/dejavu-sans-fonts/DejaVuSans.ttf",
	"/usr/share/fonts/truetype/noto/NotoSansSymbols-Regular.ttf",
	"/usr/share/fonts/noto/NotoSansSymbols-Regular.ttf",
	"/usr/share/fonts/google-noto/NotoSansSymbols-Regular.ttf",
	"/usr/share/fonts/truetype/noto/NotoSansSymbols2-Regular.ttf",
	"/usr/share/fonts/noto/NotoSansSymbols2-Regular.ttf",
	"/usr/share/fonts/google-noto/NotoSansSymbols2-Regular.ttf",
	"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
	"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
	"/System/Library/Fonts/Apple Symbols.ttf",
	"/System/Library/Fonts/Supplemental/Arial Unicode.ttf",
}

// loadFonts opens the first monospace font found in fontPaths, followed by the fallbacks found.
func loadFonts() ([]font.Face, error) {
	var faces []font.Face
	for _, path := range fontPaths {
		if f := openFace(path); f != nil {
			faces = append(faces, f)
			break
		}
	}
	if faces == nil {
		return nil, errors.New("no monospace font found (looked for DejaVu Sans Mono, Ubuntu Mono, Liberation Mono, Menlo and Monaco)")
	}
	for _, path := range fallbackPaths {
		if f := openFace(path); f != nil {
			faces = append(faces, f)
		}
	}
	return faces, nil
}

// openFace opens the font at path, or returns nil.
func openFace(path string) font.Face {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	coll, err := opentype.ParseCollection(b) // a single font parses as a collection of one
	var f *sfnt.Font
	if err == nil {
		f, err = coll.Font(0)
	}
	var face font.Face
	if err == nil {
		face, err = opentype.NewFace(f, &opentype.FaceOptions{Size: 16, DPI: 72, Hinting: font.HintingFull})
	}
	if err != nil {
		log.Printf("font %s: %v", path, err)
		return nil
	}
	return face
}

// substitutes stand in for characters no font has: the free space and autocompact buffer squares
// of Claude's /context.
var substitutes = map[rune]rune{0x26f6: '□', 0x26dd: '⊠'}

// faceFor returns the first face that has r, or else r's substitute. ok is false when none has either.
func faceFor(faces []font.Face, r rune) (font.Face, rune, bool) {
	for _, r := range []rune{r, substitutes[r]} {
		for _, f := range faces {
			if _, ok := f.GlyphAdvance(r); ok && r != 0 {
				return f, r, true
			}
		}
	}
	return faces[0], r, false
}

const pad = 8 // pixels around the screen

// render draws the grid in cells the size of the first face's, one cell per character.
func render(g [][]cell, faces []font.Face) image.Image {
	adv, _ := faces[0].GlyphAdvance('M')
	m := faces[0].Metrics()
	cw, ch := adv.Ceil(), (m.Ascent + m.Descent).Ceil()
	cols := 1
	for _, row := range g {
		cols = max(cols, len(row))
	}
	img := image.NewRGBA(image.Rect(0, 0, 2*pad+cols*cw, 2*pad+max(len(g), 1)*ch))
	draw.Draw(img, img.Bounds(), image.NewUniform(defaultBG), image.Point{}, draw.Src)
	for y, row := range g {
		for x := 0; x < len(row); x++ {
			c := row[x]
			fg, bg := c.fg, c.bg
			if c.reversed {
				fg, bg = bg, fg
			}
			w := cw
			if c.wide {
				w *= 2
			}
			px, py := pad+x*cw, pad+y*ch
			draw.Draw(img, image.Rect(px, py, px+w, py+ch), image.NewUniform(bg), image.Point{}, draw.Src)
			for i, r := range []rune(norm.NFC.String(c.text)) { // NFC, so only marks with no precomposed form are drawn apart
				f, r, ok := faceFor(faces, r)
				if !ok && i > 0 {
					continue // a mark or joiner no font has
				}
				dot := fixed.P(px, py+m.Ascent.Ceil())
				if i > 0 { // a mark ends where its cell does, so it stays over its character
					adv, _ := f.GlyphAdvance(r)
					dot.X += fixed.I(w) - adv
				}
				for dx := range 1 + btoi(c.bold) { // bold is drawn twice, one pixel apart
					glyph(img, f, r, dot.Add(fixed.P(dx, 0)), w, py+ch/2, image.NewUniform(fg))
				}
			}
			if c.wide {
				x++
			}
		}
	}
	return img
}

// glyph draws r from f at dot, as f's box when f lacks it. A glyph wider than w is shrunk towards dot's
// x and the row mid, so it fits its cell.
func glyph(img *image.RGBA, f font.Face, r rune, dot fixed.Point26_6, w, mid int, src image.Image) {
	dr, mask, mp, adv, _ := f.Glyph(dot, r)
	if k := float64(fixed.I(w)) / float64(adv); k < 1 {
		x := dot.X.Round()
		scale := func(v, o int) int { return o + int(math.Round(float64(v-o)*k)) }
		sr := image.Rectangle{mp, mp.Add(dr.Size())}
		dr = image.Rect(scale(dr.Min.X, x), scale(dr.Min.Y, mid), scale(dr.Max.X, x), scale(dr.Max.Y, mid))
		shrunk := image.NewAlpha(dr)
		draw.CatmullRom.Scale(shrunk, dr, mask, sr, draw.Src, nil)
		mask, mp = shrunk, dr.Min
	}
	draw.DrawMask(img, dr, src, image.Point{}, mask, mp, draw.Over)
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
