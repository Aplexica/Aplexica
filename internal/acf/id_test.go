package acf

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNewID_IsTimeOrdered(t *testing.T) {
	a := NewID()
	time.Sleep(2 * time.Millisecond)
	b := NewID()
	require.Less(t, a, b, "later UUIDv7 must sort greater than earlier")
}

func TestNewID_HasV7Variant(t *testing.T) {
	id := NewID()
	require.Len(t, id, 36, "UUID string is 36 chars including dashes")
	// The 15th character (index 14) of a v7 UUID is '7'.
	require.Equal(t, byte('7'), id[14], "version nibble must be 7")
}
