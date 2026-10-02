//go:build !linux

package projectquota

import (
	"context"
	"errors"
)

type Manager interface {
	Health(context.Context) (ProbeResult, error)
	Apply(context.Context, string, uint32, int64) (WorkspaceQuota, error)
	Inspect(context.Context, string, uint32) (WorkspaceQuota, error)
	Release(context.Context, string, uint32) (WorkspaceQuota, error)
}

type HelperClient struct{}

func NewHelperClient(string) (*HelperClient, error) {
	return nil, errors.New("kernel project quota helper is supported only on Linux")
}

func (*HelperClient) Health(context.Context) (ProbeResult, error) {
	return ProbeResult{}, errors.New("kernel project quota helper is supported only on Linux")
}

func (*HelperClient) Apply(context.Context, string, uint32, int64) (WorkspaceQuota, error) {
	return WorkspaceQuota{}, errors.New("kernel project quota helper is supported only on Linux")
}

func (*HelperClient) Inspect(context.Context, string, uint32) (WorkspaceQuota, error) {
	return WorkspaceQuota{}, errors.New("kernel project quota helper is supported only on Linux")
}

func (*HelperClient) Release(context.Context, string, uint32) (WorkspaceQuota, error) {
	return WorkspaceQuota{}, errors.New("kernel project quota helper is supported only on Linux")
}
