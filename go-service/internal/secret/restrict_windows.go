//go:build windows

package secret

import "golang.org/x/sys/windows"

// Restrict gives a secret file (access token, key file) to the backend's own
// account (and SYSTEM) only. Go's file mode bits do not set Windows access,
// and a data folder may pass wider access down to new files, so the
// inherited entries are replaced by an explicit, protected list.
func Restrict(path string) error {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;" + u.User.Sid.String() + ")(A;;FA;;;SY)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
