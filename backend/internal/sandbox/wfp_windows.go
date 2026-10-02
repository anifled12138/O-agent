//go:build windows

package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var fwpmDLL = windows.NewLazySystemDLL("fwpuclnt.dll")

const (
	fwpUint8                  = 1
	fwpSecurityDescriptorType = 14
	fwpMatchEqual             = 0
	fwpmProviderPersistent    = 1
	fwpmSublayerPersistent    = 1
	fwpmFilterPersistent      = 1
	fwpmClearActionRight      = 8
	fwpActionBlock            = 0x1001
	fwpmAlreadyExists         = uint32(0x80320009)
)

type fwpByteBlob struct {
	Size uint32
	Data *byte
}

type fwpValue0 struct {
	Type  uint32
	_     uint32
	Value uintptr
}

type fwpConditionValue0 struct {
	Type  uint32
	_     uint32
	Value uintptr
}

// fwpConditionValuePointer is the pointer-valued view used when WFP returns a
// security descriptor condition. The ABI union occupies the same two-word
// layout as fwpConditionValue0; keeping this view pointer-typed avoids an
// unsafe uintptr-to-pointer conversion after the native read-back call.
type fwpConditionValuePointer struct {
	Type  uint32
	_     uint32
	Value unsafe.Pointer
}

type fwpmDisplayData0 struct {
	Name        *uint16
	Description *uint16
}

type fwpmProvider0 struct {
	Key         windows.GUID
	DisplayData fwpmDisplayData0
	Flags       uint32
	Data        fwpByteBlob
	ServiceName *uint16
}

type fwpmSublayer0 struct {
	Key         windows.GUID
	DisplayData fwpmDisplayData0
	Flags       uint32
	ProviderKey *windows.GUID
	Data        fwpByteBlob
	Weight      uint16
}

type fwpmFilterCondition0 struct {
	FieldKey       windows.GUID
	MatchType      uint32
	ConditionValue fwpConditionValue0
}

type fwpmAction0 struct {
	Type uint32
	_    uint32
	Key  windows.GUID
}

type fwpmFilter0 struct {
	Key                 windows.GUID
	DisplayData         fwpmDisplayData0
	Flags               uint32
	ProviderKey         *windows.GUID
	ProviderData        fwpByteBlob
	LayerKey            windows.GUID
	SublayerKey         windows.GUID
	Weight              fwpValue0
	NumFilterConditions uint32
	FilterCondition     *fwpmFilterCondition0
	Action              fwpmAction0
	RawContext          uint64
	ProviderContext     [2]uint64
	Reserved            uint16
	_                   uint16
	_2                  uint32
	FilterID            uint64
	EffectiveWeight     fwpValue0
}

type wfpKeys struct {
	Provider windows.GUID
	Sublayer windows.GUID
	Filters  []windows.GUID
}

