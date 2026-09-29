# herdr-mattermost

herdr plugin that mirrors agent panes to Mattermost and relays replies back.

Switch mirroring on for a pane and the bot opens a thread for it in your direct message with the bot:

- The thread's root post shows the agent's live status (🟢 idle, ⏳ working, ✅ done, ✋ blocked, ❔ unknown) and is edited in place.
- When the agent finishes a turn, its last reply is posted into the thread.
- When the agent stops on an approval or question dialog, the dialog is posted into the thread and you are @mentioned. Answer the dialog on the machine; approving from Mattermost is not supported.
- A reply you write in the thread is typed into that agent, like `herdr agent prompt`.
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

Then run `herdr server reload-config`.

## Usage

Focus an agent pane and press the key, or run the **Toggle Mattermost mirroring** action from the pane's action menu. A new thread appears in your DM with the bot. Run the toggle again to stop mirroring; the root post is marked and the thread stops updating. Closing the pane does the same.

The daemon that listens for your replies is started by herdr at startup and by every toggle, so there is nothing else to run. Only one daemon runs at a time.

If the daemon is not answering, read its log:

```sh
tail ~/.local/state/herdr/plugins/herdr-mattermost/daemon.log
```

That is the plugin state directory herdr passes as `HERDR_PLUGIN_STATE_DIR`. The daemon refuses to start with a clear line when a `.env` key is missing or the bot token is rejected. After editing `.env`, stop it with `pkill -f 'herdr-mm daemon'`; the next toggle that switches a pane on, or the next herdr start, starts it again.

`herdr plugin log list --plugin herdr-mattermost` shows the output of the toggle action and the event hooks.

## How it works

`herdr-mm` is one binary with four commands:

| Command | Run by | Does |
| --- | --- | --- |
| `start` | `[[startup]]` hook, and after each toggle | Launches `herdr-mm daemon` detached. |
| `daemon` | `start` | Holds the Mattermost WebSocket and types your thread replies into the agent with `herdr agent prompt`. A lock file keeps it to one instance. |
| `toggle` | the pane action | Creates the pane's root post, or marks it and stops mirroring. |
| `event` | `[[events]]` hooks for `pane.agent_status_changed` and `pane.closed` | Edits the root post and posts replies or dialogs for a mirrored pane. |

Status changes arrive through the plugin `[[events]]` hook rather than the socket's `events.subscribe`. The hook already fires for every pane and needs no connection to keep alive, while a subscription is per pane and would have to be re-made whenever a pane is toggled, the daemon restarts, or herdr restarts. Hooks can run concurrently and late, so each one takes a lock on the state file, asks `herdr agent get` for the pane's current state instead of trusting the event, and skips replies and dialogs it has already posted.

The last reply is read from the agent's Claude transcript, `~/.claude/projects/<cwd with every non-alphanumeric character replaced by ->/<session id>.jsonl` (`$CLAUDE_CONFIG_DIR` replaces `~/.claude` when set): the text blocks of the newest assistant message outside subagents. Posts are cut to Mattermost's 16383-character limit.

The pane-to-thread mapping lives in `panes.json` in the plugin state directory, so a restarted daemon keeps using the same threads.

## Limits and follow-ups

Not built yet:

- Approving or answering dialogs from Mattermost (with `herdr agent send-keys`).
- Reactions, and file attachments for output longer than one post.
- Agents other than Claude Code.
- Private channels, slash commands and webhooks.
- More than one machine, or more than one herdr session at a time: pane ids are not unique across sessions, and the single daemon prompts through the session that started it.
- Mirroring panes automatically.
