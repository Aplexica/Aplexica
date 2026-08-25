//go:build windows

package privatefs

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func regularReadOpenFlags() int { return os.O_RDONLY }

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

// currentOwnerSID returns the SID Windows assigns as the owner of objects
// created by the current token. For an elevated local administrator this is
// normally BUILTIN\Administrators rather than TokenUser, even when the
// process is running under an individual account.
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
		return nil, fmt.Errorf("privatefs: Windows token owner is absent")
	}
	buffer := make([]byte, size)
	if err := windows.GetTokenInformation(token, windows.TokenOwner, &buffer[0], size, &size); err != nil {
		return nil, err
	}
	owner := (*tokenOwner)(unsafe.Pointer(&buffer[0])).Owner
	if owner == nil || !owner.IsValid() {
		return nil, fmt.Errorf("privatefs: Windows token owner is invalid")
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
		return nil, fmt.Errorf("privatefs: unsupported Windows token owner")
	}
	return owner.Copy()
}

func privateACL() (*windows.ACL, error) {
	user, err := currentUserSID()
	if err != nil {
		return nil, err
	}
	system, err := windows.StringToSid("S-1-5-18")
	if err != nil {
		return nil, err
	}
	admins, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		return nil, err
	}
	inherit := uint32(windows.CONTAINER_INHERIT_ACE | windows.OBJECT_INHERIT_ACE)
	entries := make([]windows.EXPLICIT_ACCESS, 0, 3)
	for _, sid := range []*windows.SID{user, system, admins} {
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inherit,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeValue: windows.TrusteeValueFromSID(sid),
			},
		})
	}
	return windows.ACLFromEntries(entries, nil)
}

func ownerIsCurrent(sd *windows.SECURITY_DESCRIPTOR) (bool, error) {
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return false, err
	}
	user, err := currentUserSID()
	if err != nil {
		return false, err
	}
	tokenOwner, err := currentOwnerSID()
	return err == nil && (owner.Equals(user) || owner.Equals(tokenOwner)), err
}

func validatePrivateDescriptor(sd *windows.SECURITY_DESCRIPTOR, access DirAccess) error {
	owned, err := ownerIsCurrent(sd)
	if err != nil || !owned {
		return fmt.Errorf("privatefs: Windows owner mismatch")
	}
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if access == AccessPrivate && control&windows.SE_DACL_PROTECTED == 0 {
		return fmt.Errorf("privatefs: Windows DACL is inherited")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return fmt.Errorf("privatefs: Windows DACL is absent")
	}
	// AccessPrivate is normalized to a protected allow-list by every repair
	// path. Descriptor validation additionally rejects a trivially empty DACL;
	// effective access remains enforced by the kernel on every retained handle.
	if access == AccessPrivate && dacl.AceCount == 0 {
		return fmt.Errorf("privatefs: Windows private DACL has no entries")
	}
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	system, _ := windows.StringToSid("S-1-5-18")
	admins, _ := windows.StringToSid("S-1-5-32-544")
	dangerous := windows.ACCESS_MASK(windows.GENERIC_ALL | windows.GENERIC_WRITE | windows.WRITE_DAC | windows.WRITE_OWNER | windows.DELETE | windows.FILE_GENERIC_WRITE)
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		if ace.Header.AceType == windows.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return fmt.Errorf("privatefs: unsupported Windows allow ACE")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		trusted := sid.IsValid() && (sid.Equals(user) || sid.Equals(system) || sid.Equals(admins))
		if access == AccessPrivate && !trusted {
			return fmt.Errorf("privatefs: Windows DACL grants an untrusted principal")
		}
		if access == AccessIntegrityOnly && !trusted && ace.Mask&dangerous != 0 {
			return fmt.Errorf("privatefs: Windows DACL grants write access to an untrusted principal")
		}
	}
	return nil
}

func repairNamedPrivate(path string) error {
	acl, err := privateACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil)
}

func validateOrRepairDir(path string, info os.FileInfo, policy DirPolicy) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("privatefs: root is not a real directory")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	if err = validatePrivateDescriptor(sd, policy.Access); err == nil {
		return nil
	}
	owned, ownerErr := ownerIsCurrent(sd)
	if ownerErr != nil || !owned || !policy.RepairOwned || policy.Access != AccessPrivate {
		return err
	}
	if err := repairNamedPrivate(path); err != nil {
		return err
	}
	sd, err = windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	return validatePrivateDescriptor(sd, policy.Access)
}

func descriptorForFile(f *os.File) (*windows.SECURITY_DESCRIPTOR, error) {
	return windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
}

func validateDirInfo(info os.FileInfo, _ DirPolicy) error {
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("privatefs: expected directory")
	}
	return nil
}

