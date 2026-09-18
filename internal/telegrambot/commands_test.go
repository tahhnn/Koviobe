package telegrambot

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// Telegram validates every entry in setMyCommands and rejects the WHOLE call if
// any one is malformed — so a single bad description silently leaves the bot
// with no menu at all, which is exactly the symptom this list is meant to fix.
// Rules: name is 1-32 chars of [a-z0-9_]; description is 1-256 chars.
var namePattern = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

func TestCommandSpecsAreValidForTelegram(t *testing.T) {
	if len(commands) == 0 {
		t.Fatal("command list is empty")
	}
	seen := map[string]bool{}
	for _, c := range commands {
		if !namePattern.MatchString(c.Name) {
			t.Errorf("command %q: name must match [a-z0-9_]{1,32}", c.Name)
		}
		if strings.HasPrefix(c.Name, "/") {
			t.Errorf("command %q: setMyCommands takes the name without a leading slash", c.Name)
		}
		if n := len(c.Menu); n == 0 || n > 256 {
			t.Errorf("command %q: description length %d is outside 1-256", c.Name, n)
		}
		if seen[c.Name] {
			t.Errorf("command %q is listed twice", c.Name)
		}
		seen[c.Name] = true
	}
}

// Every advertised command must resolve to a handler. Checked by lookup rather
// than by calling: several handlers touch Postgres and Redis, which are not
// available in a unit test.
func TestEveryAdvertisedCommandResolves(t *testing.T) {
	for _, c := range commands {
		got, ok := byName[c.Name]
		if !ok {
			t.Errorf("/%s is in the menu but not in the dispatch table", c.Name)
			continue
		}
		if got.Run == nil {
			t.Errorf("/%s has no handler", c.Name)
		}
	}
	if s, ok := byName["start"]; !ok || s.Run == nil {
		t.Error("/start must resolve (Telegram sends it on first contact)")
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	if got := dispatch("/khongcolenhnay", nil, time.Now()); !strings.HasPrefix(got, "Lệnh không hợp lệ") {
		t.Errorf("expected a rejection, got: %s", got)
	}
}

func TestHelpListsEveryCommand(t *testing.T) {
	help := cmdHelp()
	for _, c := range commands {
		if !strings.Contains(help, "/"+c.Name) {
			t.Errorf("/help does not mention /%s", c.Name)
		}
	}
}
