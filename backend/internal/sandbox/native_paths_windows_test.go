//go:build windows

package sandbox

import (
	"os"
	"path/filepath"
	"strings"
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
	actual := nativeDACLSDDL(readBack.String())
	if strings.Contains(actual, "WD") || !strings.Contains(actual, strings.ToUpper(owner.User.Sid.String())) ||
		!strings.Contains(actual, "SY") || !strings.Contains(actual, "BA") {
		t.Fatalf("repaired directory DACL is missing its protected principals or still grants Everyone: %s", actual)
	}
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
	actual := nativeDACLSDDL(readBack.String())
	if strings.Contains(actual, "WD") || !strings.Contains(actual, strings.ToUpper(owner.User.Sid.String())) ||
		!strings.Contains(actual, "SY") || !strings.Contains(actual, "BA") || !strings.Contains(actual, "BU") {
		t.Fatalf("runner DACL read-back does not match its protected principals: %s", actual)
	}
}
