//go:build windows

package sandbox

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

func runGitCredentialHelper(args []string) (resultErr error) {
	operation := "get"
	pipeName, commandID := "", ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "get", "store", "erase":
			operation = args[i]
		case "--pipe":
			if i+1 >= len(args) {
				return errors.New("credential helper pipe is missing")
			}
			i++
			pipeName = args[i]
		case "--command-id":
			if i+1 >= len(args) {
				return errors.New("credential helper command ID is missing")
			}
			i++
			commandID = args[i]
		default:
			return errors.New("credential helper received an unknown argument")
		}
	}
	if operation == "store" || operation == "erase" {
		input, err := io.ReadAll(io.LimitReader(os.Stdin, (64<<10)+1))
		if err != nil {
			return fmt.Errorf("read Git helper input: %w", err)
		}
		defer clear(input)
		if len(input) > 64<<10 {
			return errors.New("Git helper input exceeded its limit")
		}
		return nil
	}
	if !strings.HasPrefix(pipeName, `\\.\pipe\OAgentCredential-`) || commandID == "" {
		return errors.New("credential helper has invalid command channel")
	}
	values := map[string]string{}
	scanner := bufio.NewScanner(io.LimitReader(os.Stdin, 64<<10))
	scanner.Buffer(make([]byte, 1024), 8192)
	terminated := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			terminated = true
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.ContainsAny(key+value, "\x00\r\n") {
			return errors.New("credential helper input is invalid")
		}
		if key != "protocol" && key != "host" && key != "path" && key != "username" {
			return errors.New("credential helper input contains an unsupported field")
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !terminated {
		return errors.New("credential helper input is incomplete")
	}
	if values["protocol"] != "https" || values["host"] == "" || values["path"] == "" || strings.Contains(values["host"], "@") {
		return errors.New("credential helper only accepts HTTPS repository requests")
	}
	requestURL := "https://" + values["host"] + "/" + strings.TrimLeft(values["path"], "/")
	pipe16, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	var handle windows.Handle
	for time.Now().Before(deadline) {
		handle, err = windows.CreateFile(pipe16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			break
		}
		if err := waitForNativeNamedPipe(pipe16, 200); err != nil {
			return fmt.Errorf("wait for Git credential broker: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if handle == 0 {
		return fmt.Errorf("connect Git credential broker: %w", err)
	}
	pipe := os.NewFile(uintptr(handle), "oagent-git-credential-broker")
	defer func() { resultErr = errors.Join(resultErr, pipe.Close()) }()
	if err := writeNativeFrame(pipe, nativeFrame{Version: nativeProtocolVersion, Type: "credential-request", CommandID: commandID, URL: requestURL}); err != nil {
		return err
	}
	response, err := readNativeFrame(pipe)
	if err != nil {
		return err
	}
	if response.Version != nativeProtocolVersion || response.Type != "credential-response" || response.CommandID != commandID || response.Error != "" {
		return errors.New("Git credential broker did not authorize this repository")
	}
	if response.Username == "" || response.Password == "" || strings.ContainsAny(response.Username+response.Password, "\r\n\x00") {
		return errors.New("Git credential broker returned an invalid response")
	}
	defer zeroString(&response.Password)
	if _, err := fmt.Fprintf(os.Stdout, "username=%s\npassword=%s\n\n", response.Username, response.Password); err != nil {
		return err
	}
	return nil
}
