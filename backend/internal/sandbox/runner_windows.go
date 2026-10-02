//go:build windows

package sandbox

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	nativeProtocolVersion = 2
	maxNativeFrame        = 4 << 20
	maxNativeInput        = 2 << 20 // Includes the PowerShell exit-status wrapper.
	nativeChunkSize       = 24 << 10
	createRestrictedFlags = 0x01 | 0x04 | 0x08 // DISABLE_MAX_PRIVILEGE | LUA_TOKEN | WRITE_RESTRICTED
)

var (
	procCreateRestrictedToken = windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
	procCreateDesktop         = windows.NewLazySystemDLL("user32.dll").NewProc("CreateDesktopW")
	procCloseDesktop          = windows.NewLazySystemDLL("user32.dll").NewProc("CloseDesktop")
)

type nativeFrame struct {
	Version           int                  `json:"version"`
	Type              string               `json:"type"`
	CommandID         string               `json:"commandId"`
	ProcessID         uint32               `json:"processId,omitempty"`
	ProcessStart      uint64               `json:"processStart,omitempty"`
	UserSID           string               `json:"userSid,omitempty"`
	LogonSID          string               `json:"logonSid,omitempty"`
	RestrictedSIDs    []string             `json:"restrictedSids,omitempty"`
	EnabledPrivileges []string             `json:"enabledPrivileges,omitempty"`
	Runner            string               `json:"runner,omitempty"`
	Plan              *nativeExecutionPlan `json:"plan,omitempty"`
	Stream            string               `json:"stream,omitempty"`
	Data              []byte               `json:"data,omitempty"`
	ExitCode          uint32               `json:"exitCode,omitempty"`
	TimedOut          bool                 `json:"timedOut,omitempty"`
	Error             string               `json:"error,omitempty"`
	URL               string               `json:"url,omitempty"`
	Username          string               `json:"username,omitempty"`
	Password          string               `json:"password,omitempty"`
}

type nativeRunnerArgs struct {
	Pipe      string
	CommandID string
	Probe     bool
}

func RunCommandRunner(args []string) int {
	if helperArgs, ok := nativeGitCredentialHelperArgs(os.Args[0], args); ok {
		if err := runGitCredentialHelper(helperArgs); err != nil {
			fmt.Fprintln(os.Stderr, "Git credential broker request failed")
			return 1
		}
		return 0
	}
	if len(args) > 0 && args[0] == "--git-credential-helper" {
		if err := runGitCredentialHelper(args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "Git credential broker request failed")
			return 1
		}
		return 0
	}
	parsed, err := parseRunnerArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	if parsed.Probe {
		if err := runnerProbe(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if err := runCommandRunner(parsed); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func parseRunnerArgs(args []string) (nativeRunnerArgs, error) {
	var parsed nativeRunnerArgs
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--probe":
			parsed.Probe = true
		case "--pipe":
			if i+1 >= len(args) {
				return parsed, errors.New("runner missing pipe name")
			}
			i++
			parsed.Pipe = args[i]
		case "--command-id":
			if i+1 >= len(args) {
				return parsed, errors.New("runner missing command ID")
			}
			i++
			parsed.CommandID = args[i]
		default:
			return parsed, fmt.Errorf("runner received an unknown option %q", args[i])
		}
	}
	if parsed.Probe {
		if parsed.Pipe != "" || parsed.CommandID != "" {
			return parsed, errors.New("runner Probe accepts no command arguments")
		}
		return parsed, nil
	}
	if parsed.Pipe == "" || parsed.CommandID == "" || strings.ContainsAny(parsed.CommandID, "\\/:\x00") {
		return parsed, errors.New("runner requires a pipe and command ID")
	}
	if !strings.HasPrefix(parsed.Pipe, `\\.\pipe\OAgentSandbox-`) {
		return parsed, errors.New("runner pipe name is outside the O Agent namespace")
	}
	return parsed, nil
}

func runnerProbe() (resultErr error) {
	restricted, _, logonSID, err := createNativeRestrictedToken()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(windows.Handle(restricted))) }()
	if logonSID == nil || !logonSID.IsValid() {
		return errors.New("runner Probe could not read its logon SID")
	}
	isRestricted, err := restricted.IsRestricted()
	if err != nil || !isRestricted {
		return errors.Join(errors.New("runner Probe token is not restricted"), err)
	}
	if err := verifyNULAccess(restricted); err != nil {
		return fmt.Errorf("restricted-token NUL compatibility Probe failed: %w", err)
	}
	if err := probeRestrictedProcess(restricted); err != nil {
		return fmt.Errorf("restricted-token process creation Probe failed: %w", err)
	}
	return nil
}

