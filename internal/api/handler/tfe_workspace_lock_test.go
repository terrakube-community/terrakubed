package handler

import (
	"strings"
	"testing"
)

func TestParseWorkspaceLockPath(t *testing.T) {
	cases := []struct {
		path      string
		wsID, act string
		ok        bool
	}{
		{"workspaces/ws-1/actions/lock", "ws-1", "lock", true},
		{"workspaces/ws-1/actions/unlock", "ws-1", "unlock", true},
		{"workspaces/ws-1/actions/force-unlock", "", "", false},
		{"workspaces/ws-1/configuration-versions", "", "", false},
		{"workspaces//actions/lock", "", "", false},
		{"workspaces", "", "", false},
	}
	for _, c := range cases {
		parts := strings.Split(c.path, "/")
		id, act, ok := parseWorkspaceLockPath(parts)
		if id != c.wsID || act != c.act || ok != c.ok {
			t.Errorf("%q: got (%q,%q,%v), want (%q,%q,%v)", c.path, id, act, ok, c.wsID, c.act, c.ok)
		}
	}
}
