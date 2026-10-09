package coretools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"axiom.local/agent/internal/provider"
	"axiom.local/agent/internal/runfiles"
	"axiom.local/agent/internal/sandbox"
)

type Tool struct {
	Definition provider.ToolDefinition
	Handler    func(ctx context.Context, args json.RawMessage) (any, error)
}

type BrowserSessions interface {
	Tool() Tool
	Close() error
}

type SourceArchiver func(sourceType string, content []byte) (string, error)
type StreamSourceArchiver func(ctx context.Context, sourceType string, reader io.Reader, size int64, sha256 string) (string, error)

// ExecutionConfig is injected by the trusted Agent host, never tool arguments.
type ExecutionConfig struct {
	Backend                 sandbox.Backend
	InstallDir              string
	RunnerPath              string
	SetupPath               string
	RunnerSource            string
	GitCredentials          sandbox.GitCredentialBroker
	GitCredentialURLs       []string
	ProtectedPaths          []string
	TaskWorkspaceQuotaBytes int64
	BrowserSessions         BrowserSessions
}

type processPolicy struct {
	backend                     sandbox.Backend
	installDir                  string
	runnerPath                  string
	readOnlyPaths               []string
	writePaths                  []string
	networkAccess               bool
	networkAllowHosts           []string
	gitCredentials              sandbox.GitCredentialBroker
	gitCredentialURLs           []string
	protectedPaths              []string
	powershellExitWrapper       bool
	privateTempWorkingDirectory bool
	journalPath                 string
	timeout                     time.Duration
}

func GetCoreTools(workspaceRoot string, runScopes ...*runfiles.Scope) []Tool {
	var runScope *runfiles.Scope
	if len(runScopes) > 0 {
		runScope = runScopes[0]
	}
	return GetCoreToolsWithSourceArchive(workspaceRoot, runScope, nil)
}

// GetCoreToolsWithSourceArchive lets Agent runs retain immutable snapshots
// before a tool applies its model-facing output limit.
func GetCoreToolsWithSourceArchive(workspaceRoot string, runScope *runfiles.Scope, archive SourceArchiver, configs ...ExecutionConfig) []Tool {
	return getCoreTools(workspaceRoot, runScope, archive, nil, configs...)
}

// GetCoreToolsWithStreamSourceArchive adds durable streaming snapshots for
// files too large to keep inline while preserving the existing small-source API.
func GetCoreToolsWithStreamSourceArchive(workspaceRoot string, runScope *runfiles.Scope, archive SourceArchiver, streamArchive StreamSourceArchiver, configs ...ExecutionConfig) []Tool {
	return getCoreTools(workspaceRoot, runScope, archive, streamArchive, configs...)
}