func runCommandRunner(args nativeRunnerArgs) (resultErr error) {
	pipe, err := connectRunnerPipe(args.Pipe)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, pipe.Close()) }()
	currentToken := windows.GetCurrentProcessToken()
	user, err := currentToken.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read runner account identity: %w", err)
	}
	groups, err := currentToken.GetTokenGroups()
	if err != nil {
		return fmt.Errorf("read runner logon session: %w", err)
	}
	logonSID := tokenLogonSID(groups)
	if logonSID == nil {
		return errors.New("runner token has no logon SID")
	}
	restricted, accountSID, restrictedLogonSID, err := createNativeRestrictedToken()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(windows.Handle(restricted))) }()
	if !accountSID.Equals(user.User.Sid) || !restrictedLogonSID.Equals(logonSID) {
		return errors.New("restricted token identity does not match the runner logon session")
	}
	if err := verifyNULAccess(restricted); err != nil {
		return fmt.Errorf("restricted-token NUL compatibility Probe failed: %w", err)
	}
	if err := probeRestrictedProcess(restricted); err != nil {
		return fmt.Errorf("restricted-token process creation Probe failed: %w", err)
	}
	everyoneSID, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		return err
	}
	creation, err := processCreationTime(windows.CurrentProcess())
	if err != nil {
		return fmt.Errorf("read runner process start time: %w", err)
	}
	processStart := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	if err := writeNativeFrame(pipe, nativeFrame{
		Version: nativeProtocolVersion, Type: "hello", CommandID: args.CommandID,
		ProcessID: uint32(os.Getpid()), ProcessStart: processStart, UserSID: user.User.Sid.String(), LogonSID: logonSID.String(),
		RestrictedSIDs:    []string{accountSID.String(), logonSID.String(), everyoneSID.String()},
		EnabledPrivileges: []string{"SeChangeNotifyPrivilege"},
	}); err != nil {
		return fmt.Errorf("send runner handshake: %w", err)
	}
	start, err := readNativeFrame(pipe)
	if err != nil {
		return fmt.Errorf("read host execution authorization: %w", err)
	}
	if start.Version != nativeProtocolVersion || start.Type != "start" || start.CommandID != args.CommandID || start.Plan == nil {
		return errors.New("host sent an invalid execution authorization frame")
	}
	plan := start.Plan
	if plan.Version != nativeProtocolVersion || plan.CommandID != args.CommandID || plan.InstallationVersion == "" || plan.CommandPath == "" || !filepath.IsAbs(plan.CommandPath) || !filepath.IsAbs(plan.WorkingDirectory) || len(plan.Input) > maxNativeInput || plan.TimeoutMS <= 0 || plan.TimeoutMS > int64((180*time.Second)/time.Millisecond) {
		return errors.New("host execution plan is invalid or exceeds the protocol limit")
	}
	if plan.GitCredentialPipe != "" && (!plan.NetworkAccess || !strings.HasPrefix(plan.GitCredentialPipe, `\\.\pipe\OAgentCredential-`)) {
		return errors.New("host execution plan contains an invalid Git credential channel")
	}
	if err := runRestrictedCommand(pipe, plan, restricted, logonSID); err != nil {
		frameErr := writeNativeFrame(pipe, nativeFrame{Version: nativeProtocolVersion, Type: "error", CommandID: args.CommandID, Error: err.Error()})
		return errors.Join(err, frameErr)
	}
	return nil
}

