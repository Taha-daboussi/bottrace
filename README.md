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
bottrace diff <A> <B>          # screens of two sessions side by side, e.g. main and a branch
bottrace ls
```

A session lives in `~/.local/share/bottrace/sessions/<date>-<name>/`:

- `index.html`: every screen drawn like Telegram (picture, formatted text, custom emoji, buttons with their style), plus the limit flags.
- `screens/NNN.html` and `screens/NNN.txt`: one screen, and the same screen as plain text (`[ ✦ Positions ]`, `✦` = a custom emoji icon, `| primary` = the button style).
- `screens.txt`: all screens as text, for grep and diffs.
- `timeline.html`: user actions, bot calls and their answers, toasts, deletes and outside calls in time order, with the bot's answer time per action and tx links. Outside calls that happen outside any user action (feeds, pollers) are hidden by default; a checkbox shows them.
- `events.jsonl`: the raw record.

For a before/after check, record the same steps on `main` and on the branch and run `bottrace diff`.

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

Update ids: the bot sees ids shifted by the number of injected updates, and bottrace maps its offsets back. The shift is kept in `~/.local/state/bottrace/bots/<bot id>.json`, so a bot that outlives a bottrace restart stays in step.

## Not seen

Clients that do not use the env proxy: the EVM log feed and Arbitrum sequencer websockets, pump.fun and fomo through their own proxies, `httpx.Public`, and gRPC streams (passed through unopened). Postgres and Dragonfly are not HTTP.