func ownedWFPKeys(ownerSID string) (wfpKeys, error) {
	provider, err := stableGUID("provider:" + ownerSID)
	if err != nil {
		return wfpKeys{}, err
	}
	sublayer, err := stableGUID("sublayer:" + ownerSID)
	if err != nil {
		return wfpKeys{}, err
	}
	type rule struct{ name, layer, accountSID string }
	offlineName, onlineName := sandboxAccountNames(ownerSID)
	offlineSID, err := lookupLocalAccountSID(offlineName)
	if err != nil {
		return wfpKeys{}, fmt.Errorf("read Offline account SID for network policy: %w", err)
	}
	onlineSID, err := lookupLocalAccountSID(onlineName)
	if err != nil {
		return wfpKeys{}, fmt.Errorf("read Online account SID for network policy: %w", err)
	}
	const (
		connectV4 = "c38d57d1-05a7-4c33-904f-7fbceee60e82"
		connectV6 = "4a72393b-319f-44bc-84c3-ba54dcb3b6b4"
		listenV4  = "88bb5dad-76d7-4227-9c71-df0a3ed7be7e"
		listenV6  = "7ac9de24-17dd-4814-b4bd-a9fbc95a321b"
		recvV4    = "e1cd9fe7-f4b5-4273-96c0-592e487b8650"
		recvV6    = "a3b42c97-9f04-4672-b87e-cee9c483257f"
	)
	rules := []rule{
		{name: "offline-connect-v4", layer: connectV4, accountSID: offlineSID.String()},
		{name: "offline-connect-v6", layer: connectV6, accountSID: offlineSID.String()},
		{name: "offline-listen-v4", layer: listenV4, accountSID: offlineSID.String()},
		{name: "offline-listen-v6", layer: listenV6, accountSID: offlineSID.String()},
		{name: "offline-receive-v4", layer: recvV4, accountSID: offlineSID.String()},
		{name: "offline-receive-v6", layer: recvV6, accountSID: offlineSID.String()},
		{name: "online-listen-v4", layer: listenV4, accountSID: onlineSID.String()},
		{name: "online-listen-v6", layer: listenV6, accountSID: onlineSID.String()},
		{name: "online-receive-v4", layer: recvV4, accountSID: onlineSID.String()},
		{name: "online-receive-v6", layer: recvV6, accountSID: onlineSID.String()},
	}
	keys := wfpKeys{Provider: provider, Sublayer: sublayer}
	for _, rule := range rules {
		key, err := stableGUID("filter:" + ownerSID + ":" + rule.name)
		if err != nil {
			return wfpKeys{}, err
		}
		keys.Filters = append(keys.Filters, key)
	}
	return keys, nil
}

func stableGUID(value string) (windows.GUID, error) {
	sum := sha256.Sum256([]byte("O Agent Windows Sandbox v2\x00" + value))
	text := fmt.Sprintf("{%08x-%04x-%04x-%04x-%012x}",
		uint32(sum[0])<<24|uint32(sum[1])<<16|uint32(sum[2])<<8|uint32(sum[3]),
		uint16(sum[4])<<8|uint16(sum[5]),
		(uint16(sum[6])<<8|uint16(sum[7]))&0x0fff|0x5000,
		uint16(sum[8])<<8|uint16(sum[9]),
		uint64(sum[10])<<40|uint64(sum[11])<<32|uint64(sum[12])<<24|uint64(sum[13])<<16|uint64(sum[14])<<8|uint64(sum[15]))
	return windows.GUIDFromString(text)
}

func installOfflineWFP(ownerSID string) (result wfpKeys, resultErr error) {
	keys, err := ownedWFPKeys(ownerSID)
	if err != nil {
		return wfpKeys{}, err
	}
	engine, err := openWFP()
	if err != nil {
		return wfpKeys{}, err
	}
	defer func() { resultErr = errors.Join(resultErr, closeWFP(engine)) }()
	if err := wfpTransaction(engine, func() error {
		if err := ensureWFPProvider(engine, keys.Provider); err != nil {
			return err
		}
		if err := ensureWPFSublayer(engine, keys); err != nil {
			return err
		}
		return ensureWFPFilters(engine, ownerSID, keys)
	}); err != nil {
		return wfpKeys{}, err
	}
	if err := verifyWFPFilters(engine, ownerSID, keys); err != nil {
		return keys, err
	}
	return keys, nil
}

func openWFP() (windows.Handle, error) {
	proc := fwpmDLL.NewProc("FwpmEngineOpen0")
	if err := proc.Find(); err != nil {
		return 0, err
	}
	var engine windows.Handle
	r1, _, _ := proc.Call(0, 10, 0, 0, uintptr(unsafe.Pointer(&engine)))
	if r1 != 0 {
		return 0, syscall.Errno(r1)
	}
	return engine, nil
}

func closeWFP(engine windows.Handle) error {
	if engine == 0 {
		return nil
	}
	r1, _, _ := fwpmDLL.NewProc("FwpmEngineClose0").Call(uintptr(engine))
	if r1 != 0 {
		return fmt.Errorf("close WFP engine: %w", syscall.Errno(r1))
	}
	return nil
}

