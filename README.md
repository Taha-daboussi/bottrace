# bottrace

A local recording proxy for the DawnTrade Telegram bot. It records what the bot sends to Telegram, what Telegram answers, every user action, and every outside call (RPC, swap APIs), and turns a session into HTML pages: one page per screen, an index, and a timeline. It can also drive the bot: tap a button or send a message as the user, without Telegram Web.

The bot needs no code change. Every outside client of the bot uses `httpx.Transport()`, which reads `HTTPS_PROXY`, so the bot sends its HTTPS calls through bottrace when the variable is set. bottrace opens each call with its own local CA (the bot trusts it through `SSL_CERT_FILE`), records it, and sends it on to the real server. Without the two variables the bot runs as before; production never sets them.

This does not replace the live test in Telegram Web.

## Install

```sh
cd ~/sandbox/DawnTrade/bottrace && go build -o ~/.local/bin/bottrace .
```

## Record a session

```sh
bottrace rec -name my-check                     # terminal 1: keeps running
env $(bottrace env) devbot run <worktree>       # terminal 2: the bot on a devbot slot, through bottrace
```

Send any message to the bot once in Telegram, so bottrace learns your user. After that:

```sh
bottrace send /start           # a message from the user; prints the screens the bot drew
bottrace tap Positions         # tap a button of the newest bot message that has it
bottrace tap -data side:buy    # tap by callback data
bottrace screen -data          # the newest bot message, with each button's callback data
```

Ctrl-C in terminal 1 stops the recorder and renders the session. Restart the bot without the env line afterwards (`devbot run <worktree>`), or it cannot reach Telegram.

## Pages

```sh
bottrace render [session]      # default: the latest; works while rec runs too
bottrace diff <A> <B>          # the screens of two sessions step by step, e.g. main and a branch (see Diff)
bottrace ls
```

A session lives in `~/.local/share/bottrace/sessions/<date>-<name>/`:

- `index.html`: every screen drawn like Telegram (picture, formatted text, custom emoji, buttons with their style), plus the limit flags.
- `screens/NNN.html`, `screens/NNN.bare.html` and `screens/NNN.txt`: one screen, the message alone (for a screenshot compare), and the same screen as plain text (`[ ✦ Positions ]`, `✦` = a custom emoji icon, `| primary` = the button style).
- `screens.txt`: all screens as text, for grep and diffs.
- `timeline.html`: user actions, bot calls and their answers, toasts, deletes and outside calls in time order, with the bot's answer time per action and tx links. Outside calls that happen outside any user action (feeds, pollers) are hidden by default; a checkbox shows them.
- `events.jsonl`: the raw record.

For a before/after check, record the same steps on `main` and on the branch and run `bottrace diff`.

## Diff

```sh
bottrace diff <A> <B> [-live money,percent,time,number] [-live-re <regexp>] [-fail] [-by steps|screens|auto] [-out dir]
```

- Pairs by step: each user action (a message or a tap, from the bot side, so also a tap in Telegram Web) and the final state of each message it changed. A screen that draws first and fills in later, or a step that only one side has, does not shift the rest. The steps of the two sessions line up by their input (`send /start`, `tap goto:settings`). A session with no actions falls back to pairing by screen number (`-by screens`).
- Compares everything a test needs: the text, the formatting (bold, underline, links), the custom emoji ids, and for each button its label, target (callback data or URL), icon and style.
- Change kinds per screen: `text`, `format`, `emoji`, `media`, `buttons` (added, removed or renamed), `order`, `layout`, `callback`, `style`, `icon`, `live`.
- `-live`: values that change on their own between two runs (prices, clocks) count as `live`, not as a change. Off by default, because a real change of such a value (a fee, a limit) then hides there too. `diff.txt` still shows each live value as `~ old → new`.
- `-fail`: exit with 1 when a screen changed or a step is only on one side, for a script or an e2e gate.
- Writes `index.html` (side by side), `diff.txt` (line diff of the full screens) and `diff.json` (each step with its status and change kinds).
- A list of changes: each change has an id (`step.screen.n`), a kind and the exact lines on each side, in `diff.json` (`changes` of each screen) and as `» 3.1.2 callback: <before>  ⇒  <after>` lines in `diff.txt`. Text changes are groups of changed lines (`text`, `line-added`, `line-removed`, `format`, `emoji`, `live`); button changes match the buttons by label, so one added button gives one `button-added` and nothing else (`button-added`, `button-removed`, `label`, `callback`, `style`, `icon`, `order`, `layout`); also `media`, `screen-added`, `screen-removed`. A judge can answer per id and quote these lines instead of typing them again.
- `bottrace help diff` (also `check`, `snapshot`, `pages`) prints these docs from the README the binary was built with.

