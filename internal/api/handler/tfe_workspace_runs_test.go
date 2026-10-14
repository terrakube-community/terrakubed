package handler

import (
	"strings"
	"testing"
)

func TestParseWorkspaceRunsPath(t *testing.T) {
	cases := []struct {
		path string
		id   string
		ok   bool
	}{
		{"workspaces/ws-1/runs", "ws-1", true},
		{"workspaces/ws-1/actions/lock", "", false},
		{"workspaces//runs", "", false},
		{"workspaces/ws-1/runs/extra", "", false},
		{"workspaces", "", false},
	}
	for _, c := range cases {
		id, ok := parseWorkspaceRunsPath(strings.Split(c.path, "/"))
		if id != c.id || ok != c.ok {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", c.path, id, ok, c.id, c.ok)
		}
	}
}
