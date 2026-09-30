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
		"unknown": "không rõ", "noagent": "không có agent", "off": "tắt", "closed": "đã đóng",
		"root.off":            "_Đã dừng đẩy lên Mattermost._",
		"root.closed":         "_Pane đã đóng, đã dừng đẩy lên Mattermost._",
		"notice.off":          "Đã dừng đẩy lên Mattermost cho",
		"notice.closed":       "Pane đã đóng, đã dừng đẩy lên Mattermost cho",
		"dialog":              "@all ✋ **%s** đang chờ trả lời một hộp thoại, ai đó vào máy trả lời giúp nhé:",
		"notice.started":      "🟢 herdr-mm đã kết nối",
		"notice.reconnected":  "🟡 herdr-mm đã kết nối lại",
		"notice.host":         "Máy",
		"notice.panes":        "Đang đẩy",
		"notice.panecount":    "%d pane",
		"notice.down":         "Mất kết nối",
		"plugin.off":          "@all ⚪ herdr không chạy hoặc plugin Mattermost đang tắt, nên tin này chưa được gõ vào agent nào và bot đã ngừng nghe. Bật plugin, rồi bật/tắt một pane hoặc khởi động lại herdr để bot hoạt động lại.",
		"prompt.failed":       "@all ❌ Không gửi được lệnh cho agent: ",
		"prompt.blocked":      "@all ✋ Agent đang chờ trả lời một hộp thoại. Vào máy duyệt hoặc trả lời, rồi nhắn lại.",
		"prompt.late":         "📬 Gửi trễ: lúc bạn gửi bot chưa kết nối, nên tin vừa được gõ vào agent.",
		"prompt.received":     "📥 Đã nhận, agent đang xử lý.",
		"root.channel":        "_Nhắc (@mention) bot trong kênh này để gửi lệnh cho agent._",
		"channel.refused":     "Bạn không có quyền gọi mình",
		"whitelist.prompt":    "Whitelist của %s, các username Mattermost bot nghe lệnh trong kênh này, cách nhau bằng dấu phẩy hoặc khoảng trắng: ",
		"whitelist.none":      "trống",
		"whitelist.unknown":   "không có user Mattermost %s, whitelist chưa được lưu",
		"whitelist.saved":     "Đã lưu whitelist.",
		"whitelist.nochannel": "pane này chưa gắn kênh: nhấn t để chọn kênh trước",
		"target.taken":        "kênh %s đã gắn với một pane khác",
		"picker.loading":      "Đang tải danh sách kênh cho %s...",
		"picker.title":        "Chọn kênh để đẩy %s lên, trong các kênh bot đang ở (mỗi kênh chỉ một pane)",
		"picker.unlink":       "Bỏ gắn kênh (dừng đẩy lên Mattermost)",
		"picker.none":         "Bot chưa ở kênh nào còn trống. Thêm bot vào một kênh trong Mattermost rồi thử lại.",
		"toggle.nochannel":    "pane này chưa gắn kênh: mở popup trạng thái Mattermost, chọn pane và nhấn t để chọn kênh trước",
		"dm.refused":          "Bot chỉ hoạt động trong kênh. Mở popup trạng thái Mattermost, chọn một pane, nhấn t để chọn kênh rồi Enter để bắt đầu đẩy lên, sau đó @mention bot trong kênh đó.",
		"picker.hint":         "- ↑/↓ hoặc j/k: di chuyển\n- Enter: chọn\n- q hoặc Esc: huỷ",
		"picker.linking":      "Đang gắn %s với %s...",
		"picker.unlinking":    "Đang bỏ gắn kênh của %s...",
		"settings.title":      "Cài đặt Mattermost. Nhấn Enter để giữ giá trị hiện tại.",
		"settings.url":        "URL Mattermost [%s]: ",
		"settings.token":      "Token của bot [%s]: ",
		"settings.set":        "đã có",
		"settings.unset":      "chưa có",
		"settings.saved":      "Đã lưu cài đặt và khởi động lại daemon.",
		"help": "**Các lệnh hỗ trợ** (trong kênh có pane đang đẩy lên Mattermost, viết sau @mention của bot, ở ngoài hay trong thread bất kỳ)\n" +
			"- Nội dung bất kỳ: gửi lệnh cho agent của kênh.\n" +
			"- `#capture /command`: gõ lệnh rồi chụp màn hình pane, ví dụ `#capture /context`.\n" +
			"- `#exec /command`: gõ đúng lệnh vào pane, không chụp màn hình, ví dụ gọi một skill, `/clear`, `/compact`.\n" +
			"- `list`: xem các pane đang đẩy vào kênh này.\n" +
			"- `help`: hiện danh sách này.\n\n" +
			"Tin nhắn bắt đầu bằng `/` bị Mattermost hiểu là lệnh của nó, nên hãy dùng `#exec`.",
		"exec.usage":     "Cách dùng: `#exec /command`, ví dụ `#exec /clear`.",
		"list.none":      "Chưa có pane nào đẩy lên Mattermost. Mở popup trạng thái Mattermost, chọn một pane, nhấn t để chọn kênh rồi Enter để bắt đầu.",
		"popup.running":  "Daemon Mattermost: đang chạy (pid %d)",
		"popup.stopped":  "Daemon Mattermost: không chạy",
		"popup.none":     "Không có pane agent nào.",
		"popup.header":   " \tTÊN\tAGENT\tTRẠNG THÁI\tKẾT NỐI\tKÊNH\tWHITELIST",
		"popup.yes":      "Đã kết nối",
		"popup.no":       "Chưa kết nối",
		"popup.channel":  "Kênh:",
		"popup.allowed":  "Whitelist:",
		"popup.hint":     "- ↑/↓ hoặc j/k: di chuyển\n- Enter hoặc Space: bật/tắt đẩy lên Mattermost (chưa có kênh thì chọn kênh trước)\n- t: chọn hoặc bỏ gắn kênh\n- w: sửa whitelist của kênh\n- s: cài đặt\n- l: chuyển sang English\n- q hoặc Esc: đóng",
		"popup.toggling": "Đang bật/tắt %s...",
		"popup.close":    "Nhấn q hoặc Esc để đóng.",
		"popup.failed":   "Không bật/tắt được %s: %v",
		"popup.error":    "herdr-mm status gặp lỗi: %v",
		"truncated":      "\n… (đã cắt bớt)",
		"err.missing":    "thiếu %s: nhập bằng phím s trong popup trạng thái Mattermost, hoặc trong %s",
		"err.login":      "đăng nhập Mattermost thất bại, kiểm tra MM_URL và MM_BOT_TOKEN",
	},
	"en": {
		"idle": "idle", "done": "done", "working": "working", "blocked": "blocked",
		"unknown": "unknown", "noagent": "no agent", "off": "off", "closed": "closed",
		"root.off":            "_Mirroring stopped._",
		"root.closed":         "_Pane closed, mirroring stopped._",
		"notice.off":          "Mirroring stopped for",
		"notice.closed":       "Pane closed, mirroring stopped for",
		"dialog":              "@all ✋ **%s** is waiting on a dialog. Someone answer it on the machine:",
		"notice.started":      "🟢 herdr-mm connected",
		"notice.reconnected":  "🟡 herdr-mm reconnected",
		"notice.host":         "Host",
		"notice.panes":        "Mirroring",
		"notice.panecount":    "%d pane(s)",
		"notice.down":         "Connection lost",
		"plugin.off":          "@all ⚪ herdr is not running or the Mattermost plugin is disabled, so this was not typed into any agent and the bot has stopped listening. Enable the plugin, then toggle a pane or restart herdr to bring it back.",
		"prompt.failed":       "@all ❌ Could not prompt the agent: ",
		"prompt.blocked":      "@all ✋ The agent is waiting on a dialog. Approve or answer it on the machine, then reply again.",
		"prompt.late":         "📬 Delivered late: the bot was not connected when you sent this, so it was typed into the agent just now.",
		"prompt.received":     "📥 Received - the agent is working on it.",
		"root.channel":        "_@mention the bot in this channel to prompt the agent._",
		"channel.refused":     "You are not allowed to call me",
		"whitelist.prompt":    "Whitelist of %s, the Mattermost usernames the bot obeys there, comma or space separated: ",
		"whitelist.none":      "empty",
		"whitelist.unknown":   "unknown Mattermost user %s, the whitelist was not saved",
		"whitelist.saved":     "Whitelist saved.",
		"whitelist.nochannel": "this pane has no channel: press t to pick one first",
		"target.taken":        "channel %s is linked to another pane",
		"picker.loading":      "Loading the channels for %s...",
		"picker.title":        "Pick the channel to mirror %s into, among those the bot is in (one pane per channel)",
		"picker.unlink":       "Unlink the channel (stops mirroring)",
		"picker.none":         "The bot is in no free channel. Add it to a channel in Mattermost and try again.",
		"toggle.nochannel":    "this pane has no channel: open the Mattermost status popup, select the pane and press t to pick one first",
		"dm.refused":          "The bot only works in channels. Open the Mattermost status popup, select a pane, press t to pick its channel and Enter to mirror it, then @mention the bot there.",
		"picker.hint":         "- ↑/↓ or j/k: move\n- Enter: pick\n- q or Esc: cancel",
		"picker.linking":      "Linking %s to %s...",
		"picker.unlinking":    "Unlinking the channel of %s...",
		"settings.title":      "Mattermost settings. Press Enter to keep the current value.",
		"settings.url":        "Mattermost URL [%s]: ",
		"settings.token":      "Bot token [%s]: ",
		"settings.set":        "set",
		"settings.unset":      "not set",
		"settings.saved":      "Settings saved and the daemon restarted.",
		"help": "**Supported commands** (in a channel with a mirrored pane, after the bot's @mention, top-level or in any thread)\n" +
			"- Anything else: prompt the channel's agent.\n" +
			"- `#capture /command`: type the command, then post a screenshot of the pane, for example `#capture /context`.\n" +
			"- `#exec /command`: type exactly the command into the pane, without a screenshot, for example a skill, `/clear`, `/compact`.\n" +
			"- `list`: list the panes mirrored into this channel.\n" +
			"- `help`: show this list.\n\n" +
			"A message starting with `/` is taken by Mattermost as its own slash command, so use `#exec`.",
		"exec.usage":     "Usage: `#exec /command`, for example `#exec /clear`.",
		"list.none":      "No panes are mirrored. Open the Mattermost status popup, select a pane, press t to pick its channel and Enter to mirror it.",
		"popup.running":  "Mattermost daemon: running (pid %d)",
		"popup.stopped":  "Mattermost daemon: not running",
		"popup.none":     "No agent panes.",
		"popup.header":   " \tNAME\tAGENT\tSTATUS\tCONNECTION\tCHANNEL\tWHITELIST",
		"popup.yes":      "Connected",
		"popup.no":       "Not connected",
		"popup.channel":  "Channel:",
		"popup.allowed":  "Whitelist:",
		"popup.hint":     "- ↑/↓ or j/k: move\n- Enter or Space: toggle mirroring (picking a channel first when it has none)\n- t: pick or unlink its channel\n- w: edit its channel's whitelist\n- s: settings\n- l: switch to Tiếng Việt\n- q or Esc: close",
		"popup.toggling": "Toggling %s...",
		"popup.close":    "Press q or Esc to close.",
		"popup.failed":   "toggle %s: %v",
		"popup.error":    "herdr-mm status: %v",
		"truncated":      "\n… (truncated)",
		"err.missing":    "missing %s: set it with s in the Mattermost status popup, or in %s",
		"err.login":      "mattermost login failed, check MM_URL and MM_BOT_TOKEN",
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
