package adapter

import "errors"

// ErrArtifactTombstoned is returned by Export paths when the requested
// artifact's most recent event is a redaction. Callers can check with
// errors.Is and choose to skip silently, write to a .redacted sidecar,
// or surface a clear error.
var ErrArtifactTombstoned = errors.New("adapter: artifact is tombstoned (last event is a redaction)")
