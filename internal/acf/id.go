package acf

import "github.com/google/uuid"

// NewID returns a UUIDv7 string. Time-ordered, lexicographically sortable.
// This UUIDv7 identifier is stable for the
// artifact's lifetime — never reissued, never reused.
func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		// The underlying source is crypto/rand; an error here means the OS
		// RNG is broken, in which case the daemon cannot safely operate.
		panic("uuid: NewV7 failed: " + err.Error())
	}
	return id.String()
}
