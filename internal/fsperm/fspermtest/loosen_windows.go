//go:build windows

package fspermtest

import (
	"os"

	"golang.org/x/sys/windows"
)

func loosen(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	users, err := windows.CreateWellKnownSid(windows.WinAuthenticatedUserSid)
	if err != nil {
		return err
	}
	inherit := uint32(windows.NO_INHERITANCE)
	if st.IsDir() {
		inherit = windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT
	}
	entry := func(sid *windows.SID, access windows.ACCESS_MASK, kind windows.TRUSTEE_TYPE) windows.EXPLICIT_ACCESS {
		return windows.EXPLICIT_ACCESS{
			AccessPermissions: access, AccessMode: windows.GRANT_ACCESS, Inheritance: inherit,
			Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: kind, TrusteeValue: windows.TrusteeValueFromSID(sid)},
		}
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{
		entry(u.User.Sid, windows.GENERIC_ALL, windows.TRUSTEE_IS_USER),
		entry(users, windows.GENERIC_READ, windows.TRUSTEE_IS_WELL_KNOWN_GROUP),
	}, nil)
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
