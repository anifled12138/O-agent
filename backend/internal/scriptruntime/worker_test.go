package scriptruntime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"axiom.local/agent/internal/capability"
)

func TestMain(m *testing.M) {
	if IsWorker() {
		os.Exit(RunWorker(os.Stdin, os.Stdout))
	}
	os.Exit(m.Run())
}

func TestExecuteProgramTransformsJSON(t *testing.T) {
	output, err := executeProgram(workerRequest{
		Program: `return {total: input.values.reduce((sum, value) => sum + value, 0)};`,
		Input:   json.RawMessage(`{"values":[2,3,5]}`), TimeoutMillis: 500, MaxOutputKiB: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"total":10}` {
		t.Fatalf("unexpected output %s", output)
	}
}

func TestExecuteProgramInterruptsInfiniteLoop(t *testing.T) {
	_, err := executeProgram(workerRequest{Program: `for (;;) {}`, Input: json.RawMessage(`{}`), TimeoutMillis: 10, MaxOutputKiB: 16})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected deadline error, got %v", err)
	}
}

func TestWorkerDoesNotExposeHostAPIs(t *testing.T) {
	output, err := executeProgram(workerRequest{
		Program: `return {require: typeof require, process: typeof process, fetch: typeof fetch};`,
		Input:   json.RawMessage(`{}`), TimeoutMillis: 500, MaxOutputKiB: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(output), `"function"`) || strings.Contains(string(output), `"object"`) {
		t.Fatalf("host API leaked into worker: %s", output)
	}
}

func TestSupervisorRunsProgramOutOfProcess(t *testing.T) {
	output, _, err := New().Execute(context.Background(), `return {value: input.value * 2};`, json.RawMessage(`{"value":21}`), capability.Limits{TimeoutMillis: 1000, MemoryMiB: 64, MaxOutputKiB: 16})
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != `{"value":42}` {
		t.Fatalf("unexpected output %s", output)
	}
	started := time.Now()
	_, _, err = New().Execute(context.Background(), `for (;;) {}`, json.RawMessage(`{}`), capability.Limits{TimeoutMillis: 20, MemoryMiB: 64, MaxOutputKiB: 16})
	if err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("worker did not terminate promptly: %v", err)
	}
}
