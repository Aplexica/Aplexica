//go:build unix

package privatefs

import (
	"fmt"
	"os"
	"syscall"
)

func validateTrustedPathInfo(_ string, info os.FileInfo, final bool, policy TrustedInputPolicy) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (int(st.Uid) != os.Geteuid() && (!policy.AllowSystemOwner || st.Uid != 0)) {
		return fmt.Errorf("privatefs: trusted input owner mismatch")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("privatefs: trusted input is writable by others")
	}
	if final && st.Nlink != 1 {
		return fmt.Errorf("privatefs: trusted input hardlink rejected")
	}
	if final && policy.RequirePrivate && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("privatefs: trusted input is not private")
	}
	if final && policy.RequireExecutable && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("privatefs: trusted input is not executable")
	}
	return nil
}

func validateTrustedFinal(f *os.File, info os.FileInfo, policy TrustedInputPolicy) error {
	if err := validateTrustedPathInfo("", info, true, policy); err != nil {
		return err
	}
	return nil
}
