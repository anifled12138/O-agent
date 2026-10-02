//go:build !linux

package sandbox

import (
	"errors"
	"io"
)

func RunGitCredentialHelper(_ []string, _ io.Reader, _ io.Writer) error {
	return errors.New("Unix-socket Git credential helper is available only on Linux")
}
