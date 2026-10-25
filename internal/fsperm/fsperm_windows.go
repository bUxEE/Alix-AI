//go:build windows

package fsperm

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// currentUserSID returns the SID of the user the process runs as: the service
// identity the data is restricted to.
func currentUserSID() (*windows.SID, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}

func restrict(path string, dir bool) error {
	sid, err := currentUserSID()
	if err != nil {
		return err
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if dir {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       inherit,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}

// ACE types of a DACL that only ever take access away. Every other type is
// treated as one that may grant it: an allow entry of any kind (object,
// callback, conditional) is a possible reader, and a type this code does not
// know is refused rather than skipped.
const (
	aceAccessDenied               = 0x1
	aceAccessDeniedObject         = 0x6
	aceAccessDeniedCallback       = 0xA
	aceAccessDeniedCallbackObject = 0xC
)

// tolerated are the principals an allow entry may name besides the current
// user: LocalSystem and the built-in Administrators, which can take ownership
// of any file whatever its DACL says, and CREATOR OWNER, an inheritable entry
// that stands for whoever creates something inside, which only a principal
// already allowed can do.
func tolerated() []*windows.SID {
	var out []*windows.SID
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid, windows.WinCreatorOwnerSid} {
		if sid, err := windows.CreateWellKnownSid(t); err == nil {
			out = append(out, sid)
		}
	}
	return out
}

func check(path string, _ os.FileInfo) error {
	me, err := currentUserSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		// No DACL means everyone has full access.
		return fmt.Errorf("%s has no access control list: %w", path, ErrNotOwnerOnly)
	}
	// Every entry counts, inherited or explicit: what decides is who the DACL
	// lets in now. A file placed in an owner-only directory (a key file copied
	// back, a restored database) inherits only the owner's entry and passes; one
	// under a directory that grants Authenticated Users does not.
	allowed := append([]*windows.SID{me}, tolerated()...)
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, i, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case aceAccessDenied, aceAccessDeniedObject, aceAccessDeniedCallback, aceAccessDeniedCallbackObject:
			continue // a deny entry cannot widen access
		case windows.ACCESS_ALLOWED_ACE_TYPE:
		default:
			// Object and callback allow entries keep their SID at another offset;
			// whoever they name, they are not an entry this package writes.
			return fmt.Errorf("%s has an access entry of type %d that may grant access: %w", path, ace.Header.AceType, ErrNotOwnerOnly)
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		ok := false
		for _, a := range allowed {
			if windows.EqualSid(sid, a) {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s grants access to %s: %w", path, sid.String(), ErrNotOwnerOnly)
		}
	}
	return nil
}

func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
