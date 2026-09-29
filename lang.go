package main

import (
	"os"
	"path/filepath"
	"strings"
)

// catalog holds every text the plugin writes itself, per language. Agent replies are never translated.
var catalog = map[string]map[string]string{
	"vi": {
		"idle": "rảnh", "done": "xong", "working": "đang làm", "blocked": "đang chờ bạn",
		"unknown": "không rõ", "off": "tắt", "closed": "đã đóng",
		"root.reply":      "_Trả lời trong thread này để gửi lệnh cho agent._",
		"root.off":        "_Đã dừng đẩy lên Mattermost._",
		"root.closed":     "_Pane đã đóng, đã dừng đẩy lên Mattermost._",
		"notice.off":      "Đã dừng đẩy lên Mattermost cho",
		"notice.closed":   "Pane đã đóng, đã dừng đẩy lên Mattermost cho",
		"dialog":          "@%s ✋ **%s** đang chờ bạn, vào máy trả lời giúp nhé:",
		"connected":       "🔌 Đã kết nối: daemon herdr-mm vừa khởi động.",
		"reconnected":     "🔌 Đã kết nối lại sau khi mất kết nối.",
		"plugin.off":      "⚪ herdr không chạy hoặc plugin Mattermost đang tắt, nên tin này chưa được gõ vào agent nào và bot đã ngừng nghe. Bật plugin, rồi bật/tắt một pane hoặc khởi động lại herdr để bot hoạt động lại.",
		"prompt.failed":   "❌ Không gửi được lệnh cho agent: ",
		"prompt.blocked":  "✋ Agent đang chờ bạn trả lời một hộp thoại. Vào máy duyệt hoặc trả lời, rồi nhắn lại.",
		"prompt.late":     "📬 Gửi trễ: lúc bạn gửi bot chưa kết nối, nên tin vừa được gõ vào agent.",
		"prompt.received": "📥 Đã nhận, agent đang xử lý.",
		"help":            "Trả lời trong thread của một pane để gửi lệnh cho agent, hoặc gửi `list` để xem các pane đang đẩy lên Mattermost.",
		"list.none":       "Chưa có pane nào đẩy lên Mattermost. Chạy action bật/tắt Mattermost trên một pane herdr để bắt đầu.",
		"popup.running":   "Daemon Mattermost: đang chạy (pid %d)",
		"popup.stopped":   "Daemon Mattermost: không chạy",
		"popup.none":      "Không có pane agent nào.",
		"popup.header":    " \tTÊN\tAGENT\tTRẠNG THÁI\tĐANG ĐẨY",
		"popup.yes":       "có",
		"popup.hint":      "↑/↓ hoặc j/k để di chuyển, Enter hoặc Space để bật/tắt đẩy lên Mattermost, l để chuyển sang English. Nhấn q hoặc Esc để đóng.",
		"popup.toggling":  "Đang bật/tắt %s...",
		"popup.close":     "Nhấn q hoặc Esc để đóng.",
	},
	"en": {
		"idle": "idle", "done": "done", "working": "working", "blocked": "blocked",
		"unknown": "unknown", "off": "off", "closed": "closed",
		"root.reply":      "_Reply in this thread to prompt the agent._",
		"root.off":        "_Mirroring stopped._",
		"root.closed":     "_Pane closed, mirroring stopped._",
		"notice.off":      "Mirroring stopped for",
		"notice.closed":   "Pane closed, mirroring stopped for",
		"dialog":          "@%s ✋ **%s** is waiting on a dialog. Answer it on the machine:",
		"connected":       "🔌 Connected: the herdr-mm daemon started.",
		"reconnected":     "🔌 Reconnected after the connection dropped.",
		"plugin.off":      "⚪ herdr is not running or the Mattermost plugin is disabled, so this was not typed into any agent and the bot has stopped listening. Enable the plugin, then toggle a pane or restart herdr to bring it back.",
		"prompt.failed":   "❌ Could not prompt the agent: ",
		"prompt.blocked":  "✋ The agent is waiting on a dialog. Approve or answer it on the machine, then reply again.",
		"prompt.late":     "📬 Delivered late: the bot was not connected when you sent this, so it was typed into the agent just now.",
		"prompt.received": "📥 Received - the agent is working on it.",
		"help":            "Reply in a pane's thread to prompt its agent, or send `list` to see the mirrored panes.",
		"list.none":       "No panes are mirrored. Run the Mattermost toggle action on a herdr pane to mirror it.",
		"popup.running":   "Mattermost daemon: running (pid %d)",
		"popup.stopped":   "Mattermost daemon: not running",
		"popup.none":      "No agent panes.",
		"popup.header":    " \tNAME\tAGENT\tSTATUS\tMIRRORED",
		"popup.yes":       "yes",
		"popup.hint":      "↑/↓ or j/k to move, Enter or Space to toggle mirroring, l to switch to Tiếng Việt. Press q or Esc to close.",
		"popup.toggling":  "Toggling %s...",
		"popup.close":     "Press q or Esc to close.",
	},
}

// lang reads the language saved in the plugin state dir, read afresh for every text so a switch
// in the popup applies to the running daemon. Vietnamese is the default.
func lang(stateDir string) string {
	if b, _ := os.ReadFile(filepath.Join(stateDir, "lang")); strings.TrimSpace(string(b)) == "en" {
		return "en"
	}
	return "vi"
}

// t is the text for key in the current language.
func (a *app) t(key string) string { return catalog[lang(a.stateDir)][key] }

// switchLang saves the other language.
func (a *app) switchLang() error {
	next := "en"
	if lang(a.stateDir) == "en" {
		next = "vi"
	}
	return os.WriteFile(filepath.Join(a.stateDir, "lang"), []byte(next+"\n"), 0o600)
}