## Golden files

```sh
bottrace snapshot [session] [-step 2,5] [-o golden.json]   # the screens of each step, every field set
bottrace check golden.json [session] [-live money] [-json out.json]
```

- `snapshot` writes the expected screens of an approved session, step by step: the text, the markup (formatting tags and custom emoji ids), the media kind, and each button with its target, style and icon.
- `check` lines up the golden steps with the session steps by input and compares only the fields the file sets. `snapshot` writes `nth` for each step (the nth time its input comes, so `-step 9` of the 4th `/start` checks the 4th `/start`); a step with no `nth` matches the next step in order with its input. A session step that the file does not list is not checked; a listed step that the session does not have fails. In a step, the golden screens pair with the session's screens in order, and a screen that the step does not list fails as `extra`: list every screen of a step. Exit 1 on a difference, with a line diff of each failed screen.
- A golden file can be written by hand, for example from a design image. Leave out what the image cannot show (`data`, `url`, `icon`). Set `"style": ""` for a button with no color. Set `"emoji": "loose"` (for the file or one screen) when the emoji come from a picture: the check then leaves out emoji in the text and the labels (which code point, the variation selector, and whether it is in the label or a custom icon), and checks the rest exactly. Emoji then need a visual check (see bare pages).
- Bare pages: `render` also writes `screens/NNN.bare.html`, one message with no clock, ids or links. Two runs of the same screen give the same pixels, so a screenshot compare of two bare pages shows only real changes.

## Limit flags

Errors: callback data over 64 bytes, more than 8 buttons in a row or 100 in all, text over 4096 or a caption over 1024 characters, a button with no text, any call Telegram refused. Warnings: close to those limits, a label that may be cut on a phone, an edit that changed nothing (`message is not modified`).

## Secrets

Nothing is written before it goes through the scrubber:

- No headers are recorded. The bot token in a Telegram path becomes `/bot<id>:[hidden]`.
- Outside calls keep only the method, the host (a first label that looks like a key is hidden), the path (segments that look like a key become `…`), the query names without values, status, size, duration, the JSON-RPC method names, tx hashes and error messages. No bodies.
- Privy and auth hosts keep only the status and the size.
- In Telegram data, JSON fields named like a secret, 64-byte base58 strings and 64-char hex strings (unless seen as a tx id or inside a link), Solana byte-array keys, bot tokens and runs of 12+ BIP39 words become `[hidden]`.

The CA key is in `~/.local/state/bottrace/ca-key.pem` (mode 600). It is trusted only by a process started with the `SSL_CERT_FILE` of `bottrace env`.

## How the driver works

bottrace sits on the bot's `getUpdates`. A queued action ends the waiting long poll at once and is handed to the bot as an update, with a made-up callback query id (`bottrace-N`) or message id (from 1,900,000,000). The bot then works against the real backend and the real chat. Calls that Telegram would refuse for these made-up ids (`answerCallbackQuery`, `deleteMessage`, `setMessageReaction` on them, a reply to them) are answered by bottrace or have the reply removed; the timeline marks them.

The real driver is the user client (next section); this one needs no login and has no Telegram rate limit.

