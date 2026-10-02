//go:build windows

package sandbox

import (
	"errors"
	"testing"
)

func TestPerformWFPTransactionPropagatesAbortFailure(t *testing.T) {
	applyFailure := errors.New("filter write failed")
	abortFailure := errors.New("transaction abort failed")
	var calls []string
	err := performWFPTransaction(
		func() error { calls = append(calls, "begin"); return nil },
		func() error { calls = append(calls, "apply"); return applyFailure },
		func() error { calls = append(calls, "commit"); return nil },
		func() error { calls = append(calls, "abort"); return abortFailure },
	)
	if !errors.Is(err, applyFailure) || !errors.Is(err, abortFailure) {
		t.Fatalf("transaction result lost the apply or abort failure: %v", err)
	}
	if got := len(calls); got != 3 || calls[0] != "begin" || calls[1] != "apply" || calls[2] != "abort" {
		t.Fatalf("unexpected calls after apply failure: %v", calls)
	}
}

func TestPerformWFPTransactionAbortsCommitFailure(t *testing.T) {
	commitFailure := errors.New("transaction commit failed")
	abortFailure := errors.New("transaction abort failed")
	var calls []string
	err := performWFPTransaction(
		func() error { calls = append(calls, "begin"); return nil },
		func() error { calls = append(calls, "apply"); return nil },
		func() error { calls = append(calls, "commit"); return commitFailure },
		func() error { calls = append(calls, "abort"); return abortFailure },
	)
	if !errors.Is(err, commitFailure) || !errors.Is(err, abortFailure) {
		t.Fatalf("transaction result lost the commit or abort failure: %v", err)
	}
	if got := len(calls); got != 4 || calls[0] != "begin" || calls[1] != "apply" || calls[2] != "commit" || calls[3] != "abort" {
		t.Fatalf("unexpected calls after commit failure: %v", calls)
	}
}

func TestPerformWFPTransactionDoesNotAbortWhenBeginFails(t *testing.T) {
	beginFailure := errors.New("transaction begin failed")
	var calls []string
	err := performWFPTransaction(
		func() error { calls = append(calls, "begin"); return beginFailure },
		func() error { calls = append(calls, "apply"); return nil },
		func() error { calls = append(calls, "commit"); return nil },
		func() error { calls = append(calls, "abort"); return nil },
	)
	if !errors.Is(err, beginFailure) {
		t.Fatalf("transaction result lost begin failure: %v", err)
	}
	if len(calls) != 1 || calls[0] != "begin" {
		t.Fatalf("transaction continued after begin failure: %v", calls)
	}
}
