package generationactivation

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/aplexica/aplexica/internal/privatefs"
)

const endorsementJournalVersion uint16 = 1

type endorsementRecord struct {
	Version      uint16                         `json:"version"`
	Binding      [32]byte                       `json:"binding"`
	Unsigned     GenerationActivationUnsignedV1 `json:"unsigned"`
	Endorsements []ActivationEndorsementV1      `json:"endorsements"`
	Checksum     [32]byte                       `json:"checksum"`
}

// FileEndorsementJournal preserves the exact nonce/freshness proposal across
// crashes while remote authority signatures are collected.
type FileEndorsementJournal struct{ Path string }

func endorsementChecksum(record endorsementRecord) ([32]byte, error) {
	record.Checksum = [32]byte{}
	raw, err := json.Marshal(record)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte("aplexica/generation-activation-endorsement-journal/v1\x00"), raw...)), nil
}

func (f FileEndorsementJournal) root() (*privatefs.Root, string, error) {
	absolute, err := filepath.Abs(f.Path)
	if err != nil || !filepath.IsAbs(f.Path) {
		return nil, "", ErrInvalidState
	}
	root, err := privatefs.OpenRoot(filepath.Dir(absolute), privatefs.DirPolicy{Access: privatefs.AccessPrivate, RepairOwned: true, AllowExisting: true})
	return root, filepath.Base(absolute), err
}

func (f FileEndorsementJournal) Load(binding [32]byte) (GenerationActivationUnsignedV1, []ActivationEndorsementV1, error) {
	root, base, err := f.root()
	if err != nil {
		return GenerationActivationUnsignedV1{}, nil, err
	}
	defer root.Close()
	file, err := root.OpenReadRegular(base)
	if err != nil {
		return GenerationActivationUnsignedV1{}, nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 1<<20+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(raw) > 1<<20 {
		return GenerationActivationUnsignedV1{}, nil, ErrInvalidState
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var record endorsementRecord
	if decoder.Decode(&record) != nil || decoder.Decode(&struct{}{}) != io.EOF || record.Version != endorsementJournalVersion || record.Binding != binding {
		return GenerationActivationUnsignedV1{}, nil, ErrPendingActivation
	}
	want, err := endorsementChecksum(record)
	canonical, marshalErr := json.Marshal(record)
	if err != nil || marshalErr != nil || want != record.Checksum || !bytes.Equal(canonical, raw) {
		return GenerationActivationUnsignedV1{}, nil, ErrPendingActivation
	}
	return record.Unsigned, append([]ActivationEndorsementV1(nil), record.Endorsements...), nil
}

func (f FileEndorsementJournal) Save(binding [32]byte, unsigned GenerationActivationUnsignedV1, endorsements []ActivationEndorsementV1) error {
	root, base, err := f.root()
	if err != nil {
		return err
	}
	defer root.Close()
	if existing, existingEndorsements, loadErr := f.Load(binding); loadErr == nil {
		// An empty-signature record is only a crash marker for the tentative
		// proposal made before contacting peers.  It may be replaced exactly
		// once by the proposal elected for the round.  After the first returned
		// endorsement is durable, proposal bytes are immutable.
		if len(existingEndorsements) > 0 && existing != unsigned || len(existingEndorsements) > len(endorsements) {
			return ErrPendingActivation
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return loadErr
	}
	record := endorsementRecord{Version: endorsementJournalVersion, Binding: binding, Unsigned: unsigned, Endorsements: append([]ActivationEndorsementV1(nil), endorsements...)}
	record.Checksum, err = endorsementChecksum(record)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > 1<<20 {
		return ErrInvalidState
	}
	return root.WriteFile(base, raw, privatefs.FilePolicy{RejectWritableByOthers: true})
}

func (f FileEndorsementJournal) Remove(binding [32]byte) error {
	root, base, err := f.root()
	if err != nil {
		return err
	}
	defer root.Close()
	if _, _, err := f.Load(binding); err != nil {
		return err
	}
	if err := root.RemoveRegular(base); err != nil {
		return fmt.Errorf("generation activation: remove endorsement journal: %w", err)
	}
	return nil
}

var _ EndorsementJournal = FileEndorsementJournal{}