func wfpTransaction(engine windows.Handle, apply func() error) error {
	return performWFPTransaction(
		func() error {
			r1, _, _ := fwpmDLL.NewProc("FwpmTransactionBegin0").Call(uintptr(engine), 0)
			if r1 != 0 {
				return fmt.Errorf("begin persistent WFP transaction: %w", syscall.Errno(r1))
			}
			return nil
		},
		apply,
		func() error {
			r1, _, _ := fwpmDLL.NewProc("FwpmTransactionCommit0").Call(uintptr(engine))
			if r1 != 0 {
				return fmt.Errorf("commit WFP transaction: %w", syscall.Errno(r1))
			}
			return nil
		},
		func() error {
			r1, _, _ := fwpmDLL.NewProc("FwpmTransactionAbort0").Call(uintptr(engine))
			if r1 != 0 {
				return fmt.Errorf("abort WFP transaction: %w", syscall.Errno(r1))
			}
			return nil
		},
	)
}

func performWFPTransaction(begin, apply, commit, abort func() error) error {
	if err := begin(); err != nil {
		return err
	}
	if err := apply(); err != nil {
		return errors.Join(err, abort())
	}
	if err := commit(); err != nil {
		return errors.Join(err, abort())
	}
	return nil
}

func ensureWFPProvider(engine windows.Handle, key windows.GUID) error {
	name, _ := windows.UTF16PtrFromString("O Agent per-user Windows sandbox")
	provider := fwpmProvider0{Key: key, DisplayData: fwpmDisplayData0{Name: name}, Flags: fwpmProviderPersistent}
	r1, _, _ := fwpmDLL.NewProc("FwpmProviderAdd0").Call(uintptr(engine), uintptr(unsafe.Pointer(&provider)), 0)
	if r1 == 0 || uint32(r1) == fwpmAlreadyExists {
		return nil
	}
	return fmt.Errorf("install owned WFP provider: %w", syscall.Errno(r1))
}

func ensureWPFSublayer(engine windows.Handle, keys wfpKeys) error {
	name, _ := windows.UTF16PtrFromString("O Agent per-user outbound restrictions")
	sublayer := fwpmSublayer0{Key: keys.Sublayer, DisplayData: fwpmDisplayData0{Name: name}, Flags: fwpmSublayerPersistent, ProviderKey: &keys.Provider, Weight: 0x7fff}
	r1, _, _ := fwpmDLL.NewProc("FwpmSubLayerAdd0").Call(uintptr(engine), uintptr(unsafe.Pointer(&sublayer)), 0)
	if r1 == 0 || uint32(r1) == fwpmAlreadyExists {
		return nil
	}
	return fmt.Errorf("install owned WFP sublayer: %w", syscall.Errno(r1))
}

func ensureWFPFilters(engine windows.Handle, ownerSID string, keys wfpKeys) error {
	offlineName, onlineName := sandboxAccountNames(ownerSID)
	offlineSID, err := lookupLocalAccountSID(offlineName)
	if err != nil {
		return err
	}
	onlineSID, err := lookupLocalAccountSID(onlineName)
	if err != nil {
		return err
	}
	const (
		connectV4 = "c38d57d1-05a7-4c33-904f-7fbceee60e82"
		connectV6 = "4a72393b-319f-44bc-84c3-ba54dcb3b6b4"
		listenV4  = "88bb5dad-76d7-4227-9c71-df0a3ed7be7e"
		listenV6  = "7ac9de24-17dd-4814-b4bd-a9fbc95a321b"
		recvV4    = "e1cd9fe7-f4b5-4273-96c0-592e487b8650"
		recvV6    = "a3b42c97-9f04-4672-b87e-cee9c483257f"
		userField = "af043a0a-b34d-4f86-979c-c90371af6e66"
	)
	type rule struct {
		name, layer string
		sid         *windows.SID
	}
	rules := []rule{
		{"offline-connect-v4", connectV4, offlineSID}, {"offline-connect-v6", connectV6, offlineSID},
		{"offline-listen-v4", listenV4, offlineSID}, {"offline-listen-v6", listenV6, offlineSID},
		{"offline-receive-v4", recvV4, offlineSID}, {"offline-receive-v6", recvV6, offlineSID},
		{"online-listen-v4", listenV4, onlineSID}, {"online-listen-v6", listenV6, onlineSID},
		{"online-receive-v4", recvV4, onlineSID}, {"online-receive-v6", recvV6, onlineSID},
	}
	userKey, err := windows.GUIDFromString(userField)
	if err != nil {
		return err
	}
	for i, rule := range rules {
		layer, err := windows.GUIDFromString(rule.layer)
		if err != nil {
			return err
		}
		if err := addUserBlockFilter(engine, keys, keys.Filters[i], layer, userKey, rule.sid); err != nil {
			return fmt.Errorf("install %s WFP rule: %w", rule.name, err)
		}
	}
	return nil
}

