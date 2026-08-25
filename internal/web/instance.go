package web

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"strings"
)

type Instance struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
}

func NewInstance() (Instance, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Instance{}, fmt.Errorf("web: generate runtime origin: %w", err)
	}
	id := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
	return Instance{ID: id, Hostname: "aplexica-" + id + ".localhost"}, nil
}
