//go:build windows

package privatefs

import (
	"fmt"
	"os"
)

func validateTrustedPathInfo(path string, info os.FileInfo, final bool, policy TrustedInputPolicy) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("privatefs: trusted input reparse point rejected")
	}
	// The handle-level descriptor check below is authoritative for the final.
	// Ancestors are checked by name before and after the retained-root read.
	if !final {
		return nil
	}
	return nil
}

func validateTrustedFinal(f *os.File, info os.FileInfo, policy TrustedInputPolicy) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("privatefs: trusted input is not regular")
	}
	sd, err := descriptorForFile(f)
	if err != nil {
		return err
	}
	access := AccessIntegrityOnly
	if policy.RequirePrivate {
		access = AccessPrivate
	}
	return validatePrivateDescriptor(sd, access)
}
