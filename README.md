# herdr-mattermost

herdr plugin that mirrors agent panes to Mattermost and relays replies back.

The bot works only in channels. Link a pane to a channel the bot is in (see [Channels](#channels)) and switch mirroring on, and the bot opens a thread for it in that channel:

- The thread's root post shows the agent's live status (🟢 idle, ⏳ working, ✅ done, ✋ blocked, ❔ unknown) and is edited in place.
- When the agent finishes a turn started from Mattermost, its last reply is posted, @mentioning whoever asked, into the thread of the latest post (see [Channels](#channels)). If you send another post while the agent is still answering, so it moves straight on without going idle, the reply to each of those turns is posted, oldest first. Turns you start by typing in the terminal stay off Mattermost: the plugin posts a reply only when one of the last five Mattermost posts it typed in is one of the turn's prompts in the transcript, including a post queued while the agent was still working. A turn a background task resumes after such a turn is posted too. A background task is matched to the latest prompt, not the one that started it, so a task started in the terminal that finishes after a Mattermost post can be posted by mistake, and one started from Mattermost that finishes after a terminal prompt can be kept off Mattermost.
- When the agent stops on an approval or question dialog in a turn started from Mattermost, the dialog is posted into the thread of the latest post with `@all`, so whoever is at the machine answers it; a dialog in a turn you typed in the terminal is not posted. Answer the dialog on the machine; approving from Mattermost is not supported.
- A post that @mentions the bot in the channel, from someone on the channel's whitelist, top-level or in any thread, is typed into that agent, like `herdr agent prompt`, and the bot acknowledges it right away. A post sent while the bot was disconnected is typed in when it reconnects, with a note saying it was delivered late.
- A post starting with `#capture`, after the mention, also posts a screenshot of the pane. See [Screenshots](#screenshots).
- A post starting with `#exec`, after the mention, types the rest exactly as written, such as `@herdr #exec /clear` or `@herdr #exec /compact`, with the same acknowledgement and reply posting as a normal post and no screenshot. Mattermost takes a message starting with `/` as its own slash command and never posts it, so `#exec` is how a slash command reaches the agent. `#exec` on its own gets a usage line.
- `@herdr list` lists the panes mirrored into the channel and their statuses, with a link to each thread.
- `@herdr help` gets the list of commands.

All of these, `list` and `help` included, work only in a channel with a mirrored pane.

Each linked channel has its own whitelist of Mattermost users, set in the status popup (see [Whitelist](#whitelist)). Only they are obeyed there, and only when they @mention the bot. The agents run on your machine without asking, so anyone the bot obeys can run anything there: whitelist only people you trust. Someone else who @mentions the bot in a channel with a mirrored pane gets one reply, @mentioning them, saying they are not allowed to control the agent, and nothing is typed; with an empty whitelist nobody is obeyed and the reply says so. The bot itself, and bot, webhook or system posts, such as a channel header change, are ignored. A direct message to the bot, from anyone, is not typed into any agent: the bot answers it with one line saying it only works in channels.

The bot talks to Mattermost over REST and an outbound WebSocket, so it works from behind NAT (for example WSL): no incoming or outgoing webhooks, and no slash commands. Reading replies from the transcript supports Claude Code agents only.

## Setup

### 1. Create the bot account

1. In Mattermost, open **System Console > Integrations > Bot Accounts** and turn on **Enable Bot Account Creation**.
2. Open **Integrations > Bot Accounts > Add Bot Account**, give it a username such as `herdr`, and create it.
3. Copy the access token it shows. It is shown only once.

### 2. Install the plugin

The build needs Go 1.25 or newer.

```sh
herdr plugin install ThanhTri98/herdr-mattermost
```

Or, from a local checkout:

```sh
go build -o herdr-mm .
herdr plugin link "$PWD"
```

`plugin link` does not run the build step, so rebuild `herdr-mm` yourself after pulling changes.

### 3. Enter the settings

Open the status popup (see [Usage](#usage)) and press `s`. Enter the Mattermost URL and the bot token; press `Enter` on a line to keep its current value. The token is shown while you type it, but never printed afterwards. The values are saved in `settings.json` in the plugin config directory, `herdr plugin config-dir herdr-mattermost`, with mode 600, and the daemon is restarted so it uses them.

Or write them in a `.env` file in that directory instead:

```sh
cd "$(herdr plugin config-dir herdr-mattermost)"
cat > .env <<'EOF'
MM_URL=https://mattermost.example.com
MM_BOT_TOKEN=paste-the-bot-token-here
EOF
chmod 600 .env
```

Values saved from the popup win; `.env` fills the ones not saved. One bot token serves every channel.

### 4. Add a keybinding (optional)

A plugin manifest cannot declare a default key, so bind the toggle action yourself (it needs the pane to have a channel already; see [Channels](#channels)) in `~/.config/herdr/config.toml`:

```toml
[[keys.command]]
key = "prefix+m"
type = "plugin_action"
command = "herdr-mattermost.toggle"
description = "toggle Mattermost mirroring"
```

To open the status popup with `prefix+shift+m`, add:

```toml
[[keys.command]]
key = "prefix+shift+m"
type = "plugin_action"
command = "herdr-mattermost.status"
description = "show Mattermost status"
```

Then run `herdr server reload-config`.

## Usage

Focus an agent pane and press the key, or run the **Toggle Mattermost mirroring** action from the pane's action menu. A new thread appears in the pane's channel. A pane with no channel is not mirrored: the action fails with a line saying to pick one first with `t` in the status popup. Run the toggle again to stop mirroring; the root post is marked, the thread stops updating, and a new post in the same place says mirroring stopped for that pane, with a link to its thread, so you are notified. Closing the pane does the same, and the post says the pane was closed. Moving the pane to another workspace keeps its thread.

Run the **Show Mattermost status** action, or press its key, to open a popup that shows whether the daemon is running and lists every pane herdr reports an agent in, sorted by name, with its name, agent, status and whether it is mirrored. An open pane still linked to a channel is listed too after its agent exits, with the status `no agent`, so `t` can unlink it and free the channel. Panes in workspaces whose label starts with `└ `, the temporary ones firstmate opens for its workers, are left out, even when mirrored; switch those off with the toggle action on the pane. Move with the up and down arrows or `j` and `k`, and press `Enter` or `Space` to toggle mirroring of the selected pane, exactly as the toggle action does on that pane. On a pane with no channel, `Enter` or `Space` opens the channel picker first and mirrors the pane once you pick one. The list refreshes after every key, and a failed toggle shows its error in the popup. Press `t` to pick or unlink the selected pane's channel, shown in the CHANNEL column; see [Channels](#channels). Press `w` to edit the whitelist of the selected pane's channel, shown in the WHITELIST column, the first three names followed by how many more, or `?` when `whitelists.json` cannot be read; see [Whitelist](#whitelist). Press `s` to enter the settings. Press `l` to switch the plugin's language between Vietnamese, the default, and English. Press `q` or `Esc` to close it. A pane's name is the one you gave it with herdr's pane rename, otherwise its workspace's label, followed by the tab's label when the workspace has more than one tab. Posts name a pane the same way rather than by its pane id. When several panes share a name, ` #2`, ` #3` are added to the later ones in pane id order, counting every pane herdr reports an agent in, every open pane linked to a channel and every mirrored pane, so a pane has the same label in the popup and in every post. A number can change as panes come and go; a thread's root post picks it up at its next update.

The language applies to everything the bot and the popup write themselves: root posts, acknowledgements, notices, errors, status words, the `list` reply, and the popup's columns and key hints. Agent replies are posted as written. It is saved in the `lang` file in the plugin state directory and read for every message, so the running daemon uses it right away. Posts already made stay as they are; a thread's root post switches at its next status update. `list` and `help` are the commands in both languages.

### Channels

A pane is mirrored into one channel. Add the bot to the channel in Mattermost, open the status popup, select the pane and press `t`. The picker lists the public channels, marked `#`, and the private ones, marked `🔒`, that the bot is in, such as `# test` or `🔒 herdr-mattermost-plugins`, named after their team when the bot is in more than one. Each team's Town Square and Off-Topic are left out, by their fixed channel names, whatever they are called. A channel holds one pane: a channel already linked to another pane is not offered, and if `targets.json` in the state directory ever links two panes to one channel, the first in pane id order keeps it and the other loses its channel. A pane that has a channel also gets an unlink line at the end of the picker: it removes the pane's channel and stops its mirroring, with the usual stop notice in the channel. A new pane has no channel. Picking a new channel for a mirrored pane stops its thread where it was, with the usual stop notice there, and starts a new thread in the new channel, unless its agent has exited, when mirroring stays off. The choice is kept when mirroring is switched off, follows the pane when it moves to another workspace, and is forgotten when the pane closes.

Earlier versions could mirror a pane into the DM with the bot. Such a pane reads as switched off, without a post; select it in the status popup and press `Enter` to mirror it again, which picks a channel first when it has none.

Only posts that @mention the bot from someone on the channel's whitelist are typed into the agent, top-level or in any thread of the channel, including the pane's own thread. The mention is removed before typing. A top-level mention is answered in its own thread, and a mention in a thread is answered in that thread: the acknowledgement, then the agent's reply, which @mentions the person who asked. Posts that need someone to act tag `@all` in the channel: the dialog alert, posted in the thread of the last question, the replies saying a post could not be typed in, the agent is waiting on a dialog or herdr or the plugin is off, and a screenshot that could not be taken or posted. The connect and reconnect notices in Town Square tag nobody, as Town Square holds the whole team. Posts without the mention are ignored. Anyone not on the whitelist who @mentions the bot there gets one reply saying they are not allowed, and nothing is typed; `help`, `list`, `#capture` and `#exec` follow the same rule. Mentions sent while the bot was disconnected are handled when it reconnects, but not those sent before the pane's thread was started in the channel, and a channel the bot can no longer read is skipped. `#capture` and `#exec` go after the mention, such as `@herdr #capture /context`; the screenshot is posted into the thread of that post.

### Whitelist

Each channel has its own whitelist: someone whitelisted in one channel is refused in another. After you pick a channel with `t`, the popup asks for its whitelist: type Mattermost usernames, separated by commas or spaces, with or without `@`. `Enter` keeps the current list and `-` empties it. Press `w` on a pane with a channel to edit it later. Every name is looked up in Mattermost when you save; if one is unknown, the popup says which and nothing is saved, and a pane that `Enter` was about to mirror stays linked but off. The list is saved in `whitelists.json` in the plugin state directory, keyed by channel id, so it stays with the channel when another pane is linked to it. The daemon reads it for every post, so an edit applies at once. A new channel's whitelist is empty, so nobody controls its pane until you set it. `MM_USER`, which earlier versions obeyed alone, is no longer read.

### Screenshots

Some output is only drawn in the terminal and never reaches the transcript, such as Claude's `/context`. To see it, @mention the bot in the pane's channel, top-level or in any thread, with `#capture` followed by what to type:

```
@herdr #capture /context
```

The rest of the message is typed into the agent exactly like a normal post, with the same acknowledgement, and a reply the agent writes for it is still posted. Then the bot waits until the agent is no longer working and the screen has not changed for 2 seconds, or until the agent is blocked on a dialog, and posts a PNG of the pane's visible screen into the thread. If the screen is still changing after 60 seconds, it posts the screen as it is and says so. The screenshot is taken even when typing fails, for example while a dialog is open, so it shows why. `#capture` on its own types nothing and posts the screen once it has settled. When the text is `/status`, `/stats` or `/usage`, with or without arguments, the bot then presses Esc in the pane to close the panel Claude leaves open, but only when herdr reports the agent idle or done. It presses nothing while the agent is working, blocked on a dialog or in a status herdr cannot tell, where Esc could interrupt it or reject the dialog.

The image is drawn by the plugin, with colours, bold and wide characters, from the first monospace font it finds: DejaVu Sans Mono, Ubuntu Mono or Liberation Mono on Linux, Menlo or Monaco on macOS. Characters that font lacks are taken from DejaVu Sans, Noto Sans Symbols, Noto Sans Symbols 2 or Noto Sans CJK when installed (Apple Symbols or Arial Unicode on macOS), shrunk to fit their cell when wider. `/context`'s ⛶ and ⛝, which few fonts have, are drawn as □ and ⊠ when no font has them; other missing characters show as boxes, and emoji are drawn in one colour. Without any monospace font, the bot posts an error naming the fonts it looked for instead.

The daemon that listens for your replies is started by herdr at startup and by every toggle, so there is nothing else to run. Only one daemon runs at a time. If Mattermost cannot be reached when it starts, it keeps retrying. Each time it connects, it posts a message in the Town Square of the bot's first team by name saying whether the daemon just started or reconnected after the connection dropped. Town Square is still left out of the `t` picker. A herdr restart posts only when it has to start a new daemon; a daemon that is still running keeps its connection and posts nothing.

Before it acts on a message, the daemon checks that herdr is running and the plugin is still enabled. If not, it answers that nothing was typed into the agent and exits.

To stop the daemon, run the **Stop Mattermost daemon** action, or:

```sh
herdr plugin action invoke herdr-mattermost.stop
```

If the daemon is not answering, read its log:

```sh
tail ~/.local/state/herdr/plugins/herdr-mattermost/daemon.log
```

That is the plugin state directory herdr passes as `HERDR_PLUGIN_STATE_DIR`. The daemon refuses to start with a clear line when a setting is missing or the bot token is rejected. Saving the settings from the popup restarts it. After editing `.env`, run the stop action; the next toggle, or the next herdr start, starts the daemon again. If the plugin is already disabled and its actions are gone, use `pkill -f 'herdr-mm daemon'`.

`herdr plugin log list --plugin herdr-mattermost` shows the output of the toggle action and the event hooks.

## How it works

`herdr-mm` is one binary with six commands:

| Command | Run by | Does |
| --- | --- | --- |
| `start` | `[[startup]]` hook, and after each toggle | Launches `herdr-mm daemon` detached. |
| `daemon` | `start` | Holds the Mattermost WebSocket and types the whitelisted users' @mentions of the bot into the agent with `herdr agent prompt`. For `#capture` it reads the screen with `herdr pane read --source visible --format ansi` and uploads the PNG with Mattermost's file API, then closes the panel some slash commands leave open with `herdr agent send-keys <pane> esc`. A lock file keeps it to one instance. |
| `toggle` | the pane action | Creates the pane's root post, or marks it, posts a stop notice and stops mirroring. |
| `event` | `[[events]]` hooks for `pane.agent_status_changed`, `pane.moved` and `pane.closed` | Edits the root post and posts replies or dialogs for a mirrored pane, and a stop notice when it closes. A move to another workspace gives the pane a new id, so the thread is re-keyed to it. |
| `stop` | the stop action | Stops the daemon and waits for it to exit. |
| `status` | the `status` popup pane, opened by the status action | Lists the agent panes (`herdr agent list`), and the open panes still linked to a channel, with their names, whether each is mirrored and where, toggles the selected one on `Enter` or `Space`, picks or unlinks its channel on `t` from the bot's teams and channels (`GET /users/me/teams` and `/users/me/teams/{id}/channels`), edits the channel's whitelist after `t` and on `w`, looking each name up with `GET /users/username/{name}`, and edits the settings on `s`, until `q` or `Esc`. |

Status changes arrive through the plugin `[[events]]` hook rather than the socket's `events.subscribe`. The hook already fires for every pane and needs no connection to keep alive, while a subscription is per pane and would have to be re-made whenever a pane is toggled, the daemon restarts, or herdr restarts. Hooks can run concurrently and late, so each one takes a lock on the state file, asks `herdr agent get` for the pane's current state instead of trusting the event, and skips replies and dialogs it has already posted.

Replies are read from the agent's Claude transcript, `~/.claude/projects/*/<session id>.jsonl` (`$CLAUDE_CONFIG_DIR` replaces `~/.claude` when set): for each turn since the last one handled, the text blocks of its newest assistant message outside subagents. Claude can write a turn's last entry just after herdr reports idle, so when the turn typed from Mattermost has no reply in the transcript yet, or ends in a tool call or result with no text after it, it is read again every 100 ms for up to 2 seconds. Sharing a pane marks its newest turn as handled, so every turn is considered when it had none yet, and a transcript seen with no turns, like that of a new agent started in the pane, clears the mark so all its later turns are considered. So is every turn of a transcript `/clear` started, which opens with the `/clear` command. Otherwise, when the last turn handled is not in the transcript, as after resuming another session, only the newest turn is considered, so older history stays out of the thread. Resuming a session `/clear` started, or resuming one in a new agent before its first prompt, considers all that session's turns, so an old answer to a prompt matching one of the last five Mattermost posts typed in can be posted again. Posts are cut to Mattermost's 16383-character limit.

The pane-to-thread mapping lives in `panes.json` in the plugin state directory, so a restarted daemon keeps using the same threads, and each pane's channel, by id, in `targets.json` next to it. The `last_post` file holds the time of the last message the daemon handled. Each time the WebSocket connects, the daemon fetches the messages sent since then in every channel a mirrored pane's thread is in, handles them oldest first, and skips any it has already handled.

## Limits and follow-ups

Not built yet:

- Approving or answering dialogs from Mattermost (with `herdr agent send-keys`).
- Reactions, and file attachments for output longer than one post.
- Screenshots of the scrollback, or triggered by anything but a `#capture` message.
- Agents other than Claude Code.
- A read-only mode for people not on a channel's whitelist, and `@all` or `@channel` as a mention of the bot.
- A reply is tagged with the asker of the latest question, so a reply to an earlier question, still being worked on, tags the later asker.
- In a channel, a turn is answered in the thread of the latest question, so a reply to an earlier question from another thread, still being worked on, lands in the newer thread.
- Slash commands and webhooks.
- More than one machine, or more than one herdr session at a time: pane ids are not unique across sessions, and the single daemon prompts through the session that started it.
- Mirroring panes automatically.
