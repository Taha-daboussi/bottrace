package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// allowEntry is one bot chat that the user client may record. Write lets the client send and tap there; an id pins
// the entry to that exact bot, so a lookalike bot with the same name cannot get in.
type allowEntry struct {
	Username string
	Write    bool
	ID       int64
}

type allowlist []allowEntry

func configDir() string {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "bottrace")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "bottrace")
}

func allowlistPath() string { return filepath.Join(configDir(), "chats") }

// parseAllowlist reads lines of "@username rw|ro [id]". "#" starts a comment.
func parseAllowlist(text string) (allowlist, error) {
	var out allowlist
	for i, line := range strings.Split(text, "\n") {
		line, _, _ = strings.Cut(line, "#")
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 2 || len(f) > 3 || !strings.HasPrefix(f[0], "@") || len(f[0]) < 2 || (f[1] != "rw" && f[1] != "ro") {
			return nil, fmt.Errorf("line %d: want \"@username rw|ro [id]\", got %q", i+1, strings.TrimSpace(line))
		}
		e := allowEntry{Username: strings.ToLower(f[0][1:]), Write: f[1] == "rw"}
		if len(f) == 3 {
			id, err := strconv.ParseInt(f[2], 10, 64)
			if err != nil || id <= 0 {
				return nil, fmt.Errorf("line %d: %q is not a user id", i+1, f[2])
			}
			e.ID = id
		}
		out = append(out, e)
	}
	return out, nil
}

func loadAllowlist() (allowlist, error) {
	data, err := os.ReadFile(allowlistPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no chat allowlist at %s: write one line per bot, \"@username rw\" or \"@username ro\"", allowlistPath())
	}
	if err != nil {
		return nil, err
	}
	a, err := parseAllowlist(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", allowlistPath(), err)
	}
	if len(a) == 0 {
		return nil, fmt.Errorf("%s has no chat", allowlistPath())
	}
	return a, nil
}

func (a allowlist) byName(name string) (allowEntry, bool) {
	name = strings.ToLower(strings.TrimPrefix(name, "@"))
	for _, e := range a {
		if e.Username == name {
			return e, true
		}
	}
	return allowEntry{}, false
}

// match says whether a user is an allowlisted bot. Only bots match; an entry with an id matches only that id.
func (a allowlist) match(id int64, username string, isBot bool) (allowEntry, bool) {
	if !isBot {
		return allowEntry{}, false
	}
	e, ok := a.byName(username)
	if !ok || (e.ID != 0 && e.ID != id) {
		return allowEntry{}, false
	}
	return e, true
}