func validateRegularFile(f *os.File, _ bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: expected regular file")
	}
	if err := validateSingleLinkFileHandle(f); err != nil {
		return err
	}
	sd, err := descriptorForFile(f)
	if err != nil {
		return err
	}
	return validatePrivateDescriptor(sd, AccessPrivate)
}

func validateIntegrityFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: expected regular file")
	}
	if err := validateSingleLinkFileHandle(f); err != nil {
		return err
	}
	sd, err := descriptorForFile(f)
	if err != nil {
		return err
	}
	return validatePrivateDescriptor(sd, AccessIntegrityOnly)
}

func ValidateRootHandle(r *os.Root, policy DirPolicy) error {
	info, err := r.Stat(".")
	if err != nil {
		return err
	}
	return validateDirInfo(info, policy)
}

func openRetainedDirectory(path string) (*os.File, error) { return os.Open(path) }

func validateRegularDirectoryHandle(f *os.File, policy DirPolicy) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := validateDirInfo(info, policy); err != nil {
		return err
	}
	sd, err := descriptorForFile(f)
	if err != nil {
		return err
	}
	return validatePrivateDescriptor(sd, policy.Access)
}

func validateRepairNode(info os.FileInfo, dir bool) error {
	if dir && !info.IsDir() {
		return fmt.Errorf("privatefs: expected directory")
	}
	if !dir && !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: expected regular file")
	}
	return nil
}

func validateSingleLinkFileHandle(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrUnsafeFileIdentity
	}
	if info.NumberOfLinks == 0 {
		return ErrOpenedFileUnlinked
	}
	if info.NumberOfLinks != 1 {
		return ErrUnsafeFileIdentity
	}
	return nil
}

func validateRepairHandle(f *os.File, dir bool) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := validateRepairNode(info, dir); err != nil {
		return err
	}
	var handleInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &handleInfo); err != nil {
		return err
	}
	if handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("privatefs: node identity is a reparse point")
	}
	if dir != (handleInfo.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		return fmt.Errorf("privatefs: node type mismatch")
	}
	sd, err := descriptorForFile(f)
	if err != nil {
		return err
	}
	owned, err := ownerIsCurrent(sd)
	if err != nil || !owned {
		return fmt.Errorf("privatefs: node owner mismatch")
	}
	if !dir {
		return validateSingleLinkFileHandle(f)
	}
	return nil
}

func (r *Root) hardenRelativeNode(rel string, f *os.File, dir bool) error {
	if rel == "." {
		// OpenRoot already normalized and validated the retained root before a
		// Root can be constructed. Avoid reopening it through an ambient path.
		return validateRegularDirectoryHandle(f, DirPolicy{Access: AccessPrivate})
	}
	// A harden is a write. On a directory it is also a write to every child,
	// because privateACL's ACEs are inheritable and SetSecurityInfo propagates
	// them to existing children. Skip the write when the node already satisfies
	// this function's exact post-condition: nothing is relaxed, because the check
	// IS the post-condition, read from the same handle the write would re-read at
	// the end. A descriptor read error is deliberately ignored — fall through and
	// harden.
	if sd, sdErr := descriptorForFile(f); sdErr == nil {
		if validatePrivateDescriptor(sd, AccessPrivate) == nil {
			return nil
		}
	}
	acl, err := privateACL()
	if err != nil {
		return err
	}
	name, err := windows.NewNTUnicodeString(rel)
	if err != nil {
		return err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(r.dir.Fd()),
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if dir {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	var iosb windows.IO_STATUS_BLOCK
	var allocation int64
	var repair windows.Handle
	err = windows.NtCreateFile(&repair,
		windows.READ_CONTROL|windows.WRITE_DAC|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		oa, &iosb, &allocation, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN, options, 0, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(repair)
	if err := sameWindowsFileIdentity(windows.Handle(f.Fd()), repair, dir); err != nil {
		return err
	}
	if err := windows.SetSecurityInfo(repair, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil); err != nil {
		return err
	}
	sd, err := descriptorForFile(f)
	if err != nil {
		return err
	}
	return validatePrivateDescriptor(sd, AccessPrivate)
}

func sameWindowsFileIdentity(original, repair windows.Handle, dir bool) error {
	var left, right windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(original, &left); err != nil {
		return err
	}
	if err := windows.GetFileInformationByHandle(repair, &right); err != nil {
		return err
	}
	if left.VolumeSerialNumber != right.VolumeSerialNumber || left.FileIndexHigh != right.FileIndexHigh || left.FileIndexLow != right.FileIndexLow {
		return ErrNodeIdentityChanged
	}
	for _, info := range []windows.ByHandleFileInformation{left, right} {
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || dir != (info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
			return fmt.Errorf("privatefs: unsafe node identity during permission repair")
		}
		if !dir && info.NumberOfLinks != 1 {
			return fmt.Errorf("privatefs: unsafe file identity")
		}
	}
	return nil
}
