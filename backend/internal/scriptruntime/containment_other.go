//go:build !windows

package scriptruntime

import "os/exec"

type processContainment struct{}

func containProcess(*exec.Cmd, int) (*processContainment, error) { return &processContainment{}, nil }
func (*processContainment) Close() error                         { return nil }