func addUserBlockFilter(engine windows.Handle, keys wfpKeys, filterKey, layer, userField windows.GUID, sid *windows.SID) error {
	sd, err := windows.SecurityDescriptorFromString("D:(A;;0x1;;;" + sid.String() + ")")
	if err != nil {
		return fmt.Errorf("build WFP user match descriptor: %w", err)
	}
	length, _, _ := windows.NewLazySystemDLL("advapi32.dll").NewProc("GetSecurityDescriptorLength").Call(uintptr(unsafe.Pointer(sd)))
	if length == 0 || length > 64*1024 {
		return fmt.Errorf("invalid WFP user match descriptor length %d", length)
	}
	bytes := unsafe.Slice((*byte)(unsafe.Pointer(sd)), int(length))
	blob := fwpByteBlob{Size: uint32(length), Data: &bytes[0]}
	condition := fwpmFilterCondition0{FieldKey: userField, MatchType: fwpMatchEqual, ConditionValue: fwpConditionValue0{Type: fwpSecurityDescriptorType, Value: uintptr(unsafe.Pointer(&blob))}}
	name, _ := windows.UTF16PtrFromString("O Agent sandbox network restriction")
	weight := fwpValue0{Type: fwpUint8, Value: 0xff}
	filter := fwpmFilter0{Key: filterKey, DisplayData: fwpmDisplayData0{Name: name}, Flags: fwpmFilterPersistent | fwpmClearActionRight, ProviderKey: &keys.Provider, LayerKey: layer, SublayerKey: keys.Sublayer, Weight: weight, NumFilterConditions: 1, FilterCondition: &condition, Action: fwpmAction0{Type: fwpActionBlock}}
	r1, _, _ := fwpmDLL.NewProc("FwpmFilterAdd0").Call(uintptr(engine), uintptr(unsafe.Pointer(&filter)), 0, 0)
	runtimeKeepAliveWFP(sd, bytes, blob, condition, filter, name)
	if r1 == 0 || uint32(r1) == fwpmAlreadyExists {
		return nil
	}
	return syscall.Errno(r1)
}

func runtimeKeepAliveWFP(values ...any) {
	for _, value := range values {
		runtime.KeepAlive(value)
	}
}

