//go:build windows

package fsperm

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// setDACL replaces the DACL of path with a protected one holding entries.
func setDACL(t *testing.T, path string, entries []windows.EXPLICIT_ACCESS) {
	t.Helper()
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
}

func grant(sid *windows.SID, mode windows.ACCESS_MODE) windows.EXPLICIT_ACCESS {
	return windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_READ, AccessMode: mode, Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(sid)},
	}
}

func wellKnown(t *testing.T, k windows.WELL_KNOWN_SID_TYPE) *windows.SID {
	t.Helper()
	sid, err := windows.CreateWellKnownSid(k)
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

// The DACL check: a protected DACL that grants only the owner, LocalSystem and
// Administrators passes; one more reader (Authenticated Users, as an agent
// found under E:\) does not; a deny entry does not widen access, so it does
// not fail the check.
func TestWindowsDACLCheck(t *testing.T) {
	me, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	// In an owner-only directory: what the file inherits is then known.
	dir := filepath.Join(t.TempDir(), "keys")
	if _, err := MkdirOwnerOnly(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "k.key")
	f, err := CreateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	setDACL(t, path, []windows.EXPLICIT_ACCESS{grant(me, windows.GRANT_ACCESS),
		grant(wellKnown(t, windows.WinLocalSystemSid), windows.GRANT_ACCESS),
		grant(wellKnown(t, windows.WinBuiltinAdministratorsSid), windows.GRANT_ACCESS)})
	if err := Check(path, false); err != nil {
		t.Fatalf("owner, SYSTEM and Administrators: %v", err)
	}

	setDACL(t, path, []windows.EXPLICIT_ACCESS{grant(me, windows.GRANT_ACCESS),
		grant(wellKnown(t, windows.WinAuthenticatedUserSid), windows.GRANT_ACCESS)})
	if err := Check(path, false); !errors.Is(err, ErrNotOwnerOnly) {
		t.Fatalf("Authenticated Users may read: %v", err)
	}

	setDACL(t, path, []windows.EXPLICIT_ACCESS{grant(me, windows.GRANT_ACCESS),
		grant(wellKnown(t, windows.WinWorldSid), windows.DENY_ACCESS)})
	if err := Check(path, false); err != nil {
		t.Fatalf("a deny entry for Everyone: %v", err)
	}

	// Unprotected: the file also takes the inheritable entry of its owner-only
	// directory: still owner only, because what counts is who the entries let
	// in, inherited or not.
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{grant(me, windows.GRANT_ACCESS)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.UNPROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if err := Check(path, false); err != nil {
		t.Fatalf("an inheriting DACL whose entries are all the owner's: %v", err)
	}
}
