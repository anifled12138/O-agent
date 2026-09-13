package scriptruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"axiom.local/agent/internal/capability"
)

type Supervisor struct{}

func New() *Supervisor { return &Supervisor{} }

func (s *Supervisor) Execute(ctx context.Context, program string, input json.RawMessage, limits capability.Limits) (json.RawMessage, time.Duration, error) {
	limits = limits.Normalized()
	request := workerRequest{Program: program, Input: input, TimeoutMillis: limits.TimeoutMillis, MaxOutputKiB: limits.MaxOutputKiB}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, 0, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, 0, err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(limits.TimeoutMillis+1000)*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(runCtx, executable)
	command.Env = []string{WorkerEnvironment + "=1"}
	command.Stdin = bytes.NewReader(raw)
	var stdout limitedBuffer
	stdout.limit = (limits.MaxOutputKiB + 16) << 10
	var stderr limitedBuffer
	stderr.limit = 32 << 10
	command.Stdout = &stdout
	command.Stderr = &stderr
	started := time.Now()
	if err = command.Start(); err != nil {
		return nil, 0, err
	}
	containment, err := containProcess(command, limits.MemoryMiB)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return nil, 0, fmt.Errorf("apply script worker containment: %w", err)
	}
	waitErr := command.Wait()
	_ = containment.Close()
	duration := time.Since(started)
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, duration, errors.New("script worker deadline exceeded")
	}
	var response workerResponse
	if err = json.Unmarshal(stdout.Bytes(), &response); err != nil {
		message := stderr.String()
		if message == "" {
			message = waitError(waitErr)
		}
		return nil, duration, fmt.Errorf("invalid script worker response: %s", message)
	}
	if response.Error != "" {
		return nil, duration, errors.New(response.Error)
	}
	if waitErr != nil {
		return nil, duration, fmt.Errorf("script worker failed: %s", waitError(waitErr))
	}
	if !json.Valid(response.Output) {
		return nil, duration, errors.New("script worker returned invalid JSON")
	}
	return response.Output, duration, nil
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		_, _ = b.buffer.Write(value[:remaining])
	}
	return len(value), nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buffer.Bytes() }
func (b *limitedBuffer) String() string { return b.buffer.String() }

func waitError(err error) string {
	if err == nil {
		return "worker exited without a response"
	}
	return err.Error()
}
