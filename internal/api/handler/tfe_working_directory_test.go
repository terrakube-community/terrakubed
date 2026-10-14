package handler

import "testing"

func TestTFEWorkspaceResourceWorkingDirectory(t *testing.T) {
	attrs := func(wd string) map[string]interface{} {
		return tfeWorkspaceResource("id", "ws", false, nil, false, wd)["attributes"].(map[string]interface{})
	}

	if got := attrs("/environments/staging")["working-directory"]; got != "/environments/staging" {
		t.Errorf("working-directory = %v, want /environments/staging", got)
	}
	if _, present := attrs("")["working-directory"]; present {
		t.Error("working-directory must be omitted when the workspace has none")
	}
}