func verifyWFPFilters(engine windows.Handle, ownerSID string, keys wfpKeys) error {
	if len(keys.Filters) != 10 {
		return fmt.Errorf("owned WFP filter set has %d entries; expected 10", len(keys.Filters))
	}
	if err := verifyWFPProviderAndSublayer(engine, keys); err != nil {
		return err
	}
	expectations, err := wfpFilterExpectations(ownerSID)
	if err != nil {
		return err
	}
	for i, key := range keys.Filters {
		var filter *fwpmFilter0
		r1, _, _ := fwpmDLL.NewProc("FwpmFilterGetByKey0").Call(uintptr(engine), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&filter)))
		if r1 != 0 {
			return fmt.Errorf("read back WFP rule %d: %w", i, syscall.Errno(r1))
		}
		if filter == nil {
			return fmt.Errorf("WFP rule %d returned no filter", i)
		}
		valid := filter.Key == key && filter.Flags&fwpmFilterPersistent != 0 && filter.Flags&fwpmClearActionRight != 0 &&
			filter.ProviderKey != nil && *filter.ProviderKey == keys.Provider && filter.SublayerKey == keys.Sublayer &&
			filter.Action.Type == fwpActionBlock && filter.NumFilterConditions == 1 && filter.FilterCondition != nil
		if valid {
			expectation := expectations[i]
			condition := filter.FilterCondition
			valid = condition.FieldKey == expectation.UserField && condition.MatchType == fwpMatchEqual &&
				condition.ConditionValue.Type == fwpSecurityDescriptorType && condition.ConditionValue.Value != 0 &&
				filter.LayerKey == expectation.Layer && filter.Weight.Type == fwpUint8 && uint8(filter.Weight.Value) == 0xff
			if valid {
				value := (*fwpConditionValuePointer)(unsafe.Pointer(&condition.ConditionValue))
				blob := (*fwpByteBlob)(value.Value)
				if blob == nil || blob.Data == nil || blob.Size == 0 || blob.Size > 64*1024 {
					valid = false
				} else {
					sd := (*windows.SECURITY_DESCRIPTOR)(unsafe.Pointer(blob.Data))
					expectedSD, sdErr := windows.SecurityDescriptorFromString("D:(A;;0x1;;;" + expectation.AccountSID.String() + ")")
					valid = sdErr == nil && sd.IsValid() && strings.EqualFold(sd.String(), expectedSD.String())
				}
			}
		}
		if !valid {
			freeWFP(unsafe.Pointer(filter))
			return fmt.Errorf("WFP rule %d read-back does not match its exact persistent user block policy", i)
		}
		freeWFP(unsafe.Pointer(filter))
	}
	return nil
}

type wfpFilterExpectation struct {
	Layer      windows.GUID
	UserField  windows.GUID
	AccountSID *windows.SID
}

func wfpFilterExpectations(ownerSID string) ([]wfpFilterExpectation, error) {
	offlineName, onlineName := sandboxAccountNames(ownerSID)
	offlineSID, err := lookupLocalAccountSID(offlineName)
	if err != nil {
		return nil, fmt.Errorf("read Offline account SID for WFP Probe: %w", err)
	}
	onlineSID, err := lookupLocalAccountSID(onlineName)
	if err != nil {
		return nil, fmt.Errorf("read Online account SID for WFP Probe: %w", err)
	}
	layers := []string{
		"c38d57d1-05a7-4c33-904f-7fbceee60e82", "4a72393b-319f-44bc-84c3-ba54dcb3b6b4",
		"88bb5dad-76d7-4227-9c71-df0a3ed7be7e", "7ac9de24-17dd-4814-b4bd-a9fbc95a321b",
		"e1cd9fe7-f4b5-4273-96c0-592e487b8650", "a3b42c97-9f04-4672-b87e-cee9c483257f",
		"88bb5dad-76d7-4227-9c71-df0a3ed7be7e", "7ac9de24-17dd-4814-b4bd-a9fbc95a321b",
		"e1cd9fe7-f4b5-4273-96c0-592e487b8650", "a3b42c97-9f04-4672-b87e-cee9c483257f",
	}
	userField, err := windows.GUIDFromString("af043a0a-b34d-4f86-979c-c90371af6e66")
	if err != nil {
		return nil, err
	}
	result := make([]wfpFilterExpectation, 0, len(layers))
	for i, layerText := range layers {
		layer, err := windows.GUIDFromString(layerText)
		if err != nil {
			return nil, err
		}
		sid := offlineSID
		if i >= 6 {
			sid = onlineSID
		}
		result = append(result, wfpFilterExpectation{Layer: layer, UserField: userField, AccountSID: sid})
	}
	return result, nil
}

