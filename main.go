package main

import (
	"fmt"
	"os"
	"path/filepath"
)

const usage = `bottrace: record, drive and render a Telegram bot session

  bottrace rec [-name n] [-addr host:port] [-control host:port] [-tunnel hosts]
      start the recording proxy; start the bot with the env line it prints
  bottrace env                     print the env line for the bot
  bottrace tap [-chat id] [-msg id] [-data cb] [label]
      tap a button of the newest bot message that has it, wait, print the new screens
  bottrace send [-chat id] text    send a message to the bot as the user, wait, print the new screens
  bottrace screen [-chat id]       print the newest bot message of the chat
  bottrace render [session]        write screens/, index.html, timeline.html (default: latest)
  bottrace diff <session A> <session B> [-live money,percent,time,number] [-fail] [-by steps|screens] [-out dir]
      compare two sessions step by step: text, format, emoji ids, buttons, targets, icons, styles
  bottrace snapshot [session] [-step 2,5] [-o golden.json]
      write the expected screens of each step as a golden file (every field set; edit it, or write one by hand)
  bottrace check <golden.json> [session] [-live money,...] [-json out.json]
      check a session against a golden file; only the fields the file sets; exit 1 on a difference
  bottrace ls                      list the sessions

User client (the real Telegram account; see README "User client"):
  bottrace user login|status|logout   log the account in one time, show it, end the session
  bottrace rec -user [-bot @a,@b] ... also record what the allowlisted bot chats get (or only these), as the account got it
  bottrace send -user [-bot @name] text
  bottrace tap -user [-bot @name] [-msg id] [-data cb] [label]
  bottrace screen -user [-bot @name]  the same commands, through the real Telegram
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	args := os.Args[2:]
	var err error
	switch os.Args[1] {
	case "rec":
		err = cmdRec(args)
	case "env":
		err = cmdEnv()
	case "tap":
		err = cmdTap(args)
	case "send":
		err = cmdSend(args)
	case "screen":
		err = cmdScreen(args)
	case "render":
		err = cmdRender(args)
	case "diff":
		err = cmdDiff(args)
	case "snapshot":
		err = cmdSnapshot(args)
	case "check":
		err = cmdCheck(args)
	case "ls":
		err = cmdList()
	case "user":
		err = cmdUser(args)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "bottrace:", err)
		os.Exit(1)
	}
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "bottrace")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "bottrace")
}

func dataDir() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "bottrace")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "bottrace")
}

func sessionsDir() string { return filepath.Join(dataDir(), "sessions") }
