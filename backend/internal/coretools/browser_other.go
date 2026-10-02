//go:build !linux

package coretools

func NewBrowserSessions(ExecutionConfig) (BrowserSessions, error) { return nil, nil }