func connectRunnerPipe(name string) (*os.File, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		h, err := windows.CreateFile(name16, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			return os.NewFile(uintptr(h), "axiom-sandbox-pipe"), nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("connect to host control pipe: %w", err)
		}
		if err := waitForNativeNamedPipe(name16, 250); err != nil {
			return nil, fmt.Errorf("wait for host control pipe: %w", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func writeNativeFrame(w io.Writer, frame nativeFrame) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if frame.Type == "credential-response" {
		defer clear(data)
	}
	if len(data) > maxNativeFrame {
		return errors.New("native sandbox protocol frame exceeds its size limit")
	}
	var header [4]byte
	binary.LittleEndian.PutUint32(header[:], uint32(len(data)))
	if err := writeFull(w, header[:]); err != nil {
		return err
	}
	return writeFull(w, data)
}

func readNativeFrame(r io.Reader) (nativeFrame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nativeFrame{}, err
	}
	length := binary.LittleEndian.Uint32(header[:])
	if length == 0 || length > maxNativeFrame {
		return nativeFrame{}, fmt.Errorf("invalid native sandbox frame length %d", length)
	}
	data := make([]byte, int(length))
	if _, err := io.ReadFull(r, data); err != nil {
		return nativeFrame{}, err
	}
	var frame nativeFrame
	defer func() {
		if len(data) > 0 {
			clear(data)
		}
	}()
	if err := json.Unmarshal(data, &frame); err != nil {
		return nativeFrame{}, fmt.Errorf("decode native sandbox frame: %w", err)
	}
	return frame, nil
}

func writeFull(w io.Writer, value []byte) error {
	for len(value) > 0 {
		n, err := w.Write(value)
		if n > 0 {
			value = value[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func createNativeRestrictedToken() (restrictedToken windows.Token, accountSID, logonSID *windows.SID, resultErr error) {
	access := uint32(windows.TOKEN_DUPLICATE | windows.TOKEN_QUERY | windows.TOKEN_ASSIGN_PRIMARY | windows.TOKEN_ADJUST_DEFAULT | windows.TOKEN_ADJUST_SESSIONID | windows.TOKEN_ADJUST_PRIVILEGES)
	var base windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), access, &base); err != nil {
		return 0, nil, nil, fmt.Errorf("open runner process token: %w", err)
	}
	closeBase := true
	defer func() {
		if closeBase {
			closeErr := windows.CloseHandle(windows.Handle(base))
			resultErr = errors.Join(resultErr, closeErr)
			if closeErr != nil && restrictedToken != 0 {
				resultErr = errors.Join(resultErr, windows.CloseHandle(windows.Handle(restrictedToken)))
				restrictedToken = 0
			}
		}
	}()
	user, err := base.GetTokenUser()
	if err != nil {
		return 0, nil, nil, err
	}
	groups, err := base.GetTokenGroups()
	if err != nil {
		return 0, nil, nil, err
	}
	logonSID = tokenLogonSID(groups)
	if logonSID == nil {
		return 0, nil, nil, errors.New("runner access token is missing a logon SID")
	}
	everyoneSID, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		return 0, nil, nil, err
	}
	accountSID, err = windows.StringToSid(user.User.Sid.String())
	if err != nil {
		return 0, nil, nil, err
	}
	restricting := []windows.SIDAndAttributes{{Sid: accountSID}, {Sid: logonSID}, {Sid: everyoneSID}}
	var restricted windows.Token
	r1, _, callErr := procCreateRestrictedToken.Call(uintptr(base), createRestrictedFlags, 0, 0, 0, 0, uintptr(len(restricting)), uintptr(unsafe.Pointer(&restricting[0])), uintptr(unsafe.Pointer(&restricted)))
	runtimeKeepAliveWFP(restricting, accountSID, logonSID, everyoneSID, user, groups)
	if r1 == 0 {
		return 0, nil, nil, fmt.Errorf("CreateRestrictedToken: %w", callErr)
	}
	if err := setRestrictedTokenDefaultDACL(restricted, logonSID); err != nil {
		return 0, nil, nil, errors.Join(err, windows.CloseHandle(windows.Handle(restricted)))
	}
	if err := enableChangeNotifyPrivilege(restricted); err != nil {
		return 0, nil, nil, errors.Join(err, windows.CloseHandle(windows.Handle(restricted)))
	}
	if err := verifyNativeRestrictedToken(restricted, accountSID, logonSID, everyoneSID); err != nil {
		return 0, nil, nil, errors.Join(err, windows.CloseHandle(windows.Handle(restricted)))
	}
	return restricted, accountSID, logonSID, nil
}

func verifyNativeRestrictedToken(token windows.Token, accountSID, logonSID, everyoneSID *windows.SID) error {
	if token.IsElevated() {
		return errors.New("restricted sandbox token is unexpectedly elevated")
	}
	adminSID, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return err
	}
	groups, err := token.GetTokenGroups()
	if err != nil {
		return fmt.Errorf("read restricted sandbox token groups: %w", err)
	}
	for _, group := range groups.AllGroups() {
		if group.Sid != nil && group.Sid.Equals(adminSID) && group.Attributes&windows.SE_GROUP_ENABLED != 0 {
			return errors.New("restricted sandbox token retains enabled Administrators membership")
		}
	}

	restrictedInfo, err := tokenInformation(token, windows.TokenRestrictedSids)
	if err != nil {
		return fmt.Errorf("read restricted SID set: %w", err)
	}
	restrictedGroups := (*windows.Tokengroups)(unsafe.Pointer(&restrictedInfo[0]))
	if restrictedGroups.GroupCount > 32 {
		return fmt.Errorf("restricted sandbox token has an unexpected %d restricting SIDs", restrictedGroups.GroupCount)
	}
	expected := map[string]bool{accountSID.String(): true, logonSID.String(): true, everyoneSID.String(): true}
	actualCount := 0
	for _, group := range restrictedGroups.AllGroups() {
		if group.Sid == nil || !group.Sid.IsValid() || !expected[group.Sid.String()] {
			return errors.New("restricted sandbox token contains an unapproved restricting SID")
		}
		delete(expected, group.Sid.String())
		actualCount++
	}
	if actualCount != 3 || len(expected) != 0 {
		return errors.New("restricted sandbox token does not have the exact account, logon, and compatibility restricting SID set")
	}

	privilegeInfo, err := tokenInformation(token, windows.TokenPrivileges)
	if err != nil {
		return fmt.Errorf("read restricted sandbox token privileges: %w", err)
	}
	privileges := (*windows.Tokenprivileges)(unsafe.Pointer(&privilegeInfo[0]))
	if privileges.PrivilegeCount > 128 {
		return fmt.Errorf("restricted sandbox token has an unexpected %d privileges", privileges.PrivilegeCount)
	}
	changeNotifyName, err := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	if err != nil {
		return err
	}
	var changeNotify windows.LUID
	if err := windows.LookupPrivilegeValue(nil, changeNotifyName, &changeNotify); err != nil {
		return fmt.Errorf("resolve required directory traversal privilege: %w", err)
	}
	changeNotifyEnabled := false
	for _, privilege := range privileges.AllPrivileges() {
		if privilege.Attributes&windows.SE_PRIVILEGE_ENABLED == 0 {
			continue
		}
		if privilege.Luid != changeNotify {
			return fmt.Errorf("restricted sandbox token retains unexpected enabled privilege LUID %d:%d", privilege.Luid.HighPart, privilege.Luid.LowPart)
		}
		changeNotifyEnabled = true
	}
	if !changeNotifyEnabled {
		return errors.New("restricted sandbox token is missing required SeChangeNotifyPrivilege")
	}
	return verifyRestrictedDefaultDACL(token, logonSID)
}

func tokenInformation(token windows.Token, class uint32) ([]byte, error) {
	var size uint32
	err := windows.GetTokenInformation(token, class, nil, 0, &size)
	if !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) || size == 0 || size > 1<<20 {
		return nil, fmt.Errorf("query token information size: %w", err)
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, class, &buffer[0], size, &size); err != nil {
		return nil, err
	}
	if size == 0 || size > uint32(len(buffer)) {
		return nil, errors.New("token information returned an invalid length")
	}
	return buffer[:size], nil
}

