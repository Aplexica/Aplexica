package project

import (
	"encoding/hex"
	"fmt"
	"os"
)

// FileIdentity is a closed, platform-tagged identity for an opened project
// directory. Exactly one platform arm is populated. Paths are convenient
// labels; this identity is what prevents a path replacement from silently
// inheriting previously granted project authority.
type FileIdentity struct {
	Platform      string `json:"platform"`
	UnixDevice    uint64 `json:"unixDevice,omitempty"`
	UnixInode     uint64 `json:"unixInode,omitempty"`
	VolumeSerial  uint64 `json:"volumeSerial,omitempty"`
	WindowsFileID string `json:"windowsFileId,omitempty"`
}

func (identity FileIdentity) validate() error {
	switch identity.Platform {
	case "unix":
		if identity.UnixDevice == 0 || identity.UnixInode == 0 || identity.VolumeSerial != 0 || identity.WindowsFileID != "" {
			return fmt.Errorf("project: invalid Unix file identity")
		}
	case "windows":
		decoded, err := hex.DecodeString(identity.WindowsFileID)
		if identity.UnixDevice != 0 || identity.UnixInode != 0 || identity.VolumeSerial == 0 || err != nil || len(decoded) != 16 || identity.WindowsFileID != hex.EncodeToString(decoded) {
			return fmt.Errorf("project: invalid Windows file identity")
		}
	default:
		return fmt.Errorf("project: unknown file identity platform")
	}
	return nil
}

func measureProjectIdentity(path string, info os.FileInfo) (FileIdentity, error) {
	identity, err := platformProjectIdentity(path, info)
	if err != nil {
		return FileIdentity{}, err
	}
	if err := identity.validate(); err != nil {
		return FileIdentity{}, err
	}
	return identity, nil
}
