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
		"root.channel":    "_Nhắc (@mention) bot trong kênh này để gửi lệnh cho agent._",
		"channel.refused": "Chỉ @%s mới điều khiển được agent này.",
		"target.dm":       "DM",
		"target.taken":    "kênh %s đã gắn với một pane khác",
		"picker.loading":  "Đang tải danh sách kênh cho %s...",
		"picker.title":    "Chọn nơi đẩy %s lên: DM hoặc một kênh bot đang ở (mỗi kênh chỉ một pane)",
		"picker.hint":     "↑/↓ hoặc j/k để di chuyển, Enter để chọn, q hoặc Esc để huỷ.",
		"picker.linking":  "Đang gắn %s với %s...",
		"settings.title":  "Cài đặt Mattermost. Nhấn Enter để giữ giá trị hiện tại.",
		"settings.url":    "URL Mattermost [%s]: ",
		"settings.token":  "Token của bot, không hiện khi gõ [%s]: ",
		"settings.user":   "Username Mattermost của bạn [%s]: ",
		"settings.set":    "đã có",
		"settings.unset":  "chưa có",
		"settings.saved":  "Đã lưu cài đặt và khởi động lại daemon.",
		"help": "**Các lệnh hỗ trợ**\n" +
			"- Trả lời trong thread của một pane: gửi lệnh cho agent.\n" +
			"- `#capture /command`: gõ lệnh rồi chụp màn hình pane, ví dụ `#capture /context`.\n" +
			"- `#exec /command`: gõ đúng lệnh vào pane, không chụp màn hình, ví dụ gọi một skill, `/clear`, `/compact`.\n" +
			"- `list`: xem các pane đang đẩy lên Mattermost (chỉ trong DM, gửi ngoài thread).\n" +
			"- `help`: hiện danh sách này. Trong thread, gõ sau @mention của bot.\n\n" +
			"Tin nhắn bắt đầu bằng `/` bị Mattermost hiểu là lệnh của nó, nên hãy dùng `#exec`.",
		"exec.usage":     "Cách dùng: `#exec /command`, ví dụ `#exec /clear`.",
		"list.none":      "Chưa có pane nào đẩy lên Mattermost. Chạy action bật/tắt Mattermost trên một pane herdr để bắt đầu.",
		"popup.running":  "Daemon Mattermost: đang chạy (pid %d)",
		"popup.stopped":  "Daemon Mattermost: không chạy",
		"popup.none":     "Không có pane agent nào.",
		"popup.header":   " \tTÊN\tAGENT\tTRẠNG THÁI\tĐANG ĐẨY\tĐẨY LÊN",
		"popup.yes":      "có",
		"popup.hint":     "↑/↓ hoặc j/k để di chuyển, Enter hoặc Space để bật/tắt đẩy lên Mattermost, t để chọn DM hay kênh, s để cài đặt, l để chuyển sang English. Nhấn q hoặc Esc để đóng.",
		"popup.toggling": "Đang bật/tắt %s...",
		"popup.close":    "Nhấn q hoặc Esc để đóng.",
		"popup.failed":   "Không bật/tắt được %s: %v",
		"popup.error":    "herdr-mm status gặp lỗi: %v",
		"truncated":      "\n… (đã cắt bớt)",
		"err.missing":    "thiếu %s: nhập bằng phím s trong popup trạng thái Mattermost, hoặc trong %s",
		"err.login":      "đăng nhập Mattermost thất bại, kiểm tra MM_URL và MM_BOT_TOKEN",
		"err.dm":         "không mở được DM với @%s",
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
		"root.channel":    "_@mention the bot in this channel to prompt the agent._",
		"channel.refused": "Only @%s can control this agent.",
		"target.dm":       "DM",
		"target.taken":    "channel %s is linked to another pane",
		"picker.loading":  "Loading the channels for %s...",
		"picker.title":    "Where to mirror %s: the DM or a channel the bot is in (one pane per channel)",
		"picker.hint":     "↑/↓ or j/k to move, Enter to pick, q or Esc to cancel.",
		"picker.linking":  "Linking %s to %s...",
		"settings.title":  "Mattermost settings. Press Enter to keep the current value.",
		"settings.url":    "Mattermost URL [%s]: ",
		"settings.token":  "Bot token, hidden while typed [%s]: ",
		"settings.user":   "Your Mattermost username [%s]: ",
		"settings.set":    "set",
		"settings.unset":  "not set",
		"settings.saved":  "Settings saved and the daemon restarted.",
		"help": "**Supported commands**\n" +
			"- Reply in a pane's thread: prompt its agent.\n" +
			"- `#capture /command`: type the command, then post a screenshot of the pane, for example `#capture /context`.\n" +
			"- `#exec /command`: type exactly the command into the pane, without a screenshot, for example a skill, `/clear`, `/compact`.\n" +
			"- `list`: list the mirrored panes (DM only, outside a thread).\n" +
			"- `help`: show this list. In a thread, write it after the bot's @mention.\n\n" +
			"A message starting with `/` is taken by Mattermost as its own slash command, so use `#exec`.",
		"exec.usage":     "Usage: `#exec /command`, for example `#exec /clear`.",
		"list.none":      "No panes are mirrored. Run the Mattermost toggle action on a herdr pane to mirror it.",
		"popup.running":  "Mattermost daemon: running (pid %d)",
		"popup.stopped":  "Mattermost daemon: not running",
		"popup.none":     "No agent panes.",
		"popup.header":   " \tNAME\tAGENT\tSTATUS\tMIRRORED\tTARGET",
		"popup.yes":      "yes",
		"popup.hint":     "↑/↓ or j/k to move, Enter or Space to toggle mirroring, t to pick the DM or a channel, s for settings, l to switch to Tiếng Việt. Press q or Esc to close.",
		"popup.toggling": "Toggling %s...",
		"popup.close":    "Press q or Esc to close.",
		"popup.failed":   "toggle %s: %v",
		"popup.error":    "herdr-mm status: %v",
		"truncated":      "\n… (truncated)",
		"err.missing":    "missing %s: set it with s in the Mattermost status popup, or in %s",
		"err.login":      "mattermost login failed, check MM_URL and MM_BOT_TOKEN",
		"err.dm":         "open DM with @%s",
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
