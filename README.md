# herdr-mattermost

herdr plugin that mirrors agent panes to Mattermost and relays replies back.

Switch mirroring on for a pane and the bot opens a thread for it in your direct message with the bot:

- The thread's root post shows the agent's live status (🟢 idle, ⏳ working, ✅ done, ✋ blocked, ❔ unknown) and is edited in place.
- When the agent finishes a turn you started from the thread, its last reply is posted into the thread. If you send another reply while the agent is still answering, so it moves straight on without going idle, the reply to each of those turns is posted, oldest first. Turns you start by typing in the terminal stay off Mattermost: the plugin posts a reply only when one of the last five thread replies it typed in is one of the turn's prompts in the transcript, including a reply queued while the agent was still working. A turn a background task resumes after such a turn is posted too. A background task is matched to the latest prompt, not the one that started it, so a task started in the terminal that finishes after a thread reply can be posted by mistake, and one started from the thread that finishes after a terminal prompt can be kept off Mattermost.
- When the agent stops on an approval or question dialog in a turn you started from the thread, the dialog is posted into the thread and you are @mentioned; a dialog in a turn you typed in the terminal is not posted. Answer the dialog on the machine; approving from Mattermost is not supported.
- A reply you write in the thread is typed into that agent, like `herdr agent prompt`, and the bot acknowledges it in the thread right away. A reply sent while the bot was disconnected is typed in when it reconnects, with a note in the thread saying it was delivered late.
- Sending `list` in the DM (outside a thread) lists the mirrored panes and their statuses.

Only the one Mattermost user named in `MM_USER` is obeyed, and only in the bot's DM. Everyone else, the bot itself, and bot or webhook posts are ignored.

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

### 3. Write the config

```sh
cd "$(herdr plugin config-dir herdr-mattermost)"
cat > .env <<'EOF'
MM_URL=https://mattermost.example.com
MM_BOT_TOKEN=paste-the-bot-token-here
MM_USER=your-mattermost-username
EOF
chmod 600 .env
```

`MM_USER` is your own Mattermost username, the only person the bot obeys.

### 4. Add a keybinding (optional)

A plugin manifest cannot declare a default key, so bind the toggle action yourself in `~/.config/herdr/config.toml`:

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

Focus an agent pane and press the key, or run the **Toggle Mattermost mirroring** action from the pane's action menu. A new thread appears in your DM with the bot. Run the toggle again to stop mirroring; the root post is marked, the thread stops updating, and a new DM post says mirroring stopped for that pane, with a link to its thread, so you are notified. Closing the pane does the same, and the post says the pane was closed. Moving the pane to another workspace keeps its thread.

Run the **Show Mattermost status** action, or press its key, to open a popup that shows whether the daemon is running and lists every pane herdr reports an agent in, sorted by name, with its name, agent, status and whether it is mirrored. Panes in workspaces whose label starts with `└ `, the temporary ones firstmate opens for its workers, are left out, even when mirrored; switch those off with the toggle action on the pane. Move with the up and down arrows or `j` and `k`, and press `Enter` or `Space` to toggle mirroring of the selected pane, exactly as the toggle action does on that pane. The list refreshes after every key, and a failed toggle shows its error in the popup. Press `l` to switch the plugin's language between Vietnamese, the default, and English. Press `q` or `Esc` to close it. A pane's name is the one you gave it with herdr's pane rename, otherwise its workspace's label, followed by the tab's label when the workspace has more than one tab. Posts name a pane the same way rather than by its pane id; when several panes share a name, the popup and the `list` reply add ` #2`, ` #3` to the later ones in pane id order.

The language applies to everything the bot and the popup write themselves: root posts, acknowledgements, notices, errors, status words, the `list` reply, and the popup's columns and key hints. Agent replies are posted as written. It is saved in the `lang` file in the plugin state directory and read for every message, so the running daemon uses it right away. Posts already made stay as they are; a thread's root post switches at its next status update. `list` is the DM command in both languages.

