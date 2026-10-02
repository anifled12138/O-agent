# Run from an elevated PowerShell prompt. Adds only the minimum NUL-device
# access AppContainer runtimes need for /dev/null, preserving all other ACEs.
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run this script from an elevated PowerShell prompt (Run as administrator).'
}

Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;
using System.Security.AccessControl;

public static class AppContainerNullAccess {
    [StructLayout(LayoutKind.Sequential)]
    private struct Trustee {
        public IntPtr Multiple;
        public int MultipleOperation;
        public int Form;
        public int Type;
        public IntPtr Name;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct ExplicitAccess {
        public uint Permissions;
        public int Mode;
        public uint Inheritance;
        public Trustee Trustee;
    }

    [DllImport("advapi32.dll", CharSet = CharSet.Unicode)]
    private static extern uint GetNamedSecurityInfo(string name, int objectType, uint info,
        out IntPtr owner, out IntPtr group, out IntPtr dacl, out IntPtr sacl, out IntPtr descriptor);
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode)]
    private static extern uint SetNamedSecurityInfo(string name, int objectType, uint info,
        IntPtr owner, IntPtr group, IntPtr dacl, IntPtr sacl);
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool ConvertStringSidToSid(string sid, out IntPtr result);
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode)]
    private static extern uint SetEntriesInAcl(uint count, ref ExplicitAccess entries, IntPtr oldAcl, out IntPtr newAcl);
    [DllImport("advapi32.dll")]
    private static extern uint GetSecurityDescriptorLength(IntPtr descriptor);
    [DllImport("kernel32.dll")]
    private static extern IntPtr LocalFree(IntPtr value);

    private const uint DaclSecurityInformation = 4;
    private const uint FileGenericReadWrite = 0x0012019f;
    private const string AllApplicationPackagesSid = "S-1-15-2-1";

    private static RawSecurityDescriptor Read(out IntPtr dacl, out IntPtr descriptor) {
        IntPtr owner, group, sacl;
        uint status = GetNamedSecurityInfo("NUL", 1, DaclSecurityInformation,
            out owner, out group, out dacl, out sacl, out descriptor);
        if (status != 0) throw new Win32Exception((int)status, "Read NUL device ACL failed");
        int length = checked((int)GetSecurityDescriptorLength(descriptor));
        byte[] bytes = new byte[length];
        Marshal.Copy(descriptor, bytes, 0, length);
        return new RawSecurityDescriptor(bytes, 0);
    }

    private static bool HasReadWriteAce(RawSecurityDescriptor descriptor) {
        if (descriptor.DiscretionaryAcl == null) return false;
        foreach (GenericAce rawAce in descriptor.DiscretionaryAcl) {
            CommonAce ace = rawAce as CommonAce;
            if (ace != null && ace.AceQualifier == AceQualifier.AccessAllowed &&
                ace.SecurityIdentifier.Value == AllApplicationPackagesSid &&
                (unchecked((uint)ace.AccessMask) & FileGenericReadWrite) == FileGenericReadWrite) return true;
        }
        return false;
    }

    public static string GrantAndVerify() {
        IntPtr originalDacl, descriptor, sid = IntPtr.Zero, newAcl = IntPtr.Zero;
        RawSecurityDescriptor before = Read(out originalDacl, out descriptor);
        try {
            if (HasReadWriteAce(before)) return "NUL ACL already grants read/write to ALL APPLICATION PACKAGES.";
            if (!ConvertStringSidToSid(AllApplicationPackagesSid, out sid))
                throw new Win32Exception(Marshal.GetLastWin32Error(), "Resolve ALL APPLICATION PACKAGES SID failed");

            ExplicitAccess entry = new ExplicitAccess();
            entry.Permissions = FileGenericReadWrite;
            entry.Mode = 1; // GRANT_ACCESS: preserve all existing ACEs.
            entry.Trustee.Form = 0; // TRUSTEE_IS_SID
            entry.Trustee.Type = 5; // TRUSTEE_IS_WELL_KNOWN_GROUP
            entry.Trustee.Name = sid;
            uint status = SetEntriesInAcl(1, ref entry, originalDacl, out newAcl);
            if (status != 0) throw new Win32Exception((int)status, "Build updated NUL ACL failed");
            status = SetNamedSecurityInfo("NUL", 1, DaclSecurityInformation,
                IntPtr.Zero, IntPtr.Zero, newAcl, IntPtr.Zero);
            if (status != 0) throw new Win32Exception((int)status, "Write NUL ACL failed");
        } finally {
            if (newAcl != IntPtr.Zero) LocalFree(newAcl);
            if (sid != IntPtr.Zero) LocalFree(sid);
            if (descriptor != IntPtr.Zero) LocalFree(descriptor);
        }

        IntPtr verifyDacl, verifyDescriptor;
        RawSecurityDescriptor after = Read(out verifyDacl, out verifyDescriptor);
        try {
            if (!HasReadWriteAce(after)) throw new Exception("NUL ACL readback did not contain the requested ACE.");
            return "Verified: ALL APPLICATION PACKAGES can read/write NUL. Writes are discarded by the device.";
        } finally { if (verifyDescriptor != IntPtr.Zero) LocalFree(verifyDescriptor); }
    }
}
'@

[AppContainerNullAccess]::GrantAndVerify()
