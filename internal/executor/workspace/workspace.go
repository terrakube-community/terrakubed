package workspace

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/terrakube-community/terrakubed/internal/git"
	"github.com/terrakube-community/terrakubed/internal/model"
)

type Workspace struct {
	Job        *model.TerraformJob
	WorkingDir string
	apiToken   string // used for downloading remote-content (CLI) uploads
	sshKeyPath string // persisted SSH key for terraform init module downloads
}

func NewWorkspace(job *model.TerraformJob, apiToken string) *Workspace {
	return &Workspace{
		Job:      job,
		apiToken: apiToken,
	}
}

// Setup prepares the working directory.
// For CLI-triggered runs (branch == "remote-content") it downloads and extracts
// the tar.gz that Terraform CLI uploaded to the API, matching the Java executor's
// SetupWorkspaceImpl.prepareWorkspace() behaviour.
// For VCS-backed runs it does the usual git clone.
func (w *Workspace) Setup() (string, error) {
	if w.Job.Branch == "remote-content" {
		return w.setupFromTarGz()
	}

	// Persist SSH key so terraform init can also use it when downloading modules.
	// git.CloneWorkspace writes the key to a temp dir and cleans it up after clone,
	// so by the time terraform init runs the key is gone. We keep a separate copy.
	if err := w.persistSSHKey(); err != nil {
		log.Printf("Warning: failed to persist SSH key for terraform init: %v", err)
	}

	gitSvc := git.NewService()
	finalDir, err := gitSvc.CloneWorkspace(w.Job.Source, w.Job.Branch, w.Job.VcsType, w.Job.ConnectionType, w.Job.AccessToken, w.Job.Folder, w.Job.JobId)
	if err != nil {
		return "", err
	}

	// WorkingDir keeps track of the temp root so it can be cleaned up
	// finalDir might be inside the temp root (when Folder is set)
	if w.Job.Folder != "" {
		w.WorkingDir = strings.TrimSuffix(finalDir, "/"+w.Job.Folder)
	} else {
		w.WorkingDir = finalDir
	}

	return finalDir, nil
}

// setupFromTarGz handles CLI-uploaded configurations.
// The Java API sets source = "https://<api>/remote/tfe/v2/configuration-versions/<id>/terraformContent.tar.gz"
// and branch = "remote-content" when Terraform CLI uploads local config via the remote backend.
func (w *Workspace) setupFromTarGz() (string, error) {
	tempDir, err := os.MkdirTemp("", "terrakube-cli-")
	if err != nil {
		return "", fmt.Errorf("failed to create temp dir for CLI upload: %w", err)
	}
	w.WorkingDir = tempDir

	req, err := http.NewRequest(http.MethodGet, w.Job.Source, nil)
	if err != nil {
		return "", fmt.Errorf("failed to build download request for %s: %w", w.Job.Source, err)
	}
	if w.apiToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.apiToken)
	}

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to download CLI config from %s: %w", w.Job.Source, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("failed to download CLI config: HTTP %d from %s", resp.StatusCode, w.Job.Source)
	}

	if err := extractTarGz(resp.Body, tempDir); err != nil {
		return "", fmt.Errorf("failed to extract CLI config tar.gz: %w", err)
	}

	// For CLI uploads the Folder field is meaningless: Terraform CLI uploads only the
	// specific directory's contents (not the full repo), so the extracted root IS the
	// working directory. Applying Folder here would point to a non-existent sub-path.
	return tempDir, nil
}

// extractTarGz extracts a gzipped tar archive into destDir.
func extractTarGz(r io.Reader, destDir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("not a valid gzip stream: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	cleanDest := filepath.Clean(destDir) + string(filepath.Separator)

	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		// Resolve target path and guard against path-traversal
		target := filepath.Join(destDir, header.Name)
		if !strings.HasPrefix(filepath.Clean(target)+string(filepath.Separator), cleanDest) {
			return fmt.Errorf("unsafe tar entry path: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(header.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.Symlink(header.Linkname, target); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}

	return nil
}

// persistSSHKey writes the VCS SSH key to a temp file that outlives the git
// clone so that subprocesses spawned by terraform init (e.g. git clone for
// module sources) can also authenticate. The path is injected into the job's
// EnvironmentVariables as GIT_SSH_COMMAND, which terraform-exec propagates to
// every git subprocess it launches.
func (w *Workspace) persistSSHKey() error {
	if !strings.HasPrefix(w.Job.VcsType, "SSH") || w.Job.AccessToken == "" {
		return nil
	}

	// Derive key filename from VCS type (e.g. "SSH~id_ed25519" → "id_ed25519")
	keyName := "id_rsa"
	if parts := strings.SplitN(w.Job.VcsType, "~", 2); len(parts) == 2 && parts[1] != "" {
		keyName = parts[1]
	}

	keyFile, err := os.CreateTemp("", fmt.Sprintf("terrakube-ssh-%s-*", keyName))
	if err != nil {
		return fmt.Errorf("create temp key file: %w", err)
	}
	defer keyFile.Close()

	if err := os.Chmod(keyFile.Name(), 0600); err != nil {
		return fmt.Errorf("chmod key file: %w", err)
	}
	if _, err := keyFile.WriteString(w.Job.AccessToken); err != nil {
		return fmt.Errorf("write key file: %w", err)
	}

	w.sshKeyPath = keyFile.Name()

	// Inject into job env so terraform and any git subprocess it spawns use this key.
	// StrictHostKeyChecking=no avoids known_hosts issues in ephemeral pod environments.
	if w.Job.EnvironmentVariables == nil {
		w.Job.EnvironmentVariables = make(map[string]string)
	}
	w.Job.EnvironmentVariables["GIT_SSH_COMMAND"] = fmt.Sprintf(
		"ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null",
		w.sshKeyPath,
	)

	log.Printf("SSH key persisted for terraform module downloads: %s", w.sshKeyPath)
	return nil
}

func (w *Workspace) Cleanup() error {
	if w.sshKeyPath != "" {
		os.Remove(w.sshKeyPath)
	}
	if w.WorkingDir != "" {
		return os.RemoveAll(w.WorkingDir)
	}
	return nil
}