func verifyRestrictedDefaultDACL(token windows.Token, logonSID *windows.SID) error {
	info, err := tokenInformation(token, windows.TokenDefaultDacl)
	if err != nil {
		return fmt.Errorf("read restricted default DACL: %w", err)
	}
	type tokenDefaultDACL struct{ DefaultDACL *windows.ACL }
	defaultDACL := (*tokenDefaultDACL)(unsafe.Pointer(&info[0]))
	if defaultDACL.DefaultDACL == nil || defaultDACL.DefaultDACL.AceCount != 2 {
		return errors.New("restricted token default DACL is not the expected two-entry private DACL")
	}
	ownerRights, err := windows.StringToSid("S-1-3-4")
	if err != nil {
		return err
	}
	seenLogon, seenOwnerRights := false, false
	for index := uint32(0); index < uint32(defaultDACL.DefaultDACL.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(defaultDACL.DefaultDACL, index, &ace); err != nil {
			return err
		}
		if ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERITED_ACE != 0 {
			return errors.New("restricted token default DACL contains an unexpected ACE type or inheritance flag")
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		switch {
		case aceSID.IsValid() && aceSID.Equals(logonSID) && ace.Mask == windows.ACCESS_MASK(windows.GENERIC_ALL):
			seenLogon = true
		case aceSID.IsValid() && aceSID.Equals(ownerRights) && ace.Mask == windows.ACCESS_MASK(windows.READ_CONTROL):
			seenOwnerRights = true
		default:
			return errors.New("restricted token default DACL contains an unapproved trustee or access mask")
		}
	}
	if !seenLogon || !seenOwnerRights {
		return errors.New("restricted token default DACL is missing the command logon SID or OWNER RIGHTS entry")
	}
	return nil
}

func tokenLogonSID(groups *windows.Tokengroups) *windows.SID {
	if groups == nil {
		return nil
	}
	for _, group := range groups.AllGroups() {
		if group.Attributes&windows.SE_GROUP_LOGON_ID == windows.SE_GROUP_LOGON_ID {
			return group.Sid
		}
	}
	return nil
}

func setRestrictedTokenDefaultDACL(token windows.Token, logonSID *windows.SID) error {
	ownerRights, err := windows.StringToSid("S-1-3-4")
	if err != nil {
		return err
	}
	entries := []windows.EXPLICIT_ACCESS{
		{AccessPermissions: windows.ACCESS_MASK(windows.GENERIC_ALL), AccessMode: windows.GRANT_ACCESS, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN, TrusteeValue: windows.TrusteeValueFromSID(logonSID)}},
		{AccessPermissions: windows.ACCESS_MASK(windows.READ_CONTROL), AccessMode: windows.GRANT_ACCESS, Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_UNKNOWN, TrusteeValue: windows.TrusteeValueFromSID(ownerRights)}},
	}
	dacl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return fmt.Errorf("build command logon default DACL: %w", err)
	}
	info := struct{ DefaultDACL *windows.ACL }{DefaultDACL: dacl}
	if err := windows.SetTokenInformation(token, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return fmt.Errorf("set command logon default DACL: %w", err)
	}
	return nil
}

