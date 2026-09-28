package coretools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/runfiles"
)

type Tool struct {
	Definition provider.ToolDefinition
	Handler    func(ctx context.Context, args json.RawMessage) (any, error)
}

type processPolicy struct {
	readOnlyPaths               []string
	privateTempWorkingDirectory bool
	journalPath                 string
}

func GetCoreTools(workspaceRoot string, runScopes ...*runfiles.Scope) []Tool {
	var runScope *runfiles.Scope
	if len(runScopes) > 0 {
		runScope = runScopes[0]
	}
	return []Tool{
		{
			Definition: toolDef(
				"fs_read",
				"Read workspace text files up to 32 MiB or page through a run-scoped temporary artifact by ID. Supports 1-based line offsets and line-numbered output.",
				`{"type":"object","properties":{"path":{"type":"string","description":"Relative path to a file in the workspace; use this or artifactId"},"artifactId":{"type":"string","description":"Opaque ID returned by grep_search or exec_script for a temporary artifact; use this or path"},"offset":{"type":"integer","description":"1-based starting line number (default 1)"},"limit":{"type":"integer","description":"Maximum number of lines to read (default 200)"},"withLineNumbers":{"type":"boolean","description":"Whether to prepend line numbers (default true)"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var p struct {
					Path            string `json:"path"`
					ArtifactID      string `json:"artifactId"`
					Offset          int    `json:"offset"`
					Limit           int    `json:"limit"`
					WithLineNumbers *bool  `json:"withLineNumbers"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				if (p.Path == "") == (p.ArtifactID == "") {
					return nil, fmt.Errorf("provide exactly one of path or artifactId")
				}
				var data []byte
				var err error
				if p.ArtifactID != "" {
					if runScope == nil {
						return nil, fmt.Errorf("temporary artifacts are unavailable outside an Agent run")
					}
					data, err = runScope.ReadArtifact(p.ArtifactID)
				} else {
					var targetPath string
					targetPath, err = safeResolve(workspaceRoot, p.Path)
					if err == nil {
						var info os.FileInfo
						info, err = os.Stat(targetPath)
						if err == nil && info.Size() > 32<<20 {
							err = fmt.Errorf("file exceeds the 32 MiB read limit")
						}
						if err == nil {
							data, err = os.ReadFile(targetPath)
						}
					}
				}
				if err != nil {
					return nil, err
				}

				withNumbers := true
				if p.WithLineNumbers != nil {
					withNumbers = *p.WithLineNumbers
				}

				rawLines := strings.Split(string(data), "\n")
				totalLines := len(rawLines)

				start := 0
				if p.Offset > 1 {
					start = p.Offset - 1
					if start > totalLines {
						start = totalLines
					}
				}

				limit := p.Limit
				if limit <= 0 {
					limit = 200
				}
				end := start + limit
				if end > totalLines {
					end = totalLines
				}

				var formatted []string
				for i := start; i < end; i++ {
					lineContent := rawLines[i]
					// Remove trailing carriage return if Windows style
					lineContent = strings.TrimSuffix(lineContent, "\r")
					if withNumbers {
						formatted = append(formatted, fmt.Sprintf("%6d | %s", i+1, lineContent))
					} else {
						formatted = append(formatted, lineContent)
					}
				}

				content := strings.Join(formatted, "\n")
				truncated := false
				const maxFileReadBytes = 64 * 1024
				if len(content) > maxFileReadBytes {
					content = content[:maxFileReadBytes] + fmt.Sprintf("\n... [truncated to %d KB; use smaller limit or offset]", maxFileReadBytes/1024)
					truncated = true
				}

				hasMore := end < totalLines
				var nextOffset any = nil
				if hasMore {
					nextOffset = end + 1
				}

				return map[string]any{
					"path":       p.Path,
					"artifactId": p.ArtifactID,
					"startLine":  start + 1,
					"endLine":    end,
					"totalLines": totalLines,
					"hasMore":    hasMore,
					"nextOffset": nextOffset,
					"content":    content,
					"truncated":  truncated,
				}, nil
			},
		},
		{
			Definition: toolDef(
				"exec_script",
				"Run a short-lived script from source stored as a turn-scoped artifact. The script can read the workspace; its working directory and temporary writes use a private per-command AppContainer directory that is removed when the command ends. On Windows it runs in an AppContainer with network access disabled and a 128-process limit; platforms without a native sandbox backend reject execution. Script source is capped at 1 MiB and execution at 180 seconds; each output stream captures at most 1 MiB. Use exec_command for intentional project commands.",
				`{"type":"object","required":["language","source"],"properties":{"language":{"type":"string","enum":["python","node","powershell","shell"],"description":"Installed interpreter to use"},"source":{"type":"string","description":"Script source; maximum 1 MiB"},"timeoutSeconds":{"type":"integer","description":"Execution timeout in seconds (default 30, max 180)"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				if runScope == nil {
					return nil, fmt.Errorf("temporary script execution is available only inside an Agent run")
				}
				var p struct {
					Language       string `json:"language"`
					Source         string `json:"source"`
					TimeoutSeconds int    `json:"timeoutSeconds"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				p.Language = strings.ToLower(strings.TrimSpace(p.Language))
				if len(p.Source) > 1<<20 {
					return nil, fmt.Errorf("script source exceeds the 1 MiB limit")
				}
				if p.TimeoutSeconds <= 0 {
					p.TimeoutSeconds = 30
				}
				if p.TimeoutSeconds > 180 {
					p.TimeoutSeconds = 180
				}
				extension, candidates, prefixArgs, err := scriptRuntime(p.Language)
				if err != nil {
					return nil, err
				}
				artifact, err := runScope.WriteArtifact("script", extension, []byte(p.Source))
				if err != nil {
					return nil, err
				}
				journalPath, err := runScope.NewSandboxJournalPath()
				if err != nil {
					return nil, err
				}
				var interpreter string
				for _, candidate := range candidates {
					interpreter, err = exec.LookPath(candidate)
					if err == nil {
						break
					}
				}
				if err != nil {
					return nil, fmt.Errorf("no interpreter found for %s script: tried %s", p.Language, strings.Join(candidates, ", "))
				}
				cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
				defer cancel()
				cmdArgs := make([]string, 0, len(prefixArgs)+1)
				if p.Language == "python" && runtime.GOOS == "windows" && strings.EqualFold(filepath.Base(interpreter), "py.exe") {
					cmdArgs = append(cmdArgs, "-3")
				}
				cmdArgs = append(cmdArgs, prefixArgs...)
				cmd := exec.CommandContext(cmdCtx, interpreter, cmdArgs...)
				cmd.Dir = workspaceRoot
				stdout, stderr := cappedBuffer{limit: 1 << 20}, cappedBuffer{limit: 1 << 20}
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				policy := processPolicy{readOnlyPaths: []string{workspaceRoot}, privateTempWorkingDirectory: true, journalPath: journalPath}
				runErr, cleanupErr := runProcessTree(cmdCtx, cmd, []byte(p.Source), policy)
				if cleanupErr != nil {
					return nil, errors.Join(runErr, cleanupErr)
				}
				exitCode := 0
				if runErr != nil {
					var exited bool
					exitCode, exited = processExitCode(runErr)
					if !exited && cmdCtx.Err() == nil {
						return nil, runErr
					}
				}
				outStr, outTruncated := balanceTruncate(stdout.String(), 24*1024)
				errStr, errTruncated := balanceTruncate(stderr.String(), 16*1024)
				return map[string]any{
					"language":         p.Language,
					"scriptArtifactId": artifact.ID,
					"stdout":           outStr,
					"stderr":           errStr,
					"exitCode":         exitCode,
					"timedOut":         errors.Is(cmdCtx.Err(), context.DeadlineExceeded),
					"outTruncated":     outTruncated || errTruncated || stdout.truncated || stderr.truncated,
				}, nil
			},
		},
		{
			Definition: toolDef(
				"fs_write",
				"Write text content to a file in the workspace. Overwrites existing file or creates parent directories as needed.",
				`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string","description":"Relative path to the file"},"content":{"type":"string","description":"Full text content to write"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var p struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				targetPath, err := safeResolve(workspaceRoot, p.Path)
				if err != nil {
					return nil, err
				}
				if err := os.MkdirAll(filepath.Dir(targetPath), 0755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(targetPath, []byte(p.Content), 0644); err != nil {
					return nil, err
				}
				return map[string]any{
					"status":       "ok",
					"path":         p.Path,
					"bytesWritten": len(p.Content),
				}, nil
			},
		},
		{
			Definition: toolDef(
				"file_edit",
				"Perform exact string replacement in an existing file. oldString must match uniquely in the file.",
				`{"type":"object","required":["path","oldString","newString"],"properties":{"path":{"type":"string","description":"Relative path to the file"},"oldString":{"type":"string","description":"Exact snippet to find and replace"},"newString":{"type":"string","description":"New replacement snippet"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var p struct {
					Path      string `json:"path"`
					OldString string `json:"oldString"`
					NewString string `json:"newString"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				if p.OldString == "" {
					return nil, fmt.Errorf("oldString cannot be empty")
				}
				targetPath, err := safeResolve(workspaceRoot, p.Path)
				if err != nil {
					return nil, err
				}
				data, err := os.ReadFile(targetPath)
				if err != nil {
					return nil, err
				}

				content := string(data)
				// Normalize windows line endings for matching if needed
				normalizedOld := strings.ReplaceAll(p.OldString, "\r\n", "\n")
				normalizedContent := strings.ReplaceAll(content, "\r\n", "\n")

				count := strings.Count(normalizedContent, normalizedOld)
				if count == 0 {
					return map[string]any{
						"status":  "error",
						"message": "oldString not found in file. Ensure exact matching including whitespace and indentation.",
					}, nil
				}
				if count > 1 {
					return map[string]any{
						"status":  "error",
						"message": fmt.Sprintf("oldString matches %d occurrences in file. Please provide more surrounding context to make it unique.", count),
					}, nil
				}

				// Replace the unique occurrence
				updated := strings.Replace(normalizedContent, normalizedOld, strings.ReplaceAll(p.NewString, "\r\n", "\n"), 1)
				if err := os.WriteFile(targetPath, []byte(updated), 0644); err != nil {
					return nil, err
				}

				return map[string]any{
					"status":  "ok",
					"path":    p.Path,
					"message": "File successfully updated with exact replacement.",
				}, nil
			},
		},
		{
			Definition: toolDef(
				"fs_list",
				"List directory contents or sub-trees in the workspace up to a maximum depth.",
				`{"type":"object","properties":{"path":{"type":"string","description":"Relative path to directory (default .)"},"depth":{"type":"integer","description":"Traversal depth (1 to 5, default 2)"},"includeIgnored":{"type":"boolean","description":"Whether to include .git and node_modules (default false)"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var p struct {
					Path           string `json:"path"`
					Depth          int    `json:"depth"`
					IncludeIgnored bool   `json:"includeIgnored"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				if p.Depth <= 0 {
					p.Depth = 2
				}
				if p.Depth > 5 {
					p.Depth = 5
				}
				workspaceAbs, err := filepath.Abs(workspaceRoot)
				if err != nil {
					return nil, err
				}
				targetDir, err := safeResolve(workspaceRoot, p.Path)
				if err != nil {
					return nil, err
				}

				type Entry struct {
					Path  string `json:"path"`
					IsDir bool   `json:"isDir"`
					Size  int64  `json:"size,omitempty"`
				}
				var entries []Entry

				var walkDir func(current string, currentDepth int) error
				walkDir = func(current string, currentDepth int) error {
					if currentDepth > p.Depth {
						return nil
					}
					files, err := os.ReadDir(current)
					if err != nil {
						return err
					}
					for _, f := range files {
						if len(entries) >= 300 {
							return nil
						}
						name := f.Name()
						if !p.IncludeIgnored && (name == ".git" || name == "node_modules") {
							continue
						}
						full := filepath.Join(current, name)
						rel, err := filepath.Rel(workspaceAbs, full)
						if err != nil {
							return err
						}
						cleanRel := filepath.ToSlash(rel)

						var size int64
						info, err := f.Info()
						if err != nil {
							return err
						}
						if !f.IsDir() {
							size = info.Size()
						}
						entries = append(entries, Entry{
							Path:  cleanRel,
							IsDir: f.IsDir(),
							Size:  size,
						})

						if f.IsDir() && currentDepth < p.Depth {
							if err := walkDir(full, currentDepth+1); err != nil {
								return err
							}
						}
					}
					return nil
				}

				if err := walkDir(targetDir, 1); err != nil {
					return nil, fmt.Errorf("list workspace: %w", err)
				}

				return map[string]any{
					"path":       p.Path,
					"depth":      p.Depth,
					"totalCount": len(entries),
					"entries":    entries,
				}, nil
			},
		},
		{
			Definition: toolDef(
				"grep_search",
				"Search workspace files using a regular expression. Skips .git, node_modules, .pnpm-store, and symbolic links by default; explicitly targeted paths can include ignored directories but symbolic links are never followed. Large result sets are stored as run-scoped temporary artifacts and can be paged with fs_read; artifacts retain at most the first 10000 matches.",
				`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string","description":"Regex search pattern; maximum 4 KiB"},"path":{"type":"string","description":"Relative directory or specific file path to search"},"maxMatches":{"type":"integer","description":"Maximum snippets to return directly in context (default 30, max 100)"},"includeIgnored":{"type":"boolean","description":"Whether to include .git, node_modules, and .pnpm-store (default false)"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var p struct {
					Pattern        string `json:"pattern"`
					Path           string `json:"path"`
					MaxMatches     int    `json:"maxMatches"`
					IncludeIgnored bool   `json:"includeIgnored"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				if p.MaxMatches <= 0 {
					p.MaxMatches = 30
				}
				if p.MaxMatches > 100 {
					p.MaxMatches = 100
				}
				if len(p.Pattern) > 4096 {
					return nil, fmt.Errorf("regex pattern exceeds the 4 KiB limit")
				}
				re, err := regexp.Compile(p.Pattern)
				if err != nil {
					return nil, fmt.Errorf("invalid regex pattern: %w", err)
				}
				targetPath, err := safeResolve(workspaceRoot, p.Path)
				if err != nil {
					return nil, err
				}

				type MatchSnippet struct {
					File            string `json:"file"`
					Line            int    `json:"line"`
					ColumnOffset    int    `json:"columnOffset,omitempty"`
					Snippet         string `json:"snippet"`
					TotalLineLength int    `json:"totalLineLength,omitempty"`
				}

				var matches []MatchSnippet
				var allMatches []MatchSnippet
				totalFound := 0
				const maxArtifactMatches = 10000

				isExplicitTarget := p.Path != "" && p.Path != "." && p.Path != "./"

				walkErr := filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					// filepath.Walk uses Lstat and does not follow symlinks. On Windows,
					// reading a symlink to a directory as a file fails with ERROR_INVALID_FUNCTION.
					// Skip all symlinks to avoid that error and prevent escaping the workspace.
					if info.Mode()&os.ModeSymlink != 0 {
						return nil
					}
					if info.IsDir() {
						name := info.Name()
						if !p.IncludeIgnored && !isExplicitTarget {
							if name == ".git" || name == "node_modules" || name == ".pnpm-store" {
								return filepath.SkipDir
							}
						}
						return nil
					}

					workspaceAbs, absErr := filepath.Abs(workspaceRoot)
					if absErr != nil {
						return absErr
					}
					rel, relErr := filepath.Rel(workspaceAbs, path)
					if relErr != nil {
						return relErr
					}
					cleanRel := filepath.ToSlash(rel)

					if info.Size() > 10*1024*1024 {
						return nil
					}

					data, err := os.ReadFile(path)
					if err != nil {
						return err
					}

					lines := strings.Split(string(data), "\n")
					for i, line := range lines {
						loc := re.FindStringIndex(line)
						if loc == nil {
							continue
						}

						totalFound++
						matchStart := loc[0]
						matchEnd := loc[1]
						lineLen := len(line)

						var snippet string
						const snippetWindow = 120
						if lineLen <= 300 {
							snippet = strings.TrimSpace(line)
						} else {
							sStart := matchStart - snippetWindow
							if sStart < 0 {
								sStart = 0
							}
							sEnd := matchEnd + snippetWindow
							if sEnd > lineLen {
								sEnd = lineLen
							}
							snippet = fmt.Sprintf("...[offset %d] %s ...[offset %d]", sStart, strings.TrimSpace(line[sStart:sEnd]), sEnd)
						}

						item := MatchSnippet{
							File:            cleanRel,
							Line:            i + 1,
							ColumnOffset:    matchStart,
							Snippet:         snippet,
							TotalLineLength: lineLen,
						}

						if len(allMatches) < maxArtifactMatches {
							allMatches = append(allMatches, item)
						}
						if len(matches) < p.MaxMatches {
							matches = append(matches, item)
						}
					}
					return nil
				})
				if walkErr != nil {
					return nil, fmt.Errorf("search workspace: %w", walkErr)
				}

				res := map[string]any{
					"pattern":    p.Pattern,
					"matches":    matches,
					"totalFound": totalFound,
					"returned":   len(matches),
				}

				if totalFound > len(matches) {
					if runScope == nil {
						return nil, fmt.Errorf("large grep results require an Agent run artifact scope")
					}
					dumpPayload, dumpErr := json.MarshalIndent(map[string]any{
						"pattern":    p.Pattern,
						"path":       p.Path,
						"totalFound": totalFound,
						"matches":    allMatches,
						"truncated":  totalFound > len(allMatches),
					}, "", "  ")
					if dumpErr != nil {
						return nil, fmt.Errorf("encode large grep results: %w", dumpErr)
					}
					artifact, writeErr := runScope.WriteArtifact("grep-results", "json", dumpPayload)
					if writeErr != nil {
						return nil, fmt.Errorf("save large grep results: %w", writeErr)
					}
					res["artifactId"] = artifact.ID
					res["artifactTruncated"] = totalFound > len(allMatches)
					res["notice"] = fmt.Sprintf("Found %d matches. Returning the first %d snippets; read artifactId %s with fs_read to page through the retained results.", totalFound, len(matches), artifact.ID)
				}

				return res, nil
			},
		},
		{
			Definition: toolDef(
				"exec_command",
				"Execute a read-only project command with a timeout in the workspace or specified subdirectory. On Windows the process runs in an AppContainer with workspace read access, writes limited to its private per-command temporary storage, network access disabled, and a 128-process limit; platforms without a native sandbox backend reject execution. Use the workspace file tools for project changes. Process descendants are terminated with the command; output capture is capped at 1 MiB per stream and returned head/tail are bounded.",
				`{"type":"object","required":["cmd"],"properties":{"cmd":{"type":"string","description":"Shell command to execute; maximum 1 MiB"},"workdir":{"type":"string","description":"Relative directory to run command in (default workspace root)"},"timeoutSeconds":{"type":"integer","description":"Command timeout in seconds (default 30, max 180)"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				if runScope == nil {
					return nil, fmt.Errorf("command execution is available only inside an Agent run")
				}
				var p struct {
					Cmd            string `json:"cmd"`
					Workdir        string `json:"workdir"`
					TimeoutSeconds int    `json:"timeoutSeconds"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				if len(p.Cmd) > 1<<20 {
					return nil, fmt.Errorf("command exceeds the 1 MiB limit")
				}
				if p.TimeoutSeconds <= 0 {
					p.TimeoutSeconds = 30
				}
				if p.TimeoutSeconds > 180 {
					p.TimeoutSeconds = 180
				}
				cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
				defer cancel()

				targetWorkdir := workspaceRoot
				if p.Workdir != "" {
					resolved, err := safeResolve(workspaceRoot, p.Workdir)
					if err != nil {
						return nil, err
					}
					targetWorkdir = resolved
				}

				var cmd *exec.Cmd
				if runtime.GOOS == "windows" {
					cmd = exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", "-")
				} else {
					cmd = exec.CommandContext(cmdCtx, "sh")
				}
				cmd.Dir = targetWorkdir
				journalPath, err := runScope.NewSandboxJournalPath()
				if err != nil {
					return nil, err
				}
				stdout, stderr := cappedBuffer{limit: 1 << 20}, cappedBuffer{limit: 1 << 20}
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				policy := processPolicy{readOnlyPaths: []string{workspaceRoot}, journalPath: journalPath}
				err, cleanupErr := runProcessTree(cmdCtx, cmd, []byte(p.Cmd), policy)
				if cleanupErr != nil {
					return nil, errors.Join(err, cleanupErr)
				}
				exitCode := 0
				if err != nil {
					var exited bool
					exitCode, exited = processExitCode(err)
					if !exited && cmdCtx.Err() == nil {
						return nil, err
					}
				}

				outStr, outTruncated := balanceTruncate(stdout.String(), 24*1024)
				errStr, errTruncated := balanceTruncate(stderr.String(), 16*1024)

				return map[string]any{
					"cmd":          p.Cmd,
					"workdir":      p.Workdir,
					"stdout":       outStr,
					"stderr":       errStr,
					"exitCode":     exitCode,
					"timedOut":     errors.Is(cmdCtx.Err(), context.DeadlineExceeded),
					"outTruncated": outTruncated || errTruncated || stdout.truncated || stderr.truncated,
				}, nil
			},
		},
	}
}

func processExitCode(err error) (int, bool) {
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return -1, false
}

type cappedBuffer struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || originalLength > 0
		return originalLength, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		b.truncated = true
	}
	_, err := b.Buffer.Write(value)
	return originalLength, err
}

func scriptRuntime(language string) (string, []string, []string, error) {
	switch language {
	case "python":
		if runtime.GOOS == "windows" {
			return "py", []string{"python", "python3", "py"}, []string{"-"}, nil
		}
		return "py", []string{"python3", "python"}, []string{"-"}, nil
	case "node":
		return "js", []string{"node"}, []string{"-"}, nil
	case "powershell":
		if runtime.GOOS == "windows" {
			return "ps1", []string{"powershell.exe", "pwsh.exe"}, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-"}, nil
		}
		return "ps1", []string{"pwsh", "powershell"}, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-"}, nil
	case "shell":
		if runtime.GOOS == "windows" {
			return "ps1", []string{"powershell.exe", "pwsh.exe"}, []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", "-"}, nil
		}
		return "sh", []string{"sh"}, nil, nil
	default:
		return "", nil, nil, fmt.Errorf("unsupported script language %q", language)
	}
}

// balanceTruncate keeps the first half and last half when output exceeds maxBytes,
// preserving both command start and error/exit stack traces at the end.
func balanceTruncate(s string, maxBytes int) (string, bool) {
	if len(s) <= maxBytes {
		return s, false
	}
	half := maxBytes / 2
	head := s[:half]
	tail := s[len(s)-half:]
	omitted := len(s) - maxBytes
	return fmt.Sprintf("%s\n\n... [%d bytes omitted / preserved head and tail stack] ...\n\n%s", head, omitted, tail), true
}

func toolDef(name, desc, schema string) provider.ToolDefinition {
	td := provider.ToolDefinition{Type: "function"}
	td.Function.Name = name
	td.Function.Description = desc
	td.Function.Parameters = json.RawMessage(schema)
	return td
}

func safeResolve(root, p string) (string, error) {
	cleanRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return "", err
	}
	var target string
	if filepath.IsAbs(p) {
		target, err = filepath.Abs(filepath.Clean(p))
	} else {
		target, err = filepath.Abs(filepath.Clean(filepath.Join(cleanRoot, p)))
	}
	if err != nil {
		return "", err
	}
	if !pathWithin(cleanRoot, target) {
		return "", fmt.Errorf("path traversal denied: %s is outside workspace root", p)
	}

	realRoot, err := filepath.EvalSymlinks(cleanRoot)
	if err != nil {
		if !os.IsPermission(err) {
			return "", fmt.Errorf("resolve workspace root: %w", err)
		}
		realRoot = cleanRoot
	}
	existing := target
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			break
		} else if !os.IsNotExist(statErr) {
			return "", statErr
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return "", fmt.Errorf("resolve path %q: no existing ancestor", p)
		}
		existing = parent
	}
	realExisting, err := filepath.EvalSymlinks(existing)
	if err != nil {
		if !os.IsPermission(err) {
			return "", err
		}
		realExisting = existing
	}
	if !pathWithin(realRoot, realExisting) {
		return "", fmt.Errorf("path traversal denied: %s escapes workspace through a symbolic link", p)
	}
	return target, nil
}

// ExistingWorkspaceFile reports whether a workspace write would replace an
// existing regular file. It uses the same traversal and symlink checks as the
// write handlers so permission classification cannot mistake an outside path
// for a harmless new file.
func ExistingWorkspaceFile(root, p string) (bool, error) {
	target, err := safeResolve(root, p)
	if err != nil {
		return false, err
	}
	info, err := os.Stat(target)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.IsDir() {
		return false, nil
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("cannot classify write target %q", p)
	}
	return true, nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