func verifyWFPProviderAndSublayer(engine windows.Handle, keys wfpKeys) error {
	var provider *fwpmProvider0
	r1, _, _ := fwpmDLL.NewProc("FwpmProviderGetByKey0").Call(uintptr(engine), uintptr(unsafe.Pointer(&keys.Provider)), uintptr(unsafe.Pointer(&provider)))
	if r1 != 0 {
		return fmt.Errorf("read back owned WFP provider: %w", syscall.Errno(r1))
	}
	providerValid := provider != nil && provider.Key == keys.Provider && provider.Flags&fwpmProviderPersistent != 0
	freeWFP(unsafe.Pointer(provider))
	if !providerValid {
		return errors.New("owned WFP provider read-back does not match the persistent provider")
	}
	var sublayer *fwpmSublayer0
	r1, _, _ = fwpmDLL.NewProc("FwpmSubLayerGetByKey0").Call(uintptr(engine), uintptr(unsafe.Pointer(&keys.Sublayer)), uintptr(unsafe.Pointer(&sublayer)))
	if r1 != 0 {
		return fmt.Errorf("read back owned WFP sublayer: %w", syscall.Errno(r1))
	}
	sublayerValid := sublayer != nil && sublayer.Key == keys.Sublayer && sublayer.Flags&fwpmSublayerPersistent != 0 &&
		sublayer.ProviderKey != nil && *sublayer.ProviderKey == keys.Provider && sublayer.Weight == 0x7fff
	freeWFP(unsafe.Pointer(sublayer))
	if !sublayerValid {
		return errors.New("owned WFP sublayer read-back does not match the persistent sublayer")
	}
	return nil
}

func freeWFP(ptr unsafe.Pointer) {
	if ptr == nil {
		return
	}
	_, _, _ = fwpmDLL.NewProc("FwpmFreeMemory0").Call(uintptr(unsafe.Pointer(&ptr)))
}

func removeOwnedWFP(ownerSID string) (resultErr error) {
	keys, err := ownedWFPKeyIdentifiers(ownerSID)
	if err != nil {
		return err
	}
	engine, err := openWFP()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeWFP(engine)) }()
	var joined error
	for i := len(keys.Filters) - 1; i >= 0; i-- {
		key := keys.Filters[i]
		r1, _, _ := fwpmDLL.NewProc("FwpmFilterDeleteByKey0").Call(uintptr(engine), uintptr(unsafe.Pointer(&key)))
		if r1 != 0 && uint32(r1) != uint32(0x80320003) { // FWP_E_FILTER_NOT_FOUND
			joined = errors.Join(joined, fmt.Errorf("remove owned WFP filter %d: %w", i, syscall.Errno(r1)))
		}
	}
	for _, item := range []struct {
		name     string
		key      windows.GUID
		notFound uint32
	}{
		{"sublayer", keys.Sublayer, uint32(windows.FWP_E_SUBLAYER_NOT_FOUND)},
		{"provider", keys.Provider, uint32(windows.FWP_E_PROVIDER_NOT_FOUND)},
	} {
		procName := "FwpmSubLayerDeleteByKey0"
		if item.name == "provider" {
			procName = "FwpmProviderDeleteByKey0"
		}
		r1, _, _ := fwpmDLL.NewProc(procName).Call(uintptr(engine), uintptr(unsafe.Pointer(&item.key)))
		if r1 != 0 && uint32(r1) != item.notFound {
			joined = errors.Join(joined, fmt.Errorf("remove owned WFP %s: %w", item.name, syscall.Errno(r1)))
		}
	}
	if joined != nil {
		return joined
	}
	if err := verifyOwnedWFPAbsentWithEngine(engine, keys); err != nil {
		return fmt.Errorf("verify removal of owned WFP policy: %w", err)
	}
	return nil
}

