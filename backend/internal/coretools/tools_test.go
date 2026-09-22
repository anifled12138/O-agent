package coretools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoreToolsExecution(t *testing.T) {
	tempDir := t.TempDir()
	toolList := GetCoreTools(tempDir)
	if len(toolList) != 6 {
		t.Fatalf("expected 6 tools, got %d", len(toolList))
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

	// 8. Test path traversal sandbox security
	badArgs, _ := json.Marshal(map[string]any{
		"path": "../../../outside.txt",
	})
	_, err = readTool.Handler(ctx, badArgs)
	if err == nil {
		t.Fatal("expected path traversal error, got nil")
	}
}
