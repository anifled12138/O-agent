//go:build windows

package sandbox

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	netapiDLL                   = windows.NewLazySystemDLL("netapi32.dll")
	netUserAddProc              = netapiDLL.NewProc("NetUserAdd")
	netUserDelProc              = netapiDLL.NewProc("NetUserDel")
	netUserSetInfoProc          = netapiDLL.NewProc("NetUserSetInfo")
	netUserGetLocalGroupsProc   = netapiDLL.NewProc("NetUserGetLocalGroups")
	netLocalGroupAddMembersProc = netapiDLL.NewProc("NetLocalGroupAddMembers")
	lookupAccountSidProc        = windows.NewLazySystemDLL("advapi32.dll").NewProc("LookupAccountSidW")
	lsaOpenPolicyProc           = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaOpenPolicy")
	lsaAddAccountRightsProc     = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaAddAccountRights")
	lsaRemoveAccountRightsProc  = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaRemoveAccountRights")
	lsaEnumerateRightsProc      = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaEnumerateAccountRights")
	lsaFreeMemoryProc           = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaFreeMemory")
	lsaCloseProc                = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaClose")
	lsaNtStatusToWinErrorProc   = windows.NewLazySystemDLL("advapi32.dll").NewProc("LsaNtStatusToWinError")
)

const (
	userPrivUser        = 1
	ufScript            = 0x0001
	ufDontExpirePasswd  = 0x10000
	nerrUserNotFound    = 2221
	nerrMemberInAlias   = 1378
	policyCreateAccount = 0x10
	policyLookupNames   = 0x800
	ufAccountDisable    = 0x0002
	lgIncludeIndirect   = 1
)

type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Privilege   uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

type localGroupMembersInfo0 struct{ SID *windows.SID }

type localGroupInfo0 struct{ Name *uint16 }

type userInfo1008 struct{ Flags uint32 }

type userInfo23 struct {
	Name     *uint16
	FullName *uint16
	Comment  *uint16
	Flags    uint32
	SID      *windows.SID
}

type userInfo1Native struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Privilege   uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

type lsaObjectAttributes struct {
	Length                   uint32
	RootDirectory            uintptr
	ObjectName               uintptr
	Attributes               uint32
	SecurityDescriptor       uintptr
	SecurityQualityOfService uintptr
}

type lsaUnicodeString struct {
	Length        uint16
	MaximumLength uint16
	Buffer        *uint16
}

func createSandboxAccount(name string) (sidText, password string, err error) {
	password, err = randomPassword()
	if err != nil {
		return "", "", err
	}
	sidText, err = createSandboxAccountWithPassword(name, password)
	if err != nil {
		zeroString(&password)
		return "", "", err
	}
	return sidText, password, nil
}

func createSandboxAccountWithPassword(name, password string) (sidText string, err error) {
	exists, err := accountNameExists(name)
	if err != nil {
		return "", fmt.Errorf("check sandbox account name %q: %w", name, err)
	}
	if exists {
		return "", fmt.Errorf("sandbox account %q already exists without a verified installation credential", name)
	}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return "", err
	}
	password16, err := windows.UTF16PtrFromString(password)
	if err != nil {
		return "", err
	}
	info := userInfo1{Name: name16, Password: password16, Privilege: userPrivUser, Flags: ufScript | ufDontExpirePasswd}
	var parameterError uint32
	r1, _, _ := netUserAddProc.Call(0, 1, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&parameterError)))
	clearUTF16(password16)
	if r1 != 0 {
		return "", fmt.Errorf("create standard local sandbox account %q (parameter %d): %w", name, parameterError, syscall.Errno(r1))
	}
	created := true
	defer func() {
		if err != nil && created {
			if cleanupErr := deleteSandboxAccount(name); cleanupErr != nil {
				err = fmt.Errorf("%w; rollback account %q: %v", err, name, cleanupErr)
			}
		}
	}()

	sid, err := lookupLocalAccountSID(name)
	if err != nil {
		return "", fmt.Errorf("read SID for new sandbox account %q: %w", name, err)
	}
	usersGroup, err := builtinGroupName("S-1-5-32-545")
	if err != nil {
		return "", fmt.Errorf("resolve local Users group: %w", err)
	}
	group16, err := windows.UTF16PtrFromString(usersGroup)
	if err != nil {
		return "", err
	}
	member := localGroupMembersInfo0{SID: sid}
	r1, _, _ = netLocalGroupAddMembersProc.Call(0, uintptr(unsafe.Pointer(group16)), 0, uintptr(unsafe.Pointer(&member)), 1)
	if r1 != 0 && uint32(r1) != nerrMemberInAlias {
		return "", fmt.Errorf("add sandbox account to standard Users group: %w", syscall.Errno(r1))
	}
	if err := addLogOnLocallyRight(sid); err != nil {
		return "", fmt.Errorf("grant required local logon right to %q: %w", name, err)
	}
	created = false
	return sid.String(), nil
}

