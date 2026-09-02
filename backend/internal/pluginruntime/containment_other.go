//go:build !windows

package pluginruntime

import "os/exec"

type processContainment struct{}

func containProcess(*exec.Cmd) (*processContainment, error) { return &processContainment{}, nil }
func (*processContainment) Close() error                    { return nil }
