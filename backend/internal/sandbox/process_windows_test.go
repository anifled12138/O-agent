//go:build windows

package sandbox

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestStageAppContainerPowerShellInputUsesPrivateFile(t *testing.T) {
	tempDir := t.TempDir()
	input := []byte("Write-Output 'sandbox-output-check'")
	command := exec.Command(`C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, "-NoProfile", "-NonInteractive", "-Command", "-")
	remaining, err := stageAppContainerPowerShellInput(command, input, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != nil {
		t.Fatalf("script bytes were still assigned to redirected stdin: %q", remaining)
	}
	if len(command.Args) < 3 || !strings.EqualFold(command.Args[len(command.Args)-2], "-EncodedCommand") {
		t.Fatalf("PowerShell command did not switch to its in-memory private loader: %q", command.Args)
	}
	encodedLoader, err := base64.StdEncoding.DecodeString(command.Args[len(command.Args)-1])
	if err != nil || len(encodedLoader)%2 != 0 {
		t.Fatalf("encoded PowerShell loader is invalid: err=%v", err)
	}
	loaderUnits := make([]uint16, len(encodedLoader)/2)
	for index := range loaderUnits {
		loaderUnits[index] = binary.LittleEndian.Uint16(encodedLoader[index*2:])
	}
	loader := string(utf16.Decode(loaderUnits))
	path := filepath.Join(tempDir, "command-input.ps1")
	if !strings.Contains(loader, path) || !strings.Contains(loader, "$env:TEMP") || !strings.Contains(loader, "$env:TMP") || !strings.Contains(loader, "[ScriptBlock]::Create") {
		t.Fatalf("PowerShell loader does not read and execute the private input file: %q", loader)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, input) {
		t.Fatalf("staged PowerShell input did not read back: %q err=%v", got, err)
	}
}

func TestStageAppContainerPowerShellInputLeavesOtherCommandsAlone(t *testing.T) {
	input := []byte("git status")
	command := exec.Command("git", "status")
	remaining, err := stageAppContainerPowerShellInput(command, input, t.TempDir())
	if err != nil || !bytes.Equal(remaining, input) || !bytes.Equal([]byte(strings.Join(command.Args, "\x00")), []byte("git\x00status")) {
		t.Fatalf("non-PowerShell command was changed: args=%q input=%q err=%v", command.Args, remaining, err)
	}
}