The daemon that listens for your replies is started by herdr at startup and by every toggle, so there is nothing else to run. Only one daemon runs at a time. If Mattermost cannot be reached when it starts, it keeps retrying. Each time it connects, it posts a message in the DM saying whether the daemon just started or reconnected after the connection dropped. A herdr restart posts only when it has to start a new daemon; a daemon that is still running keeps its connection and posts nothing.

Before it acts on a message, the daemon checks that herdr is running and the plugin is still enabled. If not, it answers that nothing was typed into the agent and exits.

To stop the daemon, run the **Stop Mattermost daemon** action, or:

```sh
herdr plugin action invoke herdr-mattermost.stop
```

If the daemon is not answering, read its log:

```sh
tail ~/.local/state/herdr/plugins/herdr-mattermost/daemon.log
```

That is the plugin state directory herdr passes as `HERDR_PLUGIN_STATE_DIR`. The daemon refuses to start with a clear line when a `.env` key is missing or the bot token is rejected. After editing `.env`, run the stop action; the next toggle, or the next herdr start, starts the daemon again. If the plugin is already disabled and its actions are gone, use `pkill -f 'herdr-mm daemon'`.

`herdr plugin log list --plugin herdr-mattermost` shows the output of the toggle action and the event hooks.

## How it works

`herdr-mm` is one binary with six commands:

| Command | Run by | Does |
| --- | --- | --- |
| `start` | `[[startup]]` hook, and after each toggle | Launches `herdr-mm daemon` detached. |
| `daemon` | `start` | Holds the Mattermost WebSocket and types your thread replies into the agent with `herdr agent prompt`. A lock file keeps it to one instance. |
| `toggle` | the pane action | Creates the pane's root post, or marks it, posts a stop notice and stops mirroring. |
| `event` | `[[events]]` hooks for `pane.agent_status_changed`, `pane.moved` and `pane.closed` | Edits the root post and posts replies or dialogs for a mirrored pane, and a stop notice when it closes. A move to another workspace gives the pane a new id, so the thread is re-keyed to it. |
| `stop` | the stop action | Stops the daemon and waits for it to exit. |
| `status` | the `status` popup pane, opened by the status action | Lists the agent panes (`herdr agent list`) with their names and whether each is mirrored, and toggles the selected one on `Enter` or `Space` until `q` or `Esc`. |

Status changes arrive through the plugin `[[events]]` hook rather than the socket's `events.subscribe`. The hook already fires for every pane and needs no connection to keep alive, while a subscription is per pane and would have to be re-made whenever a pane is toggled, the daemon restarts, or herdr restarts. Hooks can run concurrently and late, so each one takes a lock on the state file, asks `herdr agent get` for the pane's current state instead of trusting the event, and skips replies and dialogs it has already posted.

Replies are read from the agent's Claude transcript, `~/.claude/projects/*/<session id>.jsonl` (`$CLAUDE_CONFIG_DIR` replaces `~/.claude` when set): for each turn since the last one handled, the text blocks of its newest assistant message outside subagents. Sharing a pane marks its newest turn as handled, so every turn is considered when it had none yet, and a transcript seen with no turns, like that of a new agent started in the pane, clears the mark so all its later turns are considered. So is every turn of a transcript `/clear` started, which opens with the `/clear` command. Otherwise, when the last turn handled is not in the transcript, as after resuming another session, only the newest turn is considered, so older history stays out of the thread. Resuming a session `/clear` started, or resuming one in a new agent before its first prompt, considers all that session's turns, so an old answer to a prompt matching one of the last five thread replies can be posted again. Posts are cut to Mattermost's 16383-character limit.

The pane-to-thread mapping lives in `panes.json` in the plugin state directory, so a restarted daemon keeps using the same threads. The `last_post` file next to it holds the time of the last DM message the daemon handled. Each time the WebSocket connects, the daemon fetches the DM messages sent since then, handles them oldest first, and skips any it has already handled.

## Limits and follow-ups

Not built yet:

- Approving or answering dialogs from Mattermost (with `herdr agent send-keys`).
- Reactions, and file attachments for output longer than one post.
- Agents other than Claude Code.
- Private channels, slash commands and webhooks.
- More than one machine, or more than one herdr session at a time: pane ids are not unique across sessions, and the single daemon prompts through the session that started it.
- Mirroring panes automatically.
