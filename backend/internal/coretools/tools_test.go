package coretools

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"axiom.local/agent/internal/runfiles"
)

func TestCoreToolsExecution(t *testing.T) {
	tempDir := tempDirWithinWorkspace(t, "coretools-workspace-")
	managerRoot := tempDirWithinWorkspace(t, "coretools-agent-runs-")
	manager, err := runfiles.NewManager(managerRoot, tempDir)
	if err != nil {
		t.Fatal(err)
	}
	runScope, err := manager.NewScope()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := runScope.Close(); err != nil {
			t.Errorf("run scope cleanup failed: %v", err)
		}
	})
	toolList := GetCoreTools(tempDir, runScope)
	if len(toolList) != 7 {
		t.Fatalf("expected 7 tools, got %d", len(toolList))
	}

	tools := make(map[string]Tool)
	for _, tool := range toolList {
		tools[tool.Definition.Function.Name] = tool
	}

	ctx := context.Background()

	// 1. Test fs_write
	writeTool, ok := tools["fs_write"]
	if !ok {
		t.Fatal("fs_write tool missing")
	}
	writeArgs, _ := json.Marshal(map[string]any{
		"path":    "hello.txt",
		"content": "line 1: hello\nline 2: world\nline 3: end",
	})
	writeRes, err := writeTool.Handler(ctx, writeArgs)
	if err != nil {
		t.Fatalf("fs_write failed: %v", err)
	}
	if writeRes.(map[string]any)["status"] != "ok" {
		t.Fatalf("expected written=true, got %v", writeRes)
	}

	// 2. Test fs_read with line numbers and paging metadata
	readTool := tools["fs_read"]
	readArgs, _ := json.Marshal(map[string]any{
		"path":   "hello.txt",
		"offset": 1,
		"limit":  2,
	})
	readRes, err := readTool.Handler(ctx, readArgs)
	if err != nil {
		t.Fatalf("fs_read failed: %v", err)
	}
	rm := readRes.(map[string]any)
	if rm["totalLines"].(int) != 3 || rm["hasMore"].(bool) != true {
		t.Fatalf("unexpected read result: %#v", rm)
	}
	content := rm["content"].(string)
	if !strings.Contains(content, "     1 | line 1: hello") {
		t.Fatalf("expected line numbering in content, got: %s", content)
	}

	// 3. Test file_edit (exact replacement)
	editTool, ok := tools["file_edit"]
	if !ok {
		t.Fatal("file_edit tool missing")
	}
	editArgs, _ := json.Marshal(map[string]any{
		"path":      "hello.txt",
		"oldString": "line 2: world",
		"newString": "line 2: modified world",
	})
	editRes, err := editTool.Handler(ctx, editArgs)
	if err != nil {
		t.Fatalf("file_edit failed: %v", err)
	}
	if editRes.(map[string]any)["status"] != "ok" {
		t.Fatalf("expected edit status ok, got %#v", editRes)
	}

	// Verify file content after edit
	updatedData, _ := os.ReadFile(filepath.Join(tempDir, "hello.txt"))
	if !strings.Contains(string(updatedData), "line 2: modified world") {
		t.Fatalf("file content not updated correctly: %s", string(updatedData))
	}

	// 4. Test file_edit error cases (not found & multiple matches)
	dupArgs, _ := json.Marshal(map[string]any{
		"path":      "hello.txt",
		"oldString": "nonexistent_string",
		"newString": "abc",
	})
	dupRes, _ := editTool.Handler(ctx, dupArgs)
	if dupRes.(map[string]any)["status"] != "error" {
		t.Fatalf("expected error for non-existent string, got %#v", dupRes)
	}

	// 5. Test grep_search with intelligent snippet and offload
	grepTool := tools["grep_search"]
	prefix := strings.Repeat("a", 2000)
	suffix := strings.Repeat("b", 2000)
	hugeContent := prefix + "TARGET_KEYWORD_HERE" + suffix
	_ = os.WriteFile(filepath.Join(tempDir, "huge.txt"), []byte(hugeContent), 0644)

	grepArgs, _ := json.Marshal(map[string]any{
		"pattern": "TARGET_KEYWORD_HERE",
		"path":    ".",
	})
	grepRes, err := grepTool.Handler(ctx, grepArgs)
	if err != nil {
		t.Fatalf("grep_search failed: %v", err)
	}
	gb, _ := json.Marshal(grepRes)
	var gm map[string]any
	_ = json.Unmarshal(gb, &gm)
	matches := gm["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("expected 1 match from grep_search, got %d", len(matches))
	}
	firstMatch := matches[0].(map[string]any)
	snippet := firstMatch["snippet"].(string)
	if !strings.Contains(snippet, "TARGET_KEYWORD_HERE") {
		t.Fatalf("expected snippet to contain TARGET_KEYWORD_HERE, got %s", snippet)
	}
	if !strings.Contains(snippet, "offset") {
		t.Fatalf("expected snippet to contain offset marker for long line, got %s", snippet)
	}

	// 6. Test fs_list with depth control
	listTool := tools["fs_list"]
	listArgs, _ := json.Marshal(map[string]any{
		"path":  ".",
		"depth": 2,
	})
	listRes, err := listTool.Handler(ctx, listArgs)
	if err != nil {
		t.Fatalf("fs_list failed: %v", err)
	}
	lb, _ := json.Marshal(listRes)
	var lm map[string]any
	_ = json.Unmarshal(lb, &lm)
	entries := lm["entries"].([]any)
	if len(entries) == 0 {
		t.Fatal("expected entries from fs_list")
	}

	// 7. Test balanceTruncate on exec_command
	longOutput := strings.Repeat("START_HEADER_", 500) + strings.Repeat("X", 50000) + strings.Repeat("FATAL_CRASH_STACK_", 500)
	truncated, wasTruncated := balanceTruncate(longOutput, 1024)
	if !wasTruncated {
		t.Fatal("expected balanceTruncate to truncate")
	}
	if !strings.Contains(truncated, "START_HEADER_") || !strings.Contains(truncated, "FATAL_CRASH_STACK_") {
		t.Fatalf("expected truncated output to preserve both head and tail: %s", truncated)
	}

	if runtime.GOOS == "windows" {
		// 8. Script source remains a turn-scoped artifact; execution uses AppContainer TEMP.
		scriptSource := "printf 'script-ok:%s' \"$TMPDIR\""
		if runtime.GOOS == "windows" {
			scriptSource = "Write-Output ('script-ok:' + $env:TEMP)"
		}
		scriptArgs, _ := json.Marshal(map[string]any{"language": "shell", "source": scriptSource})
		scriptRes, err := tools["exec_script"].Handler(ctx, scriptArgs)
		if err != nil {
			t.Fatalf("exec_script failed: %v", err)
		}
		scriptResult := scriptRes.(map[string]any)
		if scriptResult["exitCode"].(int) != 0 || !strings.Contains(strings.ToLower(scriptResult["stdout"].(string)), `\ac\temp\axiom-command-`) {
			t.Fatalf("unexpected script result: %#v", scriptResult)
		}
		scriptArtifactArgs, _ := json.Marshal(map[string]any{"artifactId": scriptResult["scriptArtifactId"]})
		scriptRead, err := readTool.Handler(ctx, scriptArtifactArgs)
		if err != nil || !strings.Contains(scriptRead.(map[string]any)["content"].(string), scriptSource) {
			t.Fatalf("script artifact was not readable: result=%#v err=%v", scriptRead, err)
		}
		orphanMarker := filepath.Join(tempDir, "orphan-survived.txt")
		timeoutSource := "(sleep 2; printf orphan > '" + strings.ReplaceAll(orphanMarker, "'", "'\\''") + "') & sleep 10"
		if runtime.GOOS == "windows" {
			childSource := "Start-Sleep -Seconds 2; Set-Content -LiteralPath '" + strings.ReplaceAll(orphanMarker, "'", "''") + "' -Value orphan"
			encodedUnits := utf16.Encode([]rune(childSource))
			encodedBytes := make([]byte, len(encodedUnits)*2)
			for index, unit := range encodedUnits {
				binary.LittleEndian.PutUint16(encodedBytes[index*2:], unit)
			}
			timeoutSource = "Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile','-NonInteractive','-EncodedCommand','" + base64.StdEncoding.EncodeToString(encodedBytes) + "' -WindowStyle Hidden; Start-Sleep -Seconds 10"
		}
		timeoutArgs, _ := json.Marshal(map[string]any{"language": "shell", "source": timeoutSource, "timeoutSeconds": 1})
		timeoutStart := time.Now()
		timeoutRes, err := tools["exec_script"].Handler(ctx, timeoutArgs)
		if err != nil {
			t.Fatalf("timed script failed to return a bounded result: %v", err)
		}
		timeoutResult := timeoutRes.(map[string]any)
		if timeoutResult["timedOut"] != true || time.Since(timeoutStart) > 5*time.Second {
			t.Fatalf("script timeout was not enforced: elapsed=%s result=%#v", time.Since(timeoutStart), timeoutResult)
		}
		time.Sleep(2200 * time.Millisecond)
		if _, err := os.Stat(orphanMarker); !os.IsNotExist(err) {
			t.Fatalf("script child survived run cancellation: stat error=%v", err)
		}
	}

	// 9. Large grep results are readable during this run and are never written
	// into the workspace tree.
	manyMatches := strings.Repeat("TARGET_MATCH\n", 40)
	if err := os.WriteFile(filepath.Join(tempDir, "many.txt"), []byte(manyMatches), 0o600); err != nil {
		t.Fatal(err)
	}
	largeGrepArgs, _ := json.Marshal(map[string]any{"pattern": "TARGET_MATCH", "path": "many.txt", "maxMatches": 5})
	largeGrep, err := grepTool.Handler(ctx, largeGrepArgs)
	if err != nil {
		t.Fatalf("large grep_search failed: %v", err)
	}
	largeResult := largeGrep.(map[string]any)
	artifactID, ok := largeResult["artifactId"].(string)
	if !ok || artifactID == "" {
		t.Fatalf("expected large grep artifact ID, got %#v", largeResult)
	}
	grepArtifactArgs, _ := json.Marshal(map[string]any{"artifactId": artifactID, "offset": 3, "limit": 5})
	grepArtifact, err := readTool.Handler(ctx, grepArtifactArgs)
	if err != nil || !strings.Contains(grepArtifact.(map[string]any)["content"].(string), "TARGET_MATCH") {
		t.Fatalf("large grep artifact could not be read: result=%#v err=%v", grepArtifact, err)
	}

	// 10. Test path traversal sandbox security
	badArgs, _ := json.Marshal(map[string]any{
		"path": "../../../outside.txt",
	})
	_, err = readTool.Handler(ctx, badArgs)
	if err == nil {
		t.Fatal("expected path traversal error, got nil")
	}

	runDir := runScope.Dir()
	if err := runScope.Close(); err != nil {
		t.Fatalf("run scope cleanup failed: %v", err)
	}
	if _, err := os.Stat(runDir); !os.IsNotExist(err) {
		t.Fatalf("expected run directory to be removed; stat err=%v", err)
	}
}

func tempDirWithinWorkspace(t *testing.T, pattern string) string {
	t.Helper()
	dir, err := os.MkdirTemp(".", pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove test temp directory %q: %v", dir, err)
		}
	})
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}