func enableChangeNotifyPrivilege(token windows.Token) error {
	name, err := windows.UTF16PtrFromString("SeChangeNotifyPrivilege")
	if err != nil {
		return err
	}
	var luid windows.LUID
	if err := windows.LookupPrivilegeValue(nil, name, &luid); err != nil {
		return fmt.Errorf("resolve directory traversal privilege: %w", err)
	}
	state := windows.Tokenprivileges{PrivilegeCount: 1}
	state.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
	if err := windows.AdjustTokenPrivileges(token, false, &state, uint32(unsafe.Sizeof(state)), nil, nil); err != nil {
		return fmt.Errorf("enable directory traversal privilege: %w", err)
	}
	return nil
}

func verifyNULAccess(token windows.Token) (resultErr error) {
	var impersonation windows.Token
	if err := windows.DuplicateTokenEx(token, windows.TOKEN_QUERY|windows.TOKEN_IMPERSONATE, nil, windows.SecurityImpersonation, windows.TokenImpersonation, &impersonation); err != nil {
		return fmt.Errorf("duplicate restricted token for Probe: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(windows.Handle(impersonation))) }()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := windows.SetThreadToken(nil, impersonation); err != nil {
		return fmt.Errorf("impersonate final restricted token: %w", err)
	}
	var probeErr error
	reader, err := os.Open("NUL")
	if err != nil {
		probeErr = fmt.Errorf("open NUL for reading: %w", err)
	} else {
		var value [1]byte
		read, readErr := reader.Read(value[:])
		if read != 0 {
			probeErr = errors.Join(probeErr, fmt.Errorf("read NUL returned %d bytes; expected end of stream", read))
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			probeErr = errors.Join(probeErr, fmt.Errorf("read NUL: %w", readErr))
		}
		probeErr = errors.Join(probeErr, reader.Close())
	}
	writer, err := os.OpenFile("NUL", os.O_WRONLY, 0)
	if err != nil {
		probeErr = errors.Join(probeErr, fmt.Errorf("open NUL for writing: %w", err))
	} else {
		if _, err := writer.Write([]byte{0}); err != nil {
			probeErr = errors.Join(probeErr, fmt.Errorf("write NUL: %w", err))
		}
		probeErr = errors.Join(probeErr, writer.Close())
	}
	revertErr := windows.RevertToSelf()
	return errors.Join(probeErr, revertErr)
}

func runRestrictedCommand(pipe *os.File, plan *nativeExecutionPlan, restricted windows.Token, logonSID *windows.SID) error {
	isRestricted, err := restricted.IsRestricted()
	if err != nil || !isRestricted {
		return errors.Join(errors.New("refusing to launch command without a restricted token"), err)
	}
	user, err := restricted.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read restricted token account identity: %w", err)
	}
	groups, err := restricted.GetTokenGroups()
	if err != nil {
		return fmt.Errorf("read restricted token logon identity: %w", err)
	}
	restrictedLogonSID := tokenLogonSID(groups)
	if restrictedLogonSID == nil || restrictedLogonSID.String() != logonSID.String() {
		return errors.New("runner logon SID changed during command setup")
	}
	everyoneSID, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		return err
	}
	if err := verifyNativeRestrictedToken(restricted, user.User.Sid, restrictedLogonSID, everyoneSID); err != nil {
		return fmt.Errorf("verify restricted token before process creation: %w", err)
	}
	if err := verifyNULAccess(restricted); err != nil {
		return fmt.Errorf("restricted token device Probe failed: %w", err)
	}
	return launchRestrictedCommand(pipe, plan, restricted, logonSID)
}

