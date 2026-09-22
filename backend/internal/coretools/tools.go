package coretools

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"axiom.local/agent/internal/provider"
)

type Tool struct {
	Definition provider.ToolDefinition
	Handler    func(ctx context.Context, args json.RawMessage) (any, error)
}

func GetCoreTools(workspaceRoot string) []Tool {
	return []Tool{
		{
			Definition: toolDef(
				"fs_read",
				"Read text content from a file in the workspace. Supports 1-based line offset and limit with line-numbered output.",
				`{"type":"object","required":["path"],"properties":{"path":{"type":"string","description":"Relative path to the file"},"offset":{"type":"integer","description":"1-based starting line number (default 1)"},"limit":{"type":"integer","description":"Maximum number of lines to read (default 200)"},"withLineNumbers":{"type":"boolean","description":"Whether to prepend line numbers (default true)"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var p struct {
					Path            string `json:"path"`
					Offset          int    `json:"offset"`
					Limit           int    `json:"limit"`
					WithLineNumbers *bool  `json:"withLineNumbers"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				targetPath, err := safeResolve(workspaceRoot, p.Path)
				if err != nil {
					return nil, err
				}
				data, err := os.ReadFile(targetPath)
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
				_ = json.Unmarshal(args, &p)
				if p.Depth <= 0 {
					p.Depth = 2
				}
				if p.Depth > 5 {
					p.Depth = 5
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
						return nil
					}
					for _, f := range files {
						name := f.Name()
						if !p.IncludeIgnored && (name == ".git" || name == "node_modules") {
							continue
						}
						full := filepath.Join(current, name)
						rel, _ := filepath.Rel(workspaceRoot, full)
						cleanRel := filepath.ToSlash(rel)

						var size int64
						if info, err := f.Info(); err == nil && !f.IsDir() {
							size = info.Size()
						}
						entries = append(entries, Entry{
							Path:  cleanRel,
							IsDir: f.IsDir(),
							Size:  size,
						})

						if f.IsDir() && currentDepth < p.Depth {
							_ = walkDir(full, currentDepth+1)
						}
						if len(entries) >= 300 {
							return nil
						}
					}
					return nil
				}

				_ = walkDir(targetDir, 1)

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
				"Search for text matching regex pattern across files with intelligent snippets and result persistence.",
				`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string","description":"Regex search pattern"},"path":{"type":"string","description":"Relative directory or specific file path to search"},"maxMatches":{"type":"integer","description":"Maximum snippets to return directly in context (default 30, max 100)"},"includeIgnored":{"type":"boolean","description":"Whether to include .git and node_modules (default false)"}},"additionalProperties":false}`,
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

				isExplicitTarget := p.Path != "" && p.Path != "." && p.Path != "./"

				_ = filepath.Walk(targetPath, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return nil
					}
					if info.IsDir() {
						name := info.Name()
						if !p.IncludeIgnored && !isExplicitTarget {
							if name == ".git" || name == "node_modules" {
								return filepath.SkipDir
							}
						}
						return nil
					}

					rel, relErr := filepath.Rel(workspaceRoot, path)
					if relErr != nil {
						rel = path
					}
					cleanRel := filepath.ToSlash(rel)

					if info.Size() > 10*1024*1024 {
						return nil
					}

					data, err := os.ReadFile(path)
					if err != nil {
						return nil
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

						allMatches = append(allMatches, item)
						if len(matches) < p.MaxMatches {
							matches = append(matches, item)
						}
					}
					return nil
				})

				res := map[string]any{
					"pattern":    p.Pattern,
					"matches":    matches,
					"totalFound": totalFound,
					"returned":   len(matches),
				}

				if totalFound > len(matches) {
					offloadDir := filepath.Join(workspaceRoot, ".axiom", "outputs")
					_ = os.MkdirAll(offloadDir, 0755)
					randSuffix := make([]byte, 4)
					_, _ = rand.Read(randSuffix)
					dumpFile := filepath.Join(offloadDir, fmt.Sprintf("grep_%s_%s.json", time.Now().Format("20060102_150405"), hex.EncodeToString(randSuffix)))
					dumpPayload, dumpErr := json.MarshalIndent(map[string]any{
						"pattern":    p.Pattern,
						"path":       p.Path,
						"totalFound": totalFound,
						"matches":    allMatches,
					}, "", "  ")
					if dumpErr == nil {
						if writeErr := os.WriteFile(dumpFile, dumpPayload, 0644); writeErr == nil {
							relDump, _ := filepath.Rel(workspaceRoot, dumpFile)
							res["offloadedFile"] = filepath.ToSlash(relDump)
							res["notice"] = fmt.Sprintf("Found %d matches. Returning top %d snippets. Full results saved to %s for inspection via fs_read.", totalFound, len(matches), filepath.ToSlash(relDump))
						}
					}
				}

				return res, nil
			},
		},
		{
			Definition: toolDef(
				"exec_command",
				"Execute a shell command with a timeout in the workspace or specified subdirectory. Long outputs are balanced with head and tail preservation.",
				`{"type":"object","required":["cmd"],"properties":{"cmd":{"type":"string","description":"Shell command to execute"},"workdir":{"type":"string","description":"Relative directory to run command in (default workspace root)"},"timeoutSeconds":{"type":"integer","description":"Command timeout in seconds (default 30, max 180)"}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				var p struct {
					Cmd            string `json:"cmd"`
					Workdir        string `json:"workdir"`
					TimeoutSeconds int    `json:"timeoutSeconds"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
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
				if os.Getenv("OS") == "Windows_NT" {
					cmd = exec.CommandContext(cmdCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", p.Cmd)
				} else {
					cmd = exec.CommandContext(cmdCtx, "sh", "-c", p.Cmd)
				}
				cmd.Dir = targetWorkdir
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr

				err := cmd.Run()
				exitCode := 0
				if err != nil {
					if exitErr, ok := err.(*exec.ExitError); ok {
						exitCode = exitErr.ExitCode()
					} else {
						exitCode = -1
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
					"outTruncated": outTruncated || errTruncated,
				}, nil
			},
		},
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

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
