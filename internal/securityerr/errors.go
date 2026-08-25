// Package securityerr defines stable, classifiable security failures shared by
// trust-boundary packages. Callers may wrap these values with safe context, but
// must not include attacker-controlled raw input or secret material.
package securityerr

import "errors"

var (
	ErrUntrustedRoster      = errors.New("security: untrusted roster")
	ErrStaleRoster          = errors.New("security: stale roster")
	ErrInvalidSignature     = errors.New("security: invalid signature")
	ErrMetadataMismatch     = errors.New("security: metadata mismatch")
	ErrUnsafeIdentifier     = errors.New("security: unsafe identifier")
	ErrPathEscape           = errors.New("security: path escapes root")
	ErrLimitExceeded        = errors.New("security: resource limit exceeded")
	ErrUnsignedInput        = errors.New("security: unsigned input")
	ErrUnsafeFilesystemNode = errors.New("security: unsafe filesystem node")
)