func probeRestrictedProcess(token windows.Token) (resultErr error) {
	systemRoot := os.Getenv("SystemRoot")
	if systemRoot == "" {
		return errors.New("SystemRoot is unavailable")
	}
	command := filepath.Join(systemRoot, "System32", "cmd.exe")
	command16, err := windows.UTF16PtrFromString(command)
	if err != nil {
		return err
	}
	line16, err := windows.UTF16PtrFromString(windows.ComposeCommandLine([]string{command, "/d", "/c", "exit", "0"}))
	if err != nil {
		return err
	}
	dir16, err := windows.UTF16PtrFromString(filepath.Dir(command))
	if err != nil {
		return err
	}
	environmentBlock, err := encodeEnvironment(map[string]string{"SYSTEMROOT": systemRoot, "WINDIR": systemRoot, "PATH": filepath.Dir(command), "PATHEXT": ".COM;.EXE;.BAT;.CMD"})
	if err != nil {
		return err
	}
	startup := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{})), Flags: windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE}
	var process windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcessAsUser(token, command16, line16, nil, nil, false, flags, &environmentBlock[0], dir16, &startup, &process); err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, windows.CloseHandle(process.Thread), windows.CloseHandle(process.Process))
	}()
	wait, err := windows.WaitForSingleObject(process.Process, 10_000)
	if err != nil {
		return fmt.Errorf("wait for restricted-token Probe process: %w", err)
	}
	if wait != windows.WAIT_OBJECT_0 {
		terminateErr := windows.TerminateProcess(process.Process, 1)
		wait, waitErr := windows.WaitForSingleObject(process.Process, 10_000)
		if waitErr == nil && wait != windows.WAIT_OBJECT_0 {
			waitErr = fmt.Errorf("restricted-token Probe process remained active after termination (wait=%d)", wait)
		}
		return errors.Join(errors.New("restricted-token child process did not exit before Probe timeout"), terminateErr, waitErr)
	}
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		return err
	}
	if exitCode != 0 {
		return fmt.Errorf("restricted-token child process exited with code %d", exitCode)
	}
	return nil
}