func getCoreTools(workspaceRoot string, runScope *runfiles.Scope, archive SourceArchiver, streamArchive StreamSourceArchiver, configs ...ExecutionConfig) []Tool {
	execution := ExecutionConfig{Backend: sandbox.PlatformDefaultBackend()}
	if len(configs) > 0 {
		execution = configs[0]
		if execution.Backend == "" {
			execution.Backend = sandbox.PlatformDefaultBackend()
		}
	}
	sandboxDescription := "The host-selected Windows process sandbox and 128-process Job limit remain active; platforms without a native sandbox reject execution."
	if execution.Backend == sandbox.BackendWindowsNative {
		sandboxDescription = "Windows uses the installed Offline/Online low-privilege accounts, restricted-token runner, file ACLs, WFP network policy, and a 128-process Job limit. When Go, Node, or Python is installed in the host toolchain path, binary-version-separated Go build/module, npm/yarn, or pip caches persist in a protected local cache directory resolved for the current Windows user and are writable by sandbox commands. HTTPS Git credentials configured in Settings are encrypted at rest and brokered only for the selected repository URL over an authenticated per-command named pipe. SSH remotes can use identities loaded in the current Windows user's OpenSSH agent through an authenticated command-scoped broker; SSH is blocked with an explicit error if the Host agent is unavailable or has no usable identity. Private keys are not copied into command scratch. SSH-format signing uses command-scoped Git settings and the Host identity; OpenPGP signing configuration remains separate."
	} else if runtime.GOOS == "linux" {
		sandboxDescription = linuxSandboxDescription(execution.TaskWorkspaceQuotaBytes)
	}
	tools := []Tool{
		{
			Definition: toolDef(
				"fs_read",
				"Read workspace text files of any size or page through a run-scoped temporary artifact by ID. Reads are streamed and support 1-based line offsets; Agent runs return a full-file SHA-256. Large files are encrypted and archived through the chunked artifact store.",
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
				var source *os.File
				var err error
				if p.ArtifactID != "" {
					if runScope == nil {
						return nil, fmt.Errorf("temporary artifacts are unavailable outside an Agent run")
					}
					source, err = runScope.OpenArtifact(p.ArtifactID)
				} else {
					var targetPath string
					targetPath, err = safeResolve(workspaceRoot, p.Path)
					if err == nil {
						source, err = os.Open(targetPath)
					}
				}
				if err != nil {
					return nil, err
				}
				defer source.Close()
				snapshot, err := streamTextSnapshot(ctx, source, fsReadInlineArchiveBytes)
				if err != nil {
					return nil, fmt.Errorf("read file snapshot: %w", err)
				}
				sourceID := ""
				sourceArchiveStatus := "not_configured"
				if archive != nil && snapshot.inline != nil {
					sourceID, err = archive("file_snapshot", snapshot.inline)
					if err != nil {
						return nil, fmt.Errorf("archive file snapshot: %w", err)
					}
					sourceArchiveStatus = "archived"
				} else if streamArchive != nil && !snapshot.inlineAvailable {
					if _, err := source.Seek(0, io.SeekStart); err != nil {
						return nil, fmt.Errorf("rewind file for encrypted snapshot archive: %w", err)
					}
					sourceID, err = streamArchive(ctx, "file_snapshot", source, snapshot.fileInfo.Size(), snapshot.sha256)
					if err != nil {
						return nil, fmt.Errorf("archive large file snapshot: %w", err)
					}
					sourceArchiveStatus = "archived"
				} else if archive != nil && !snapshot.inlineAvailable {
					sourceArchiveStatus = "requires_chunked_snapshot_transfer"
				}

				withNumbers := true
				if p.WithLineNumbers != nil {
					withNumbers = *p.WithLineNumbers
				}

				limit := p.Limit
				if limit <= 0 {
					limit = 200
				}
				if p.Offset < 1 {
					p.Offset = 1
				}
				content, startLine, endLine, pageTruncated, err := readTextPage(ctx, source, snapshot.totalLines, p.Offset, limit, withNumbers, 64*1024)
				if err != nil {
					return nil, fmt.Errorf("read requested file page: %w", err)
				}
				pageInfo, err := source.Stat()
				if err != nil {
					return nil, fmt.Errorf("verify file after reading page: %w", err)
				}
				if !os.SameFile(snapshot.fileInfo, pageInfo) || snapshot.fileInfo.Size() != pageInfo.Size() || !snapshot.fileInfo.ModTime().Equal(pageInfo.ModTime()) {
					return nil, fmt.Errorf("file changed while it was being read; retry to obtain one consistent snapshot")
				}
				truncated := pageTruncated
				hasMore := endLine < snapshot.totalLines
				var nextOffset any = nil
				if hasMore {
					nextOffset = int(endLine + 1)
				}

				return map[string]any{
					"path":                p.Path,
					"artifactId":          p.ArtifactID,
					"sourceId":            sourceID,
					"sourceArchiveStatus": sourceArchiveStatus,
					"contentSha256":       snapshot.sha256,
					"startLine":           int(startLine),
					"endLine":             int(endLine),
					"totalLines":          int(snapshot.totalLines),
					"hasMore":             hasMore,
					"nextOffset":          nextOffset,
					"content":             content,
					"truncated":           truncated,
				}, nil
			},
		},
		{
			Definition: toolDef(
				"exec_script",
				"Run a short-lived script from source stored as a turn-scoped artifact. The script can read the workspace; its working directory and temporary writes use a private per-command sandbox directory removed after the command ends. "+sandboxDescription+" Script source is capped at 1 MiB and execution at 180 seconds; each output stream captures at most 1 MiB. On Windows, PowerShell terminating errors and the last native command's exit code are reflected in exitCode. Captured output is archived before display truncation and returns captureSourceId; captureComplete is false if the capture cap was reached. Use exec_command for intentional project commands.",
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
				cmdCtx, cancel := context.WithCancel(ctx)
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
				policy := processPolicy{backend: execution.Backend, installDir: execution.InstallDir, runnerPath: execution.RunnerPath, readOnlyPaths: []string{workspaceRoot}, gitCredentials: execution.GitCredentials, gitCredentialURLs: execution.GitCredentialURLs, protectedPaths: execution.ProtectedPaths, powershellExitWrapper: runtime.GOOS == "windows" && (p.Language == "powershell" || p.Language == "shell"), privateTempWorkingDirectory: true, journalPath: journalPath, timeout: time.Duration(p.TimeoutSeconds) * time.Second}
				runErr, cleanupErr := runProcessTree(cmdCtx, cmd, []byte(p.Source), policy)
				if cleanupErr != nil {
					return nil, errors.Join(runErr, cleanupErr)
				}
				exitCode := 0
				if runErr != nil {
					var exited bool
					exitCode, exited = processExitCode(runErr)
					if !exited && cmdCtx.Err() == nil && !errors.Is(runErr, context.DeadlineExceeded) {
						return nil, runErr
					}
				}
				timedOut := errors.Is(runErr, context.DeadlineExceeded)
				capture := map[string]any{"language": p.Language, "stdout": stdout.String(), "stderr": stderr.String(), "exitCode": exitCode, "timedOut": timedOut, "captureComplete": !stdout.truncated && !stderr.truncated}
				captureBytes, marshalErr := json.Marshal(capture)
				if marshalErr != nil {
					return nil, fmt.Errorf("encode complete script output: %w", marshalErr)
				}
				captureSourceID := ""
				if archive != nil {
					captureSourceID, err = archive("process_output", captureBytes)
					if err != nil {
						return nil, fmt.Errorf("archive complete script output: %w", err)
					}
				}
				outStr, outTruncated := balanceTruncate(stdout.String(), 24*1024)
				errStr, errTruncated := balanceTruncate(stderr.String(), 16*1024)
				return map[string]any{
					"language":         p.Language,
					"scriptArtifactId": artifact.ID,
					"captureSourceId":  captureSourceID,
					"captureComplete":  !stdout.truncated && !stderr.truncated,
					"stdout":           outStr,
					"stderr":           errStr,
					"exitCode":         exitCode,
					"timedOut":         timedOut,
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
				"Search workspace files using a regular expression. Skips .git, node_modules, .pnpm-store, and symbolic links by default; explicitly targeted paths can include ignored directories but symbolic links are never followed. Large result sets are archived as a user-scoped source and a turn-local artifact; the archived result retains at most the first 10000 matches, while the artifact can be paged with fs_read during this turn.",
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
					resultSourceID := ""
					if archive != nil {
						resultSourceID, writeErr = archive("grep_results", dumpPayload)
						if writeErr != nil {
							return nil, fmt.Errorf("archive complete grep results: %w", writeErr)
						}
					}
					res["artifactId"] = artifact.ID
					if resultSourceID != "" {
						res["sourceId"] = resultSourceID
					}
					res["artifactTruncated"] = totalFound > len(allMatches)
					if resultSourceID != "" {
						res["notice"] = fmt.Sprintf("Found %d matches. Returning the first %d snippets; the complete retained result is archived as sourceId %s and can be read with axiom_context_source_read. The turn-local artifactId %s is also available to fs_read during this turn.", totalFound, len(matches), resultSourceID, artifact.ID)
					} else {
						res["notice"] = fmt.Sprintf("Found %d matches. Returning the first %d snippets; read artifactId %s with fs_read to page through the retained results during this turn.", totalFound, len(matches), artifact.ID)
					}
				}

				return res, nil
			},
		},
		{
			Definition: toolDef(
				"exec_command",
				"Execute a bounded project command. By default it can read the workspace, write to private temporary storage and any tool cache provided by the selected backend, and has no network access. Set writeAccess=true to let it modify files inside the workspace; set networkAccess=true to request network capability. On Linux, include exact DNS names in networkHosts; only their resolved public IPs are allowed for this invocation. Any port on those IPs is reachable, and the host list is part of the permission request. Workspace-auto sessions allow the requested capabilities unless the operation is classified as destructive or sensitive; request-approval sessions ask before the command runs. "+sandboxDescription+" On Windows, PowerShell terminating errors and the last native command's exit code are reflected in exitCode. Process descendants are terminated; captured output is archived before display truncation and returns captureSourceId. Capture is capped at 1 MiB per stream; captureComplete is false if that cap was reached.",
				`{"type":"object","required":["cmd"],"properties":{"cmd":{"type":"string","description":"Shell command to execute; maximum 1 MiB"},"workdir":{"type":"string","description":"Relative directory to run in (default workspace root)"},"timeoutSeconds":{"type":"integer","description":"Execution timeout in seconds (default 30, max 180)"},"writeAccess":{"type":"boolean","description":"Allow writes inside the workspace for this command; workspace-auto allows it unless classified as risky, request-approval asks first"},"networkAccess":{"type":"boolean","description":"Request network access; on Linux, egress is limited to networkHosts IPs, all ports on those IPs are reachable"},"networkHosts":{"type":"array","maxItems":32,"items":{"type":"string","maxLength":253},"description":"Exact ASCII DNS hostnames (IDNA punycode allowed) allowed for this command on Linux. Required unless the selected repository provides a credential-scoped remote. IP literals, wildcards, private and local destinations are rejected. Any port at the resolved public IP is reachable."}},"additionalProperties":false}`,
			),
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
				if runScope == nil {
					return nil, fmt.Errorf("command execution is available only inside an Agent run")
				}
				var p struct {
					Cmd            string   `json:"cmd"`
					Workdir        string   `json:"workdir"`
					TimeoutSeconds int      `json:"timeoutSeconds"`
					WriteAccess    bool     `json:"writeAccess"`
					NetworkAccess  bool     `json:"networkAccess"`
					NetworkHosts   []string `json:"networkHosts"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return nil, err
				}
				if len(p.NetworkHosts) > 32 || (!p.NetworkAccess && len(p.NetworkHosts) > 0) {
					return nil, errors.New("networkHosts requires networkAccess=true and may list at most 32 destinations")
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
				cmdCtx, cancel := context.WithCancel(ctx)
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
				networkHosts := append(append([]string(nil), p.NetworkHosts...), NetworkHostsFromURLs(execution.GitCredentialURLs)...)
				if err := validateNetworkHostRequest(runtime.GOOS, p.NetworkAccess, networkHosts); err != nil {
					return nil, err
				}
				policy := processPolicy{backend: execution.Backend, installDir: execution.InstallDir, runnerPath: execution.RunnerPath, readOnlyPaths: []string{workspaceRoot}, gitCredentials: execution.GitCredentials, gitCredentialURLs: execution.GitCredentialURLs, protectedPaths: execution.ProtectedPaths, powershellExitWrapper: runtime.GOOS == "windows", journalPath: journalPath, networkAccess: p.NetworkAccess, networkAllowHosts: networkHosts, timeout: time.Duration(p.TimeoutSeconds) * time.Second}
				if p.WriteAccess {
					policy.writePaths = []string{workspaceRoot}
				}
				err, cleanupErr := runProcessTree(cmdCtx, cmd, []byte(p.Cmd), policy)
				if cleanupErr != nil {
					return nil, errors.Join(err, cleanupErr)
				}
				exitCode := 0
				if err != nil {
					var exited bool
					exitCode, exited = processExitCode(err)
					if !exited && cmdCtx.Err() == nil && !errors.Is(err, context.DeadlineExceeded) {
						return nil, err
					}
				}

				timedOut := errors.Is(err, context.DeadlineExceeded)
				capture := map[string]any{"cmd": p.Cmd, "workdir": p.Workdir, "stdout": stdout.String(), "stderr": stderr.String(), "exitCode": exitCode, "timedOut": timedOut, "captureComplete": !stdout.truncated && !stderr.truncated}
				captureBytes, marshalErr := json.Marshal(capture)
				if marshalErr != nil {
					return nil, fmt.Errorf("encode complete command output: %w", marshalErr)
				}
				captureSourceID := ""
				if archive != nil {
					captureSourceID, err = archive("process_output", captureBytes)
					if err != nil {
						return nil, fmt.Errorf("archive complete command output: %w", err)
					}
				}
				outStr, outTruncated := balanceTruncate(stdout.String(), 24*1024)
				errStr, errTruncated := balanceTruncate(stderr.String(), 16*1024)

				return map[string]any{
					"cmd":             p.Cmd,
					"workdir":         p.Workdir,
					"captureSourceId": captureSourceID,
					"captureComplete": !stdout.truncated && !stderr.truncated,
					"stdout":          outStr,
					"stderr":          errStr,
					"exitCode":        exitCode,
					"timedOut":        timedOut,
					"outTruncated":    outTruncated || errTruncated || stdout.truncated || stderr.truncated,
				}, nil
			},
		},
	}
	if execution.BrowserSessions != nil {
		tools = append(tools, execution.BrowserSessions.Tool())
	}
	return tools
}

func linuxSandboxDescription(taskWorkspaceQuotaBytes int64) string {
	diskDescription := "This workspace has no O-managed per-task disk quota; the host filesystem controls its available space."
	if taskWorkspaceQuotaBytes > 0 {
		diskDescription = fmt.Sprintf("This cloud execution-task workspace is backed by an ext4/XFS project quota with a hard limit of %d bytes; the kernel rejects writes above that task limit. The quota applies to workspace files, not shared artifact storage, database, or logs.", taskWorkspaceQuotaBytes)
	}
	return "Linux executes Bubblewrap directly with mount, user, PID, IPC, UTS, and network namespaces. O imposes no per-command CPU, memory, swap or process-count hard limit, and no fixed private-tmpfs size cap; host and filesystem limits still apply. Commands retain their requested timeout, output bounds and process-tree cancellation. Offline execution requires a working Bubblewrap isolation probe, but does not require a systemd user manager or delegated cgroup controllers. The selected workspace is mounted at /workspace. Host O service configuration and credentials under /etc/o-agent are hidden. HTTPS Git credentials are brokered over a private per-command Unix socket only for the exact authorized repository; host Git credential helpers are not inherited. Network-enabled commands still require a separately verified per-destination IP filter; unavailable filtering is an explicit error, never unrestricted host-network execution. " + diskDescription
}

const fsReadInlineArchiveBytes int64 = 32 << 20

type streamedTextSnapshot struct {
	sha256          string
	totalLines      int64
	inline          []byte
	inlineAvailable bool
	fileInfo        os.FileInfo
}

// streamTextSnapshot hashes a file without loading it into memory. Small files
// are retained for inline source archival; larger files remain readable and
// explicitly report that chunked archival is still required.
func streamTextSnapshot(ctx context.Context, file *os.File, inlineLimit int64) (streamedTextSnapshot, error) {
	if file == nil {
		return streamedTextSnapshot{}, fmt.Errorf("file is required")
	}
	info, err := file.Stat()
	if err != nil {
		return streamedTextSnapshot{}, err
	}
	if !info.Mode().IsRegular() {
		return streamedTextSnapshot{}, fmt.Errorf("file is not a regular file")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return streamedTextSnapshot{}, err
	}
	h := sha256.New()
	var inline []byte
	inlineAvailable := info.Size() <= inlineLimit
	if inlineAvailable {
		inline = make([]byte, 0, info.Size())
	}
	buffer := make([]byte, 128*1024)
	var newlines int64
	for {
		if err := ctx.Err(); err != nil {
			return streamedTextSnapshot{}, err
		}
		n, readErr := file.Read(buffer)
		if n > 0 {
			if _, err := h.Write(buffer[:n]); err != nil {
				return streamedTextSnapshot{}, err
			}
			for _, value := range buffer[:n] {
				if value == '\n' {
					newlines++
				}
			}
			if inlineAvailable {
				inline = append(inline, buffer[:n]...)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return streamedTextSnapshot{}, readErr
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return streamedTextSnapshot{}, err
	}
	return streamedTextSnapshot{sha256: hex.EncodeToString(h.Sum(nil)), totalLines: newlines + 1, inline: inline, inlineAvailable: inlineAvailable, fileInfo: info}, nil
}

func readTextPage(ctx context.Context, file *os.File, totalLines int64, offset, limit int, withNumbers bool, maxBytes int) (string, int64, int64, bool, error) {
	if file == nil || totalLines < 0 || offset < 1 || limit < 1 || maxBytes < 1 {
		return "", 0, 0, false, fmt.Errorf("invalid text page request")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, 0, false, err
	}
	start := int64(offset)
	if start > totalLines {
		return "", start, totalLines, false, nil
	}
	remainingLines := totalLines - start + 1
	pageLines := int64(limit)
	if pageLines > remainingLines {
		pageLines = remainingLines
	}
	requestedEnd := start + pageLines - 1
	reader := bufio.NewReaderSize(file, 64*1024)
	var content strings.Builder
	var lineNo int64 = 1
	var endLine int64 = start - 1
	truncated := false
	for lineNo <= requestedEnd {
		if err := ctx.Err(); err != nil {
			return "", 0, 0, false, err
		}
		selected := lineNo >= start
		prefix := ""
		if selected && withNumbers {
			prefix = fmt.Sprintf("%6d | ", lineNo)
		}
		remaining := maxBytes - content.Len() - len(prefix)
		if content.Len() > 0 {
			remaining--
		}
		if remaining < 0 {
			truncated = true
			break
		}
		line, lineTruncated, gotLine, err := readBoundedLine(ctx, reader, remaining, selected)
		if err != nil {
			return "", 0, 0, false, err
		}
		if !gotLine {
			if lineNo != totalLines {
				break
			}
			line = []byte{} // Preserve strings.Split's final empty line.
		}
		if selected {
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if content.Len() > 0 {
				content.WriteByte('\n')
			}
			content.WriteString(prefix)
			content.Write(line)
			endLine = lineNo
			if lineTruncated {
				truncated = true
				break
			}
		}
		if !gotLine {
			break
		}
		lineNo++
	}
	if endLine < start {
		endLine = start - 1
	}
	return content.String(), start, endLine, truncated, nil
}

// readBoundedLine drains one line while retaining at most keepBytes. This
// prevents a very long generated/minified line from causing an unbounded allocation.
func readBoundedLine(ctx context.Context, reader *bufio.Reader, keepBytes int, retain bool) ([]byte, bool, bool, error) {
	if keepBytes < 0 {
		keepBytes = 0
	}
	capacity := 0
	if retain {
		capacity = min(keepBytes, 64*1024)
	}
	line := make([]byte, 0, capacity)
	truncated := false
	seen := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, false, err
		}
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			seen = true
		}
		part := fragment
		complete := err == nil
		if complete {
			part = part[:len(part)-1]
		}
		if retain && len(part) > 0 {
			room := keepBytes - len(line)
			if room > 0 {
				if room > len(part) {
					room = len(part)
				}
				line = append(line, part[:room]...)
			}
			if room < len(part) {
				truncated = true
			}
		}
		if complete {
			return line, truncated, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return line, truncated, seen, nil
		}
		return nil, false, false, err
	}
}

// RunSandboxedCommand executes a host-initiated command through the same
// backend and filesystem/network policy path used by exec_command. It is for
// explicit product operations such as cloning a selected repository; model
// tool arguments must never control the ExecutionConfig.
func RunSandboxedCommand(ctx context.Context, execution ExecutionConfig, commandPath string, args []string, workdir string, readOnlyPaths, writePaths []string, networkAccess bool, timeout time.Duration) (stdout, stderr string, runErr, cleanupErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(commandPath) == "" || strings.TrimSpace(workdir) == "" {
		return "", "", errors.New("sandbox command path and working directory are required"), nil
	}
	if timeout <= 0 || timeout > 180*time.Second {
		return "", "", errors.New("sandbox command timeout must be between 1 ms and 180 seconds"), nil
	}
	command := exec.CommandContext(ctx, commandPath, args...)
	command.Dir = workdir
	stdoutCapture, stderrCapture := cappedBuffer{limit: 1 << 20}, cappedBuffer{limit: 1 << 20}
	command.Stdout, command.Stderr = &stdoutCapture, &stderrCapture
	policy := processPolicy{
		backend: execution.Backend, installDir: execution.InstallDir, runnerPath: execution.RunnerPath,
		readOnlyPaths: append([]string(nil), readOnlyPaths...), writePaths: append([]string(nil), writePaths...),
		networkAccess: networkAccess, networkAllowHosts: NetworkHostsFromURLs(execution.GitCredentialURLs), timeout: timeout,
		gitCredentials:    execution.GitCredentials,
		gitCredentialURLs: append([]string(nil), execution.GitCredentialURLs...),
		protectedPaths:    append([]string(nil), execution.ProtectedPaths...),
	}
	runErr, cleanupErr = runProcessTree(ctx, command, nil, policy)
	return stdoutCapture.String(), stderrCapture.String(), runErr, cleanupErr
}

func NetworkHostsFromURLs(values []string) []string {
	hosts := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed == nil || parsed.Hostname() == "" {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if _, exists := seen[host]; exists {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	return hosts
}

func validateNetworkHostRequest(goos string, networkAccess bool, hosts []string) error {
	if !networkAccess && len(hosts) > 0 {
		return errors.New("networkHosts requires networkAccess=true")
	}
	if len(hosts) > 32 {
		return errors.New("networkHosts may list at most 32 destinations")
	}
	if goos == "linux" && networkAccess && len(hosts) == 0 {
		return errors.New("Linux network access requires at least one networkHosts destination or a selected repository remote")
	}
	return nil
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
