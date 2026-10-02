//go:build !linux

package browsercontrol

import (
	"errors"
	"io"
)

func RunWorker(string, io.Reader, io.Writer) error {
	return errors.New("browser worker is supported only inside the Linux sandbox")
}