func launchRestrictedCommand(pipe *os.File, plan *nativeExecutionPlan, token windows.Token, logonSID *windows.SID) (resultErr error) {
	executable, err := canonicalExecutable(plan.CommandPath)
	if err != nil {
		return fmt.Errorf("resolve command executable: %w", err)
	}
	if _, _, err := nativeFileIdentity(executable); err != nil {
		return fmt.Errorf("validate command executable filesystem: %w", err)
	}
	workingDirectory, err := canonicalNativeDirectory(plan.WorkingDirectory)
	if err != nil {
		return fmt.Errorf("resolve command working directory: %w", err)
	}
	if !filepath.IsAbs(executable) || !filepath.IsAbs(workingDirectory) {
		return errors.New("command paths must be absolute")
	}
	env := make(map[string]string, len(plan.Environment))
	for _, item := range plan.Environment {
		key, value, ok := strings.Cut(item, "=")
		if !ok || key == "" || strings.ContainsRune(key, '\x00') || strings.ContainsRune(value, '\x00') {
			return errors.New("command environment contains an invalid entry")
		}
		env[key] = value
	}
	environmentBlock, err := encodeEnvironment(env)
	if err != nil {
		return err
	}
	job, err := newJobObject()
	if err != nil {
		return fmt.Errorf("create command Job Object: %w", err)
	}
	defer func() {
		if job != 0 {
			waitErr := terminateAndWaitJob(job, 1)
			closeErr := windows.CloseHandle(job)
			resultErr = errors.Join(resultErr, waitErr, closeErr)
		}
	}()

	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var childStdin, parentStdin, parentStdout, childStdout, parentStderr, childStderr windows.Handle
	if err := windows.CreatePipe(&childStdin, &parentStdin, sa, 0); err != nil {
		return fmt.Errorf("create restricted command stdin pipe: %w", err)
	}
	if err := windows.CreatePipe(&parentStdout, &childStdout, sa, 0); err != nil {
		return errors.Join(err, closeWindowsHandles(childStdin, parentStdin))
	}
	if err := windows.CreatePipe(&parentStderr, &childStderr, sa, 0); err != nil {
		return errors.Join(err, closeWindowsHandles(childStdin, parentStdin, parentStdout, childStdout))
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeWindowsHandles(childStdin, parentStdin, parentStdout, childStdout, parentStderr, childStderr))
	}()
	for _, handle := range []windows.Handle{parentStdin, parentStdout, parentStderr} {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, 0); err != nil {
			return fmt.Errorf("restrict child pipe inheritance: %w", err)
		}
	}
	desktopName := "OAgent-" + plan.CommandID
	desktop, err := createPrivateDesktop(desktopName, logonSID)
	if err != nil {
		return fmt.Errorf("create command private desktop: %w", err)
	}
	defer func() {
		if r1, _, callErr := procCloseDesktop.Call(uintptr(desktop)); r1 == 0 {
			resultErr = errors.Join(resultErr, fmt.Errorf("close command private desktop: %w", callErr))
		}
	}()
	args := plan.Arguments
	if len(args) == 0 {
		args = []string{executable}
	}
	commandLine, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return err
	}
	executable16, _ := windows.UTF16PtrFromString(executable)
	working16, _ := windows.UTF16PtrFromString(workingDirectory)
	desktopPath, err := windows.UTF16PtrFromString("WinSta0\\" + desktopName)
	if err != nil {
		return err
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fmt.Errorf("create restricted command handle list: %w", err)
	}
	defer attributes.Delete()
	inheritedHandles := []windows.Handle{childStdin, childStdout, childStderr}
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&inheritedHandles[0]), uintptr(len(inheritedHandles))*unsafe.Sizeof(inheritedHandles[0])); err != nil {
		return fmt.Errorf("limit restricted command inherited handles: %w", err)
	}
	startup := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Desktop: desktopPath, Flags: windows.STARTF_USESTDHANDLES | windows.STARTF_USESHOWWINDOW, ShowWindow: windows.SW_HIDE, StdInput: childStdin, StdOutput: childStdout, StdErr: childStderr}, ProcThreadAttributeList: attributes.List()}
	var process windows.ProcessInformation
	createFlags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW)
	createErr := windows.CreateProcessAsUser(token, executable16, commandLine, nil, nil, true, createFlags, &environmentBlock[0], working16, &startup.StartupInfo, &process)
	runtime.KeepAlive(environmentBlock)
	runtime.KeepAlive(inheritedHandles)
	if createErr != nil {
		return fmt.Errorf("start command with restricted token: %w", createErr)
	}
	defer func() {
		if process.Thread != 0 {
			resultErr = errors.Join(resultErr, windows.CloseHandle(process.Thread))
		}
		if process.Process != 0 {
			resultErr = errors.Join(resultErr, windows.CloseHandle(process.Process))
		}
	}()
	if err := windows.AssignProcessToJobObject(job, process.Process); err != nil {
		killErr := windows.TerminateProcess(process.Process, 1)
		_, waitErr := windows.WaitForSingleObject(process.Process, windows.INFINITE)
		return errors.Join(fmt.Errorf("assign command to Job Object: %w", err), killErr, waitErr)
	}
	var closeChildErr error
	for _, handle := range []*windows.Handle{&childStdin, &childStdout, &childStderr} {
		if *handle != 0 {
			closeChildErr = errors.Join(closeChildErr, windows.CloseHandle(*handle))
			*handle = 0
		}
	}
	if closeChildErr != nil {
		return closeChildErr
	}
	if _, err := windows.ResumeThread(process.Thread); err != nil {
		return fmt.Errorf("resume command after Job assignment: %w", err)
	}
	stdinFile := os.NewFile(uintptr(parentStdin), "native-sandbox-stdin")
	stdoutFile := os.NewFile(uintptr(parentStdout), "native-sandbox-stdout")
	stderrFile := os.NewFile(uintptr(parentStderr), "native-sandbox-stderr")
	parentStdin, parentStdout, parentStderr = 0, 0, 0
	defer func() { resultErr = errors.Join(resultErr, closeWindowsFiles(stdinFile, stdoutFile, stderrFile)) }()

	var frameWriteMu sync.Mutex
	streamError := make(chan error, 2)
	pump := func(stream string, source *os.File) {
		reader := bufio.NewReaderSize(source, nativeChunkSize)
		buffer := make([]byte, nativeChunkSize)
		for {
			n, readErr := reader.Read(buffer)
			if n > 0 {
				frameWriteMu.Lock()
				writeErr := writeNativeFrame(pipe, nativeFrame{Version: nativeProtocolVersion, Type: "output", CommandID: plan.CommandID, Stream: stream, Data: append([]byte(nil), buffer[:n]...)})
				frameWriteMu.Unlock()
				if writeErr != nil {
					streamError <- writeErr
					return
				}
			}
			if readErr != nil {
				if !errors.Is(readErr, io.EOF) {
					streamError <- readErr
				} else {
					streamError <- nil
				}
				return
			}
		}
	}
	go pump("stdout", stdoutFile)
	go pump("stderr", stderrFile)
	stdinDone := make(chan error, 1)
	go func() {
		if len(plan.Input) > 0 {
			if _, err := io.Copy(stdinFile, bytes.NewReader(plan.Input)); err != nil {
				stdinDone <- err
				return
			}
		}
		stdinDone <- stdinFile.Close()
	}()
	disconnected := make(chan error, 1)
	go func() {
		frame, err := readNativeFrame(pipe)
		if err == nil {
			err = fmt.Errorf("unexpected runner control frame %q", frame.Type)
		}
		disconnected <- err
	}()
	var timedOut bool
	deadline := time.Now().Add(time.Duration(plan.TimeoutMS) * time.Millisecond)
	for {
		wait, err := windows.WaitForSingleObject(process.Process, 100)
		if err != nil {
			return errors.Join(fmt.Errorf("wait for command process: %w", err), terminateAndWaitJob(job, 1))
		}
		if wait == windows.WAIT_OBJECT_0 {
			break
		}
		if wait != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("unexpected command process wait result %d", wait)
		}
		if !time.Now().Before(deadline) {
			if err := terminateAndWaitJob(job, 124); err != nil {
				return fmt.Errorf("terminate timed out command process tree: %w", err)
			}
			wait, err := windows.WaitForSingleObject(process.Process, 30_000)
			if err != nil {
				return fmt.Errorf("wait for timed out command process: %w", err)
			}
			if wait != windows.WAIT_OBJECT_0 {
				return fmt.Errorf("timed out command process remained active after Job termination (wait=%d)", wait)
			}
			timedOut = true
			break
		}
		select {
		case err := <-disconnected:
			stopErr := terminateAndWaitJob(job, 1)
			wait, waitErr := windows.WaitForSingleObject(process.Process, 30_000)
			if waitErr == nil && wait != windows.WAIT_OBJECT_0 {
				waitErr = fmt.Errorf("command process remained active after Host disconnect (wait=%d)", wait)
			}
			return errors.Join(fmt.Errorf("host control pipe disconnected during execution: %w", err), stopErr, waitErr)
		default:
		}
	}
	// Closing the kill-on-close Job also terminates descendants that outlive the
	// shell, so their inherited output handles cannot keep the pipes open.
	if err := terminateAndWaitJob(job, 1); err != nil {
		return fmt.Errorf("stop remaining command processes: %w", err)
	}
	if err := windows.CloseHandle(job); err != nil {
		return fmt.Errorf("close command Job Object: %w", err)
	}
	job = 0
	stdinErr := <-stdinDone
	stdoutErr, stderrErr := <-streamError, <-streamError
	var exitCode uint32
	if err := windows.GetExitCodeProcess(process.Process, &exitCode); err != nil {
		return err
	}
	frameWriteMu.Lock()
	frameExitCode := exitCode
	if timedOut {
		frameExitCode = 124
	}
	exitErr := writeNativeFrame(pipe, nativeFrame{Version: nativeProtocolVersion, Type: "exit", CommandID: plan.CommandID, ExitCode: frameExitCode, TimedOut: timedOut})
	frameWriteMu.Unlock()
	if exitErr != nil {
		return fmt.Errorf("send command exit status: %w", exitErr)
	}
	return errors.Join(stdinErr, stdoutErr, stderrErr)
}

func createPrivateDesktop(name string, logonSID *windows.SID) (windows.Handle, error) {
	sddl := fmt.Sprintf("D:P(A;;0x001f01ff;;;%s)(A;;RC;;;S-1-3-4)(A;;FA;;;SY)(A;;FA;;;BA)", logonSID.String())
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return 0, err
	}
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, _, callErr := procCreateDesktop.Call(uintptr(unsafe.Pointer(name16)), 0, 0, 0, 0x000F01FF, uintptr(unsafe.Pointer(&security)))
	runtime.KeepAlive(descriptor)
	if h == 0 {
		return 0, callErr
	}
	return windows.Handle(h), nil
}

var _ = syscall.Errno(0)
