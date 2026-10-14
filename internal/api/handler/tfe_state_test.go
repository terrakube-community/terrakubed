package handler

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/terrakube-community/terrakubed/internal/api/middleware"
)

func TestWorkspaceSubPath(t *testing.T) {
	cases := []struct {
		path, sub string
		id        string
		ok        bool
	}{
		{"workspaces/ws-1/current-state-version", "current-state-version", "ws-1", true},
		{"workspaces/ws-1/state-versions", "state-versions", "ws-1", true},
		{"workspaces/ws-1/state-versions", "current-state-version", "", false},
		{"workspaces//state-versions", "state-versions", "", false},
		{"workspaces/ws-1/state-versions/x", "state-versions", "", false},
	}
	for _, c := range cases {
		id, ok := workspaceSubPath(strings.Split(c.path, "/"), c.sub)
		if id != c.id || ok != c.ok {
			t.Errorf("%q/%q: got (%q,%v), want (%q,%v)", c.path, c.sub, id, ok, c.id, c.ok)
		}
	}
}

func TestDecodeStateAttributeToleratesLineBreaks(t *testing.T) {
	want := `{"serial":3,"lineage":"abc"}`
	enc := base64.StdEncoding.EncodeToString([]byte(want))
	wrapped := enc[:10] + "\r\n" + enc[10:20] + "\n" + enc[20:]
	got, err := decodeStateAttribute(wrapped)
	if err != nil || string(got) != want {
		t.Fatalf("got (%q, %v), want %q", got, err, want)
	}
	if _, err := decodeStateAttribute("not base64 !!"); err == nil {
		t.Fatal("expected an error for invalid base64")
	}
}

// Last gate before the live state is overwritten.
func TestParseStateSummary(t *testing.T) {
	good := `{"version":4,"terraform_version":"1.6.4","serial":12,"lineage":"l-1","outputs":{},"resources":[]}`
	s, err := parseStateSummary([]byte(good))
	if err != nil || s.Serial != 12 || s.Lineage != "l-1" || s.TerraformVersion != "1.6.4" {
		t.Fatalf("good state: got (%+v, %v)", s, err)
	}

	// serial 0 is legitimate and must not be confused with "missing".
	if _, err := parseStateSummary([]byte(`{"serial":0,"lineage":"x"}`)); err != nil {
		t.Errorf("serial 0 should be accepted: %v", err)
	}

	for name, bad := range map[string]string{
		"empty":          ``,
		"not json":       `hello`,
		"json array":     `[1,2]`,
		"missing serial": `{"lineage":"x","terraform_version":"1.6.4"}`,
		"empty object":   `{}`,
	} {
		if _, err := parseStateSummary([]byte(bad)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestCanManageStateWithoutDatabase(t *testing.T) {
	h := &RemoteTFEHandler{ownerGroup: "owners"} // nil pool: these cases must not reach the DB
	ctxFor := func(u *middleware.UserInfo) context.Context {
		return context.WithValue(context.Background(), middleware.ContextKeyUser, u)
	}

	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"no user", context.Background(), false},
		{"internal token", ctxFor(&middleware.UserInfo{Issuer: "TerrakubeInternal"}), true},
		{"owner group member", ctxFor(&middleware.UserInfo{Issuer: "dex", Groups: []string{"owners"}}), true},
		{"user with no groups", ctxFor(&middleware.UserInfo{Issuer: "dex"}), false},
		// A PAT is NOT exempt just for being a PAT: with no groups it is denied.
		{"PAT without groups", ctxFor(&middleware.UserInfo{Issuer: "Terrakube"}), false},
	}
	for _, c := range cases {
		if got := h.canManageState(c.ctx, "org", "ws"); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