func accountNameExists(name string) (exists bool, resultErr error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	var buf *byte
	err = windows.NetUserGetInfo(nil, name16, 23, &buf)
	if buf != nil {
		resultErr = errors.Join(resultErr, windows.NetApiBufferFree(buf))
	}
	if errors.Is(err, syscall.Errno(nerrUserNotFound)) {
		return false, resultErr
	}
	if err != nil {
		return false, errors.Join(err, resultErr)
	}
	if buf == nil {
		return false, errors.Join(resultErr, errors.New("NetUserGetInfo returned an empty account record"))
	}
	return true, resultErr
}

func lookupLocalAccountSID(name string) (resultSID *windows.SID, resultErr error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var buf *byte
	getErr := windows.NetUserGetInfo(nil, name16, 23, &buf)
	if buf != nil {
		defer func() { resultErr = errors.Join(resultErr, windows.NetApiBufferFree(buf)) }()
	}
	if getErr != nil {
		return nil, getErr
	}
	if buf == nil {
		return nil, fmt.Errorf("NetUserGetInfo returned an empty account record")
	}
	info := (*userInfo23)(unsafe.Pointer(buf))
	if info.SID == nil || !info.SID.IsValid() {
		return nil, fmt.Errorf("Windows returned an invalid SID for account %q", name)
	}
	copySID, err := windows.StringToSid(info.SID.String())
	if err != nil {
		return nil, err
	}
	return copySID, nil
}

func builtinGroupName(sidText string) (string, error) {
	sid, err := windows.StringToSid(sidText)
	if err != nil {
		return "", err
	}
	var nameLen, domainLen uint32
	var use uint32
	_, _, callErr := lookupAccountSidProc.Call(0, uintptr(unsafe.Pointer(sid)), 0, uintptr(unsafe.Pointer(&nameLen)), 0, uintptr(unsafe.Pointer(&domainLen)), uintptr(unsafe.Pointer(&use)))
	if nameLen == 0 {
		return "", fmt.Errorf("resolve builtin group SID %s: %w", sidText, callErr)
	}
	name := make([]uint16, nameLen)
	domain := make([]uint16, domainLen)
	r1, _, callErr := lookupAccountSidProc.Call(0, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&nameLen)), uintptr(unsafe.Pointer(&domain[0])), uintptr(unsafe.Pointer(&domainLen)), uintptr(unsafe.Pointer(&use)))
	if r1 == 0 {
		return "", fmt.Errorf("resolve builtin group SID %s: %w", sidText, callErr)
	}
	return windows.UTF16ToString(name), nil
}

