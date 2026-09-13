package scriptruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/dop251/goja"
)

const WorkerEnvironment = "O_SCRIPT_WORKER"

type workerRequest struct {
	Program       string          `json:"program"`
	Input         json.RawMessage `json:"input"`
	TimeoutMillis int             `json:"timeoutMillis"`
	MaxOutputKiB  int             `json:"maxOutputKiB"`
}

type workerResponse struct {
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

func IsWorker() bool { return os.Getenv(WorkerEnvironment) == "1" }

func RunWorker(input io.Reader, output io.Writer) int {
	decoder := json.NewDecoder(io.LimitReader(input, 128<<10))
	var request workerRequest
	if err := decoder.Decode(&request); err != nil {
		_ = json.NewEncoder(output).Encode(workerResponse{Error: "invalid worker request: " + err.Error()})
		return 2
	}
	result, err := executeProgram(request)
	response := workerResponse{Output: result}
	if err != nil {
		response.Output = nil
		response.Error = err.Error()
	}
	if encodeErr := json.NewEncoder(output).Encode(response); encodeErr != nil {
		return 3
	}
	if err != nil {
		return 1
	}
	return 0
}

func executeProgram(request workerRequest) (json.RawMessage, error) {
	if strings.TrimSpace(request.Program) == "" || len(request.Program) > 32<<10 {
		return nil, errors.New("program must be between 1 byte and 32 KiB")
	}
	if !json.Valid(request.Input) {
		return nil, errors.New("input must be valid JSON")
	}
	if request.TimeoutMillis < 1 || request.TimeoutMillis > 10000 {
		return nil, errors.New("worker timeout is outside the allowed range")
	}
	if request.MaxOutputKiB < 1 || request.MaxOutputKiB > 1024 {
		return nil, errors.New("worker output limit is outside the allowed range")
	}
	var value any
	if err := json.Unmarshal(request.Input, &value); err != nil {
		return nil, err
	}
	runtime := goja.New()
	if err := runtime.Set("__o_input", value); err != nil {
		return nil, err
	}
	timer := time.AfterFunc(time.Duration(request.TimeoutMillis)*time.Millisecond, func() {
		runtime.Interrupt("execution deadline exceeded")
	})
	defer timer.Stop()
	source := "(function(input){\n\"use strict\";\n" + request.Program + "\n})(__o_input)"
	result, err := runtime.RunString(source)
	if err != nil {
		var interrupted *goja.InterruptedError
		if errors.As(err, &interrupted) {
			return nil, errors.New("script execution deadline exceeded")
		}
		return nil, fmt.Errorf("script execution failed: %w", err)
	}
	exported := result.Export()
	raw, err := json.Marshal(exported)
	if err != nil {
		return nil, fmt.Errorf("script result is not JSON serializable: %w", err)
	}
	if len(raw) > request.MaxOutputKiB<<10 {
		return nil, errors.New("script result exceeds the output limit")
	}
	return raw, nil
}