func verifyOwnedWFPAbsent(ownerSID string) (resultErr error) {
	keys, err := ownedWFPKeyIdentifiers(ownerSID)
	if err != nil {
		return err
	}
	engine, err := openWFP()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeWFP(engine)) }()
	return verifyOwnedWFPAbsentWithEngine(engine, keys)
}

func ownedWFPKeyIdentifiers(ownerSID string) (wfpKeys, error) {
	provider, err := stableGUID("provider:" + ownerSID)
	if err != nil {
		return wfpKeys{}, err
	}
	sublayer, err := stableGUID("sublayer:" + ownerSID)
	if err != nil {
		return wfpKeys{}, err
	}
	keys := wfpKeys{Provider: provider, Sublayer: sublayer}
	for _, name := range []string{
		"offline-connect-v4", "offline-connect-v6", "offline-listen-v4", "offline-listen-v6", "offline-receive-v4", "offline-receive-v6",
		"online-listen-v4", "online-listen-v6", "online-receive-v4", "online-receive-v6",
	} {
		key, err := stableGUID("filter:" + ownerSID + ":" + name)
		if err != nil {
			return wfpKeys{}, err
		}
		keys.Filters = append(keys.Filters, key)
	}
	return keys, nil
}

func verifyOwnedWFPAbsentWithEngine(engine windows.Handle, keys wfpKeys) error {
	for i, key := range keys.Filters {
		var filter *fwpmFilter0
		r1, _, _ := fwpmDLL.NewProc("FwpmFilterGetByKey0").Call(uintptr(engine), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&filter)))
		if r1 == 0 {
			freeWFP(unsafe.Pointer(filter))
			return fmt.Errorf("owned WFP filter %d remains after removal", i)
		}
		if uint32(r1) != uint32(windows.FWP_E_FILTER_NOT_FOUND) {
			return fmt.Errorf("verify removed WFP filter %d: %w", i, syscall.Errno(r1))
		}
	}
	var sublayer *fwpmSublayer0
	r1, _, _ := fwpmDLL.NewProc("FwpmSubLayerGetByKey0").Call(uintptr(engine), uintptr(unsafe.Pointer(&keys.Sublayer)), uintptr(unsafe.Pointer(&sublayer)))
	if r1 == 0 {
		freeWFP(unsafe.Pointer(sublayer))
		return errors.New("owned WFP sublayer remains after removal")
	}
	if uint32(r1) != uint32(windows.FWP_E_SUBLAYER_NOT_FOUND) {
		return fmt.Errorf("verify removed WFP sublayer: %w", syscall.Errno(r1))
	}
	var provider *fwpmProvider0
	r1, _, _ = fwpmDLL.NewProc("FwpmProviderGetByKey0").Call(uintptr(engine), uintptr(unsafe.Pointer(&keys.Provider)), uintptr(unsafe.Pointer(&provider)))
	if r1 == 0 {
		freeWFP(unsafe.Pointer(provider))
		return errors.New("owned WFP provider remains after removal")
	}
	if uint32(r1) != uint32(windows.FWP_E_PROVIDER_NOT_FOUND) {
		return fmt.Errorf("verify removed WFP provider: %w", syscall.Errno(r1))
	}
	return nil
}

func wfpKeyStrings(keys wfpKeys) []string {
	result := []string{keys.Provider.String(), keys.Sublayer.String()}
	for _, key := range keys.Filters {
		result = append(result, key.String())
	}
	return result
}

func parseWFPKeyStrings(values []string) ([]windows.GUID, error) {
	result := make([]windows.GUID, 0, len(values))
	for _, value := range values {
		key, err := windows.GUIDFromString(value)
		if err != nil {
			return nil, fmt.Errorf("invalid owned WFP key %q: %w", value, err)
		}
		result = append(result, key)
	}
	return result, nil
}

func wfpDigest(ownerSID string) string {
	digest := sha256.Sum256([]byte(ownerSID))
	return hex.EncodeToString(digest[:8])
}