Update ids: the bot sees ids shifted by the number of injected updates, and bottrace maps its offsets back. The shift is kept in `~/.local/state/bottrace/bots/<bot id>.json`, so a bot that outlives a bottrace restart stays in step.

## User client

The injected driver above never reaches Telegram. The user client is a second, real driver: a Telegram account (gotd/td, layer 233) that sees each bot message as the account gets it and taps through Telegram, so the bot's real toast or alert text is recorded. The injected driver stays; the user client works only with `-user`.

One-time setup:

1. Make an app on my.telegram.org with the account. Write `~/.config/bottrace/telegram-app` (mode 600):

   ```
   api_id=1234567
   api_hash=0123456789abcdef0123456789abcdef
   ```

2. List the bot chats in `~/.config/bottrace/chats`, one per line: `@username rw` (stream, send, tap) or `@username ro` (stream only). An id after the mode pins the entry to that bot, so a lookalike with the same name cannot get in (`@cove_trading_bot ro 8495538877`). Only private chats with these bots are recorded; everything else is dropped before it is converted or written.
3. Run `bottrace user login` in your own terminal. Type the phone, the code from the Telegram app and, if Telegram asks, the two-step password. Do not paste the code into a chat: Telegram blocks a code that was sent in a message. The account gets a "bottrace" device in Settings > Devices.

Use:

```sh
bottrace rec -user                       # proxy + account side, one events.jsonl, one clock
bottrace rec -user -bot @dawn_test3_ugqr2_bot   # only this bot (one pane, one bot)
bottrace send -user /start               # sent through Telegram as the account
bottrace tap -user "Positions"           # real tap: messages.getBotCallbackAnswer
bottrace screen -user -bot @dawn_test3_ugqr2_bot
bottrace user status                     # account and allowlist
bottrace user logout                     # ends the session on Telegram and removes the file
```

`-bot @name` picks the bot when more than one fits. A tap prints the bot's answer (toast, alert or "no toast text"), then each message as the account got it, then the bot's calls when the bot runs behind the proxy. The user client also works with no bot behind the proxy, for example on the staging bot or a reference bot (read only).

Events: kind `tg_user`, methods `message`, `edit`, `delete` (stream) and `send`, `tap` (actions). A received message is stored in the Bot API shape (text, entities with custom emoji ids, `inline_keyboard` with callback data, URL, web app, switch inline, `style` and `icon_custom_emoji_id`, media with `file_unique_id` keys that start with `u:`), so the pages draw it like a bot-side screen. The timeline shows both sides in time order, with an "account side" filter. Screens and `diff` use the bot side; a bot with no bot side in the session gets "as received" screens instead. Message ids differ between the two sides (each account has its own ids in a private chat), so the two sides line up by time, not by id.

Safety:

- One `rec -user` at a time: Telegram pushes the updates of a login to one client only (tested: with two runs on one login, only the run that connected last got its bot's replies). A second `rec -user` stops at once and names the run that holds `~/.local/state/bottrace/user/rec.lock`. Use `-bot @name` so a run records only its own bot. A run never writes the session file; only `bottrace user login` does.
- The session file is the full key to the account. It lives in `~/.local/state/bottrace/user/` (folder 700, files 600; bottrace narrows a wider mode at start), never in a session folder, a page or a log. The gotd logger stays off.
- About 4 Telegram calls a second at most; a FLOOD_WAIT of up to 2 minutes is waited out and noted in the timeline, a longer one fails the call. The client never marks messages read and never sets the online status. Bot ids and access hashes are cached in `peers.json`, so `contacts.resolveUsername` runs once per bot.
- Real taps change the bot's state like a person would. Keep to the money rules of the test env.
- The account's own messages in an allowlisted chat (from bottrace or from another app such as Telegram Web) are recorded too, marked "sent by the account".

## Not seen

Clients that do not use the env proxy: the EVM log feed and Arbitrum sequencer websockets, pump.fun and fomo through their own proxies, `httpx.Public`, and gRPC streams (passed through unopened). Postgres and Dragonfly are not HTTP.
