//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestSecurePrivateNativeDirectoryRepairsExistingDACLAndReadsItBack(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "cache")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := setNativePathDACL(directory, "D:P(A;OICI;FA;;;WD)"); err != nil {
		t.Fatalf("seed overly broad existing directory DACL: %v", err)
	}
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := securePrivateNativeDirectory(directory, owner.User.Sid.String()); err != nil {
		t.Fatalf("secure pre-existing private directory: %v", err)
	}
	readBack, err := windows.GetNamedSecurityInfo(directory, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read repaired directory DACL: %v", err)
	}
	if readBack == nil {
		t.Fatal("secured directory returned no security descriptor")
	}
	assertProtectedNativePrincipals(t, readBack, owner.User.Sid, false)
}

func TestSecureNativeFileReadsBackProtectedDACL(t *testing.T) {
	file := filepath.Join(t.TempDir(), "runner.exe")
	if err := os.WriteFile(file, []byte("runner"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if err := secureNativeFile(file, owner.User.Sid.String(), true); err != nil {
		t.Fatalf("secure runner file: %v", err)
	}
	readBack, err := windows.GetNamedSecurityInfo(file, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("read protected runner DACL: %v", err)
	}
	if readBack == nil {
		t.Fatal("secured runner returned no security descriptor")
	}
	assertProtectedNativePrincipals(t, readBack, owner.User.Sid, true)
}
func assertProtectedNativePrincipals(t *testing.T, descriptor *windows.SECURITY_DESCRIPTOR, owner *windows.SID, runner bool) {
	t.Helper()
	if !nativeDACLProtected(descriptor.String()) {
		t.Fatalf("native path DACL is not protected: %s", descriptor.String())
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	principals := []*windows.SID{owner}
	wanted := []string{"S-1-5-18", "S-1-5-32-544"}
	if runner {
		wanted = append(wanted, "S-1-5-32-545")
	}
	for _, value := range wanted {
		sid, err := windows.StringToSid(value)
		if err != nil {
			t.Fatal(err)
		}
		principals = append(principals, sid)
	}
	for _, principal := range principals {
		if present, err := explicitACLHasSID(acl, principal, false); err != nil || !present {
			t.Fatalf("protected DACL lacks principal %s: present=%v err=%v DACL=%s", principal.String(), present, err, descriptor.String())
		}
	}
	world, err := windows.StringToSid("S-1-1-0")
	if err != nil {
		t.Fatal(err)
	}
	if present, err := explicitACLHasSID(acl, world, false); err != nil || present {
		t.Fatalf("protected DACL still grants Everyone: present=%v err=%v", present, err)
	}
}
