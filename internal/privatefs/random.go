package privatefs

import (
	"crypto/rand"
	"encoding/hex"
)

func randomSuffix() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
