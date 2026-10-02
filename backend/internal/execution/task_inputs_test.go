package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageNodeTaskInputsVerifiesContentAndCleanupBoundary(t *testing.T) {
	workspace := t.TempDir()
	source := filepath.Join(t.TempDir(), "downloaded-input")
	content := []byte("local user input\n")
	if err := os.WriteFile(source, content, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	attachment := localAgentInputArtifact{ArtifactID: "artifact_input_1", SHA256: hex.EncodeToString(digest[:]), ByteSize: int64(len(content)), FileName: "../requirements?.txt", FilePath: source}
	dir, prompt, err := stageNodeTaskInputs("task_input_local", workspace, []localAgentInputArtifact{attachment})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, attachment.SHA256) || !strings.Contains(prompt, "requirements_.txt") {
		t.Fatalf("input prompt omitted the verified file identity: %q", prompt)
	}
	path := filepath.Join(dir, nodeTaskInputName(attachment.ArtifactID, attachment.FileName))
	readBack, err := os.ReadFile(path)
	if err != nil || string(readBack) != string(content) {
		t.Fatalf("local agent workspace did not receive verified bytes: %q err=%v", readBack, err)
	}
	if _, _, err := stageNodeTaskInputs("task_input_local", workspace, []localAgentInputArtifact{attachment}); err != nil {
		t.Fatalf("matching retry did not reuse the staged input: %v", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := stageNodeTaskInputs("task_input_local", workspace, []localAgentInputArtifact{attachment}); err == nil {
		t.Fatal("tampered local input file was accepted")
	}
	if err := removeNodeTaskInputDirectory(dir, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatalf("task input directory was not removed after task execution: %v", err)
	}
	if err := removeNodeTaskInputDirectory(filepath.Join(workspace, "outside", "child"), workspace); err == nil {
		t.Fatal("cleanup accepted a path outside the task input directory")
	}
}
