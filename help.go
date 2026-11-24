package main

import (
	_ "embed"
	"fmt"
	"strings"
)

//go:embed README.md
var readme string

var helpSections = map[string]string{"diff": "Diff", "check": "Golden files", "snapshot": "Golden files", "pages": "Pages"}

// helpTopic prints a README section, so the docs an agent reads always match the binary it runs.
func helpTopic(topic string) error {
	name, ok := helpSections[topic]
	if !ok {
		return fmt.Errorf("help: no topic %q (diff, check, snapshot, pages)", topic)
	}
	_, rest, found := strings.Cut(readme, "\n## "+name+"\n")
	if !found {
		return fmt.Errorf("help: the README has no %q section", name)
	}
	if i := strings.Index(rest, "\n## "); i >= 0 {
		rest = rest[:i]
	}
	fmt.Printf("## %s\n%s\n", name, strings.TrimRight(rest, "\n"))
	return nil
}
