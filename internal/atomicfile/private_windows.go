//go:build windows

package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	privatePeerPermissionMask os.FileMode = 0o077
	privateACLEntryCount                  = 3
)

func protectPrivatePath(path string, mode os.FileMode) error {
	if mode.Perm()&privatePeerPermissionMask != 0 {
		return nil
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name,
		windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(h), path)
	if f == nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("atomicfile: retain private file descriptor")
	}
	defer f.Close()
	return protectPrivateFile(f, mode)
}

func protectPrivateFileAtPath(original *os.File, path string, mode os.FileMode) error {
	if mode.Perm()&privatePeerPermissionMask != 0 {
		return nil
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(name,
		windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	repair := os.NewFile(uintptr(h), path)
	if repair == nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("atomicfile: retain ACL repair descriptor")
	}
	defer repair.Close()
	if err := samePrivateFileIdentity(original, repair); err != nil {
		return err
	}
	return protectPrivateFile(repair, mode)
}

func protectPrivateFile(f *os.File, mode os.FileMode) error {
	if mode.Perm()&privatePeerPermissionMask != 0 {
		return nil
	}
	var fileInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &fileInfo); err != nil {
		return err
	}
	if fileInfo.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || fileInfo.NumberOfLinks != 1 {
		return fmt.Errorf("atomicfile: unsafe private file identity")
	}

	user, err := currentUserSID()
	if err != nil {
		return err
	}
	ownerSID, err := currentOwnerSID()
	if err != nil {
		return err
	}
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || (!owner.Equals(ownerSID) && !owner.Equals(user)) {
		return fmt.Errorf("atomicfile: private file owner mismatch")
	}

	acl, err := privateFileACL(user)
	if err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return err
	}
	return validatePrivateFileDescriptor(f, ownerSID, user)
}

func samePrivateFileIdentity(original, repair *os.File) error {
	var left, right windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(original.Fd()), &left); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(repair.Fd()), &right); err != nil {
		return err
	}
	if left.VolumeSerialNumber != right.VolumeSerialNumber || left.FileIndexHigh != right.FileIndexHigh || left.FileIndexLow != right.FileIndexLow {
		return fmt.Errorf("atomicfile: file identity changed during ACL repair")
	}
	for _, info := range []windows.ByHandleFileInformation{left, right} {
		if info.FileAttributes&(windows.FILE_ATTRIBUTE_DIRECTORY|windows.FILE_ATTRIBUTE_REPARSE_POINT) != 0 || info.NumberOfLinks != 1 {
			return fmt.Errorf("atomicfile: unsafe private file identity")
		}
	}
	return nil
}

func currentUserSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	u, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid.Copy()
}

type tokenOwner struct {
	Owner *windows.SID
}

func currentOwnerSID() (*windows.SID, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()
	var size uint32
	err = windows.GetTokenInformation(token, windows.TokenOwner, nil, 0, &size)
	if err != nil && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
		return nil, err
	}
	if size == 0 {
		return nil, fmt.Errorf("atomicfile: Windows token owner is absent")
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], size, &size); err != nil {
		return nil, err
	}
	owner := (*tokenOwner)(unsafe.Pointer(&buffer[0])).Owner
	if owner == nil || !owner.IsValid() {
		return nil, fmt.Errorf("atomicfile: Windows token owner is invalid")
	}
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return nil, err
	}
	if !owner.Equals(user) && !owner.Equals(admins) {
		return nil, fmt.Errorf("atomicfile: unsupported Windows token owner")
	}
	return owner.Copy()
}

func privateFileACL(user *windows.SID) (*windows.ACL, error) {
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return nil, err
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return nil, err
	}
	entries := make([]windows.EXPLICIT_ACCESS, 0, privateACLEntryCount)
	for _, sid := range []*windows.SID{user, system, admins} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	return windows.ACLFromEntries(entries, nil)
}

func validatePrivateFileDescriptor(f *os.File, ownerSID, user *windows.SID) error {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || (!owner.Equals(ownerSID) && !owner.Equals(user)) {
		return fmt.Errorf("atomicfile: private file owner mismatch")
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("atomicfile: private DACL is inherited")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount == 0 {
		return fmt.Errorf("atomicfile: private DACL is absent")
	}
	system, _ := windows.StringToSid("S-1-5-18")
	admins, _ := windows.StringToSid("S-1-5-32-544")
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("atomicfile: unsupported private DACL entry")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || (!sid.Equals(user) && !sid.Equals(system) && !sid.Equals(admins)) {
			return fmt.Errorf("atomicfile: private DACL grants an untrusted principal")
		}
	}
	return nil
}