func addLogOnLocallyRight(sid *windows.SID) (resultErr error) {
	attrs := lsaObjectAttributes{Length: uint32(unsafe.Sizeof(lsaObjectAttributes{}))}
	var policy uintptr
	status, _, _ := lsaOpenPolicyProc.Call(0, uintptr(unsafe.Pointer(&attrs)), policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if status != 0 {
		return lsaStatusError(status)
	}
	defer func() { resultErr = errors.Join(resultErr, closeLSAPolicy(policy)) }()
	rightName, err := windows.UTF16PtrFromString("SeInteractiveLogonRight")
	if err != nil {
		return err
	}
	right := lsaUnicodeString{Length: uint16(len("SeInteractiveLogonRight") * 2), MaximumLength: uint16((len("SeInteractiveLogonRight") + 1) * 2), Buffer: rightName}
	status, _, _ = lsaAddAccountRightsProc.Call(policy, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&right)), 1)
	if status != 0 {
		return lsaStatusError(status)
	}
	return nil
}

func hasLogOnLocallyRight(sid *windows.SID) (allowed bool, resultErr error) {
	attrs := lsaObjectAttributes{Length: uint32(unsafe.Sizeof(lsaObjectAttributes{}))}
	var policy uintptr
	status, _, _ := lsaOpenPolicyProc.Call(0, uintptr(unsafe.Pointer(&attrs)), policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if status != 0 {
		return false, lsaStatusError(status)
	}
	defer func() { resultErr = errors.Join(resultErr, closeLSAPolicy(policy)) }()
	var rights *lsaUnicodeString
	var count uint32
	status, _, _ = lsaEnumerateRightsProc.Call(policy, uintptr(unsafe.Pointer(sid)), uintptr(unsafe.Pointer(&rights)), uintptr(unsafe.Pointer(&count)))
	if rights != nil {
		defer func() { resultErr = errors.Join(resultErr, freeLSAMemory(unsafe.Pointer(rights))) }()
	}
	if status == uintptr(0xC0000034) { // STATUS_OBJECT_NAME_NOT_FOUND
		return false, nil
	}
	if status != 0 {
		return false, lsaStatusError(status)
	}
	for _, right := range unsafe.Slice(rights, count) {
		if windows.UTF16PtrToString(right.Buffer) == "SeInteractiveLogonRight" {
			return true, nil
		}
	}
	return false, nil
}

func removeLogOnLocallyRight(sid *windows.SID) (resultErr error) {
	attrs := lsaObjectAttributes{Length: uint32(unsafe.Sizeof(lsaObjectAttributes{}))}
	var policy uintptr
	status, _, _ := lsaOpenPolicyProc.Call(0, uintptr(unsafe.Pointer(&attrs)), policyCreateAccount|policyLookupNames, uintptr(unsafe.Pointer(&policy)))
	if status != 0 {
		return lsaStatusError(status)
	}
	defer func() { resultErr = errors.Join(resultErr, closeLSAPolicy(policy)) }()
	rightName, err := windows.UTF16PtrFromString("SeInteractiveLogonRight")
	if err != nil {
		return err
	}
	right := lsaUnicodeString{Length: uint16(len("SeInteractiveLogonRight") * 2), MaximumLength: uint16((len("SeInteractiveLogonRight") + 1) * 2), Buffer: rightName}
	allRights := uintptr(0)
	status, _, _ = lsaRemoveAccountRightsProc.Call(policy, uintptr(unsafe.Pointer(sid)), allRights, uintptr(unsafe.Pointer(&right)), 1)
	if status != 0 && status != uintptr(0xC0000034) {
		return lsaStatusError(status)
	}
	present, err := hasLogOnLocallyRight(sid)
	if err != nil {
		return fmt.Errorf("verify removal of SeInteractiveLogonRight: %w", err)
	}
	if present {
		return fmt.Errorf("SeInteractiveLogonRight remains assigned to %s", sid.String())
	}
	return nil
}

func closeLSAPolicy(policy uintptr) error {
	status, _, _ := lsaCloseProc.Call(policy)
	return lsaCallError("close LSA policy handle", status)
}

func freeLSAMemory(memory unsafe.Pointer) error {
	if memory == nil {
		return nil
	}
	status, _, _ := lsaFreeMemoryProc.Call(uintptr(memory))
	return lsaCallError("free LSA policy memory", status)
}

func lsaCallError(operation string, status uintptr) error {
	if status == 0 {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, lsaStatusError(status))
}

func disableSandboxAccount(name string) error {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var buf *byte
	if err := windows.NetUserGetInfo(nil, name16, 23, &buf); err != nil {
		if buf != nil {
			err = errors.Join(err, windows.NetApiBufferFree(buf))
		}
		return err
	}
	if buf == nil {
		return fmt.Errorf("NetUserGetInfo returned an empty account record for %q", name)
	}
	flags := (*userInfo23)(unsafe.Pointer(buf)).Flags | ufAccountDisable
	if err := windows.NetApiBufferFree(buf); err != nil {
		return fmt.Errorf("release account info buffer: %w", err)
	}
	info := userInfo1008{Flags: flags}
	var parameterError uint32
	r1, _, _ := netUserSetInfoProc.Call(0, uintptr(unsafe.Pointer(name16)), 1008, uintptr(unsafe.Pointer(&info)), uintptr(unsafe.Pointer(&parameterError)))
	if r1 != 0 {
		return fmt.Errorf("disable sandbox account %q: %w", name, syscall.Errno(r1))
	}
	buf = nil
	if err := windows.NetUserGetInfo(nil, name16, 23, &buf); err != nil {
		if buf != nil {
			err = errors.Join(err, windows.NetApiBufferFree(buf))
		}
		return fmt.Errorf("verify disabled sandbox account %q: %w", name, err)
	}
	if buf == nil {
		return fmt.Errorf("verify disabled sandbox account %q returned no record", name)
	}
	verified := (*userInfo23)(unsafe.Pointer(buf)).Flags&ufAccountDisable != 0
	if err := windows.NetApiBufferFree(buf); err != nil {
		return fmt.Errorf("release account verification buffer: %w", err)
	}
	if !verified {
		return fmt.Errorf("sandbox account %q remained enabled after disable", name)
	}
	return nil
}

func validateSandboxAccount(name, expectedSID string) error {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var accountBuf *byte
	if err := windows.NetUserGetInfo(nil, name16, 23, &accountBuf); err != nil {
		if accountBuf != nil {
			err = errors.Join(err, windows.NetApiBufferFree(accountBuf))
		}
		return fmt.Errorf("read sandbox account %q: %w", name, err)
	}
	if accountBuf == nil {
		return fmt.Errorf("sandbox account %q has no account record", name)
	}
	account := *(*userInfo23)(unsafe.Pointer(accountBuf))
	accountSID := ""
	if account.SID != nil {
		accountSID = account.SID.String()
	}
	if err := windows.NetApiBufferFree(accountBuf); err != nil {
		return fmt.Errorf("release sandbox account information: %w", err)
	}
	if accountSID == "" || accountSID != expectedSID {
		return fmt.Errorf("sandbox account %q SID does not match its installation manifest", name)
	}
	if account.Flags&ufAccountDisable != 0 {
		return fmt.Errorf("sandbox account %q is disabled", name)
	}
	var privilegeBuf *byte
	if err := windows.NetUserGetInfo(nil, name16, 1, &privilegeBuf); err != nil {
		if privilegeBuf != nil {
			err = errors.Join(err, windows.NetApiBufferFree(privilegeBuf))
		}
		return fmt.Errorf("read privilege level for sandbox account %q: %w", name, err)
	}
	if privilegeBuf == nil {
		return fmt.Errorf("sandbox account %q has no privilege record", name)
	}
	privilege := (*userInfo1Native)(unsafe.Pointer(privilegeBuf)).Privilege
	if err := windows.NetApiBufferFree(privilegeBuf); err != nil {
		return fmt.Errorf("release sandbox privilege information: %w", err)
	}
	if privilege != userPrivUser {
		return fmt.Errorf("sandbox account %q does not have the standard user privilege level", name)
	}

	groupNames, err := sandboxAccountLocalGroups(name)
	if err != nil {
		return err
	}
	usersName, err := builtinGroupName("S-1-5-32-545")
	if err != nil {
		return err
	}
	adminsName, err := builtinGroupName("S-1-5-32-544")
	if err != nil {
		return err
	}
	if !groupNames[strings.ToLower(usersName)] {
		return fmt.Errorf("sandbox account %q is missing from the standard Users group", name)
	}
	if groupNames[strings.ToLower(adminsName)] {
		return fmt.Errorf("sandbox account %q must not belong to the local Administrators group", name)
	}
	if len(groupNames) != 1 {
		return fmt.Errorf("sandbox account %q has additional local group memberships beyond the standard Users group", name)
	}
	sid, err := windows.StringToSid(accountSID)
	if err != nil {
		return fmt.Errorf("parse sandbox account SID: %w", err)
	}
	allowed, err := hasLogOnLocallyRight(sid)
	if err != nil {
		return fmt.Errorf("verify required local logon right for %q: %w", name, err)
	}
	if !allowed {
		return fmt.Errorf("sandbox account %q is missing SeInteractiveLogonRight", name)
	}
	return nil
}

func sandboxAccountLocalGroups(name string) (map[string]bool, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	var groupsBuf *byte
	var entriesRead, totalEntries uint32
	r1, _, _ := netUserGetLocalGroupsProc.Call(0, uintptr(unsafe.Pointer(name16)), 0, lgIncludeIndirect,
		uintptr(unsafe.Pointer(&groupsBuf)), uintptr(^uint32(0)), uintptr(unsafe.Pointer(&entriesRead)), uintptr(unsafe.Pointer(&totalEntries)))
	if r1 != 0 {
		resultErr := fmt.Errorf("read local group membership for sandbox account %q: %w", name, syscall.Errno(r1))
		if groupsBuf != nil {
			resultErr = errors.Join(resultErr, windows.NetApiBufferFree(groupsBuf))
		}
		return nil, resultErr
	}
	if totalEntries != entriesRead {
		if groupsBuf != nil {
			return nil, errors.Join(fmt.Errorf("local group membership for sandbox account %q was truncated", name), windows.NetApiBufferFree(groupsBuf))
		}
		return nil, fmt.Errorf("local group membership for sandbox account %q was truncated", name)
	}
	groupNames := make(map[string]bool, entriesRead)
	for _, group := range unsafe.Slice((*localGroupInfo0)(unsafe.Pointer(groupsBuf)), entriesRead) {
		groupNames[strings.ToLower(windows.UTF16PtrToString(group.Name))] = true
	}
	if groupsBuf != nil {
		if err := windows.NetApiBufferFree(groupsBuf); err != nil {
			return nil, fmt.Errorf("release sandbox group information: %w", err)
		}
	}
	return groupNames, nil
}

func ensureSandboxAccountBaseline(name, expectedSID string) error {
	sid, err := lookupLocalAccountSID(name)
	if err != nil {
		return fmt.Errorf("read sandbox account %q SID: %w", name, err)
	}
	if sid.String() != expectedSID {
		return fmt.Errorf("sandbox account %q SID does not match its setup journal", name)
	}
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	var accountBuf *byte
	if err := windows.NetUserGetInfo(nil, name16, 23, &accountBuf); err != nil {
		if accountBuf != nil {
			err = errors.Join(err, windows.NetApiBufferFree(accountBuf))
		}
		return err
	}
	if accountBuf == nil {
		return fmt.Errorf("sandbox account %q has no account record", name)
	}
	flags := (*userInfo23)(unsafe.Pointer(accountBuf)).Flags
	if err := windows.NetApiBufferFree(accountBuf); err != nil {
		return fmt.Errorf("release sandbox account information: %w", err)
	}
	if flags&ufAccountDisable != 0 {
		return fmt.Errorf("sandbox account %q is disabled", name)
	}
	var privilegeBuf *byte
	if err := windows.NetUserGetInfo(nil, name16, 1, &privilegeBuf); err != nil {
		if privilegeBuf != nil {
			err = errors.Join(err, windows.NetApiBufferFree(privilegeBuf))
		}
		return err
	}
	if privilegeBuf == nil {
		return fmt.Errorf("sandbox account %q has no privilege record", name)
	}
	privilege := (*userInfo1Native)(unsafe.Pointer(privilegeBuf)).Privilege
	if err := windows.NetApiBufferFree(privilegeBuf); err != nil {
		return err
	}
	if privilege != userPrivUser {
		return fmt.Errorf("sandbox account %q is not a standard local user", name)
	}
	groupNames, err := sandboxAccountLocalGroups(name)
	if err != nil {
		return err
	}
	usersName, err := builtinGroupName("S-1-5-32-545")
	if err != nil {
		return err
	}
	adminsName, err := builtinGroupName("S-1-5-32-544")
	if err != nil {
		return err
	}
	if groupNames[strings.ToLower(adminsName)] {
		return fmt.Errorf("sandbox account %q must not belong to the local Administrators group", name)
	}
	if !groupNames[strings.ToLower(usersName)] {
		group16, err := windows.UTF16PtrFromString(usersName)
		if err != nil {
			return err
		}
		member := localGroupMembersInfo0{SID: sid}
		r1, _, _ := netLocalGroupAddMembersProc.Call(0, uintptr(unsafe.Pointer(group16)), 0, uintptr(unsafe.Pointer(&member)), 1)
		if r1 != 0 && uint32(r1) != nerrMemberInAlias {
			return fmt.Errorf("add sandbox account to standard Users group: %w", syscall.Errno(r1))
		}
	}
	allowed, err := hasLogOnLocallyRight(sid)
	if err != nil {
		return err
	}
	if !allowed {
		if err := addLogOnLocallyRight(sid); err != nil {
			return err
		}
	}
	return validateSandboxAccount(name, expectedSID)
}

func lsaStatusError(status uintptr) error {
	code, _, _ := lsaNtStatusToWinErrorProc.Call(status)
	if code == 0 {
		return fmt.Errorf("LSA operation failed with NTSTATUS 0x%08x", uint32(status))
	}
	return syscall.Errno(code)
}

func deleteSandboxAccount(name string) error {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	sid, sidErr := lookupLocalAccountSID(name)
	if sidErr == nil {
		if err := removeLogOnLocallyRight(sid); err != nil {
			return fmt.Errorf("remove local logon right from %q: %w", name, err)
		}
	} else if !errors.Is(sidErr, syscall.Errno(nerrUserNotFound)) {
		return fmt.Errorf("inspect sandbox account %q before deletion: %w", name, sidErr)
	} else {
		return nil
	}
	r1, _, _ := netUserDelProc.Call(0, uintptr(unsafe.Pointer(name16)))
	if r1 != 0 && uint32(r1) != nerrUserNotFound {
		return syscall.Errno(r1)
	}
	if _, err := lookupLocalAccountSID(name); !errors.Is(err, syscall.Errno(nerrUserNotFound)) {
		return errors.Join(err, fmt.Errorf("sandbox account %q remains after deletion", name))
	}
	return nil
}

func randomPassword() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate random sandbox password: %w", err)
	}
	// Include one character from each class required by standard Windows
	// password-complexity policies; the random suffix provides 256-bit entropy.
	return "Aa1!" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func sandboxAccountNames(ownerSID string) (offline, online string) {
	digest := sha256.Sum256([]byte(ownerSID))
	suffix := hex.EncodeToString(digest[:4])
	return "AxOffline_" + suffix, "AxOnline_" + suffix
}

func clearUTF16(value *uint16) {
	if value == nil {
		return
	}
	for i := 0; ; i++ {
		unit := (*[1 << 20]uint16)(unsafe.Pointer(value))[i]
		(*[1 << 20]uint16)(unsafe.Pointer(value))[i] = 0
		if unit == 0 {
			return
		}
	}
}
