//go:build windows

package sandbox

import (
	"encoding/base64"
	"errors"
)

// wrapPowerShellInput runs the caller's source in a script block so the
// PowerShell host can report terminating errors and the final native command's
// exit code to the runner. Base64 keeps arbitrary source text from changing the
// wrapper's syntax.
func wrapPowerShellInput(input []byte) ([]byte, error) {
	if len(input) > 1<<20 {
		return nil, errors.New("PowerShell input exceeds the 1 MiB limit")
	}
	encoded := base64.StdEncoding.EncodeToString(input)
	script := "$ErrorActionPreference = 'Stop'\n" +
		"$global:LASTEXITCODE = 0\n" +
		"$__oagentExitCode = 0\n" +
		"try {\n" +
		"  $__oagentSource = [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String('" + encoded + "'))\n" +
		"  & ([ScriptBlock]::Create($__oagentSource))\n" +
		"  if ($null -ne $global:LASTEXITCODE) { $__oagentExitCode = [int]$global:LASTEXITCODE }\n" +
		"} catch {\n" +
		"  [Console]::Error.WriteLine($_.ToString())\n" +
		"  $__oagentExitCode = 1\n" +
		"}\n" +
		"exit $__oagentExitCode\n"
	return []byte(script), nil
}
