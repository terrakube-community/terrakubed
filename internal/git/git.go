package git

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// redactURL masks any embedded userinfo (user:token@ or token@) in a git
// remote URL so it's safe to log — used only for diagnostics, never for the
// actual command.
func redactURL(u string) string {
	idx := strings.Index(u, "://")
	if idx < 0 {
		return u
	}
	rest := u[idx+3:]
	at := strings.Index(rest, "@")
	if at < 0 {
		return u
	}
	return u[:idx+3] + "REDACTED@" + rest[at+1:]
}

type GitService interface {
	CloneRepository(source, version, vcsType, connectionType, accessToken, tagPrefix, folder string) (string, error)
	// ListRemoteTags returns tag name -> commit SHA for the given repository
	// without cloning it (`git ls-remote --tags`).
	ListRemoteTags(source, vcsType, connectionType, accessToken string) (map[string]string, error)
}

type Service struct{}

func NewService() *Service {
	return &Service{}
}

// setupCredentialURL injects the appropriate credential format based on VCS type.
func setupCredentialURL(source, vcsType, connectionType, accessToken string) string {
	if accessToken == "" || vcsType == "PUBLIC" || strings.HasPrefix(vcsType, "SSH") {
		return source
	}

	if !strings.HasPrefix(source, "https://") {
		return source
	}

	var user string
	switch vcsType {
	case "GITHUB":
		if connectionType == "OAUTH" {
			return strings.Replace(source, "https://", fmt.Sprintf("https://%s@", accessToken), 1)
		}
		user = "x-access-token"
	case "BITBUCKET":
		user = "x-token-auth"
	case "GITLAB":
		user = "oauth2"
	case "AZURE_DEVOPS":
		user = "dummy"
	default:
		user = "oauth2"
	}

	return strings.Replace(source, "https://", fmt.Sprintf("https://%s:%s@", user, accessToken), 1)
}

// setupSSHEnv prepares SSH environment for git clone when using SSH keys.
func setupSSHEnv(vcsType, accessToken, tempDir string) ([]string, func(), error) {
	cleanup := func() {}
	env := os.Environ()

	if !strings.HasPrefix(vcsType, "SSH") || accessToken == "" {
		return env, cleanup, nil
	}

	parts := strings.SplitN(vcsType, "~", 2)
	keyName := "id_rsa"
	if len(parts) == 2 && parts[1] != "" {
		keyName = parts[1]
	}

	sshDir := filepath.Join(tempDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return nil, cleanup, fmt.Errorf("failed to create SSH dir: %w", err)
	}

	keyPath := filepath.Join(sshDir, keyName)
	if err := os.WriteFile(keyPath, []byte(accessToken), 0600); err != nil {
		return nil, cleanup, fmt.Errorf("failed to write SSH key: %w", err)
	}

	cleanup = func() { os.RemoveAll(sshDir) }

	sshCmd := fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null", keyPath)
	env = append(env, "GIT_SSH_COMMAND="+sshCmd)

	return env, cleanup, nil
}

func (s *Service) CloneRepository(source, version, vcsType, connectionType, accessToken, tagPrefix, folder string) (string, error) {
	tempDir, err := os.MkdirTemp("", "terrakube-registry")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	repoURL := setupCredentialURL(source, vcsType, connectionType, accessToken)

	env, sshCleanup, err := setupSSHEnv(vcsType, accessToken, tempDir)
	if err != nil {
		return "", err
	}
	defer sshCleanup()

	// version can already carry a leading "v": when a module has no
	// tag_prefix configured, ModuleRefreshScheduler stores the full tag name
	// (including any "v") verbatim as the version string. Stripping it before
	// guessing keeps the two attempts below meaningfully distinct — without
	// this, a version of "v0.10.2" turned both attempts into the same
	// doomed "vv0.10.2"/"v0.10.2" pair instead of trying "v0.10.2" and
	// "0.10.2".
	bareVersion := strings.TrimPrefix(version, "v")

	log.Printf("CloneRepository diagnostics: source=%q repoURL=%q vcsType=%q tagPrefix=%q version=%q bareVersion=%q",
		source, redactURL(repoURL), vcsType, tagPrefix, version, bareVersion)

	// Try tag with "v" prefix first, then without
	tag := tagPrefix + "v" + bareVersion
	cloneCmd := exec.Command("git", "clone", "--depth", "1", "--branch", tag, repoURL, tempDir)
	cloneCmd.Env = env

	if output, err := cloneCmd.CombinedOutput(); err != nil {
		log.Printf("CloneRepository: first attempt (tag=%q) failed: %s", tag, string(output))
		tag = tagPrefix + bareVersion
		os.RemoveAll(tempDir)
		tempDir, _ = os.MkdirTemp("", "terrakube-registry")
		cloneCmd = exec.Command("git", "clone", "--depth", "1", "--branch", tag, repoURL, tempDir)
		cloneCmd.Env = env
		if output, err := cloneCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git clone failed for tag %s: %s: %w", tag, string(output), err)
		}
	}

	if folder != "" {
		return filepath.Join(tempDir, folder), nil
	}

	return tempDir, nil
}

// ListRemoteTags lists tags on a remote repository without cloning it,
// mirroring `git ls-remote --tags`. For an annotated tag, `git ls-remote`
// reports two lines — the tag object's own SHA, and a peeled "<tag>^{}" line
// with the commit SHA the tag actually points at. We keep the peeled commit
// SHA when present (it's the one that matters for identifying the commit),
// falling back to the tag object SHA for lightweight tags.
func (s *Service) ListRemoteTags(source, vcsType, connectionType, accessToken string) (map[string]string, error) {
	tempDir, err := os.MkdirTemp("", "terrakube-lsremote")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tempDir)

	repoURL := setupCredentialURL(source, vcsType, connectionType, accessToken)

	env, sshCleanup, err := setupSSHEnv(vcsType, accessToken, tempDir)
	if err != nil {
		return nil, err
	}
	defer sshCleanup()

	cmd := exec.Command("git", "ls-remote", "--tags", repoURL)
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git ls-remote failed: %s: %w", string(output), err)
	}

	tags := make(map[string]string)
	const tagRefPrefix = "refs/tags/"
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || !strings.HasPrefix(fields[1], tagRefPrefix) {
			continue
		}
		sha, tagName := fields[0], strings.TrimPrefix(fields[1], tagRefPrefix)
		if peeled, ok := strings.CutSuffix(tagName, "^{}"); ok {
			tags[peeled] = sha
		} else if _, exists := tags[tagName]; !exists {
			tags[tagName] = sha
		}
	}

	return tags, nil
}

func (s *Service) CloneWorkspace(source, branch, vcsType, connectionType, accessToken, folder string, jobId string) (string, error) {
	tempDir, err := os.MkdirTemp("", fmt.Sprintf("terrakube-job-%s", jobId))
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir: %w", err)
	}

	repoURL := setupCredentialURL(source, vcsType, connectionType, accessToken)

	env, sshCleanup, err := setupSSHEnv(vcsType, accessToken, tempDir)
	if err != nil {
		return "", err
	}
	defer sshCleanup()

	cmdArgs := []string{"clone", "--depth", "1"}
	if branch != "" {
		cmdArgs = append(cmdArgs, "--branch", branch)
	}
	cmdArgs = append(cmdArgs, repoURL, tempDir)

	cloneCmd := exec.Command("git", cmdArgs...)
	cloneCmd.Env = env

	if output, err := cloneCmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone failed: %s: %w", string(output), err)
	}

	finalDir := tempDir
	if folder != "" {
		finalDir = filepath.Join(tempDir, folder)
	}

	return finalDir, nil
}
