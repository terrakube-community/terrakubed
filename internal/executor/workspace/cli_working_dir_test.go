package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveCLIWorkingDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "environments", "staging"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "afile"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct{ name, folder, want string }{
		{"repo root uploaded, folder present", "/environments/staging", filepath.Join(root, "environments", "staging")},
		{"no leading slash", "environments/staging", filepath.Join(root, "environments", "staging")},
		{"first of a comma list", "/environments/staging, /other", filepath.Join(root, "environments", "staging")},
		{"only subdir uploaded (folder absent)", "/does/not/exist", root},
		{"folder is a file", "/afile", root},
		{"empty", "", root},
		{"slash", "/", root},
		{"escapes the archive", "/../../etc", root},
	}
	for _, c := range cases {
		if got := resolveCLIWorkingDir(root, c.folder); got != c.want {
			t.Errorf("%s: resolveCLIWorkingDir(%q) = %q, want %q", c.name, c.folder, got, c.want)
		}
	}
}
