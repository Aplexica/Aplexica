package identity

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/aplexica/aplexica/internal/securityerr"
)

type TransparencyLeafV1 struct {
	Version    uint16   `cbor:"version"`
	LeafType   string   `cbor:"leafType"`
	ScopeID    string   `cbor:"scopeId"`
	Epoch      uint64   `cbor:"epoch"`
	ObjectHash [32]byte `cbor:"objectHash"`
}

type SignedTreeHeadUnsignedV1 struct {
	Version       uint16   `cbor:"version"`
	LogID         [32]byte `cbor:"logId"`
	TreeSize      uint64   `cbor:"treeSize"`
	RootHash      [32]byte `cbor:"rootHash"`
	TimestampUnix int64    `cbor:"timestampUnix"`
}

type SignedTreeHeadV1 struct {
	Head      SignedTreeHeadUnsignedV1 `cbor:"head"`
	Signature [64]byte                 `cbor:"signature"`
}

type TransparencyProofV1 struct {
	Leaf             TransparencyLeafV1 `cbor:"leaf"`
	LeafIndex        uint64             `cbor:"leafIndex"`
	InclusionProof   [][32]byte         `cbor:"inclusionProof"`
	SignedTreeHead   SignedTreeHeadV1   `cbor:"signedTreeHead"`
	ConsistencyProof [][32]byte         `cbor:"consistencyProof"`
}

type RosterTransparencyBundleV1 struct {
	Manifest RosterManifestV1    `cbor:"manifest"`
	Proof    TransparencyProofV1 `cbor:"proof"`
}

type AuthorityTransparencyBundleV1 struct {
	Transition AuthorityTransitionV1 `cbor:"transition"`
	Proof      TransparencyProofV1   `cbor:"proof"`
}

type ServiceTrustConfigUnsignedV1 struct {
	Version            uint16   `cbor:"version"`
	Sequence           uint64   `cbor:"sequence"`
	ServiceOrigin      string   `cbor:"serviceOrigin"`
	TransparencyLogKey [32]byte `cbor:"transparencyLogKey"`
	PreviousConfigHash [32]byte `cbor:"previousConfigHash"`
	NotBeforeUnix      int64    `cbor:"notBeforeUnix"`
	NotAfterUnix       int64    `cbor:"notAfterUnix"`
}

type ServiceTrustConfigV1 struct {
	Config            ServiceTrustConfigUnsignedV1 `cbor:"config"`
	ReleaseRootSig    [64]byte                     `cbor:"releaseRootSig"`
	PreviousLogKeySig [64]byte                     `cbor:"previousLogKeySig"`
}

func ServiceTrustConfigHash(config ServiceTrustConfigV1) ([32]byte, error) {
	return digest("aplexica/service-trust-config-hash/v1", config)
}

func VerifyServiceTrustConfig(previous *ServiceTrustConfigV1, candidate ServiceTrustConfigV1, releaseRoot ed25519.PublicKey, now time.Time) error {
	c := candidate.Config
	if c.Version != 1 || c.Sequence == 0 || !validText(c.ServiceOrigin, 512) || !nonzero32(c.TransparencyLogKey) || c.NotBeforeUnix <= 0 || c.NotAfterUnix <= c.NotBeforeUnix || now.Unix() < c.NotBeforeUnix || now.Unix() > c.NotAfterUnix || c.NotBeforeUnix > now.Add(5*time.Minute).Unix() {
		return fmt.Errorf("%w: invalid service trust configuration", securityerr.ErrUntrustedRoster)
	}
	if err := verifySig(releaseRoot, "aplexica/service-trust-config/v1", c, candidate.ReleaseRootSig[:]); err != nil {
		return err
	}
	if previous == nil {
		if c.Sequence != 1 || c.PreviousConfigHash != ([32]byte{}) || candidate.PreviousLogKeySig != ([64]byte{}) {
			return fmt.Errorf("%w: invalid genesis service trust configuration", securityerr.ErrUntrustedRoster)
		}
		return nil
	}
	p := previous.Config
	previousHash, err := ServiceTrustConfigHash(*previous)
	if err != nil {
		return err
	}
	if c.Sequence != p.Sequence+1 || c.PreviousConfigHash != previousHash || c.ServiceOrigin != p.ServiceOrigin {
		return fmt.Errorf("%w: service trust rollback or gap", securityerr.ErrUntrustedRoster)
	}
	if err := verifySig(p.TransparencyLogKey[:], "aplexica/service-trust-key-transition/v1", c, candidate.PreviousLogKeySig[:]); err != nil {
		return err
	}
	return nil
}

func TransparencyLeafHash(leaf TransparencyLeafV1) ([32]byte, error) {
	if leaf.Version != 1 || (leaf.LeafType != "roster" && leaf.LeafType != "authority-transition") || !validText(leaf.ScopeID, 256) || leaf.Epoch == 0 || !nonzero32(leaf.ObjectHash) {
		return [32]byte{}, fmt.Errorf("identity: invalid transparency leaf")
	}
	b, err := enc.Marshal(leaf)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(append([]byte{0}, b...)), nil
}

func transparencyNode(left, right [32]byte) [32]byte {
	b := make([]byte, 1, 65)
	b[0] = 1
	b = append(b, left[:]...)
	b = append(b, right[:]...)
	return sha256.Sum256(b)
}

func SignedTreeHeadDigest(head SignedTreeHeadV1) ([32]byte, error) {
	return digest("aplexica/transparency-tree-head-digest/v1", head)
}

func verifyInclusion(leaf [32]byte, index, size uint64, proof [][32]byte, root [32]byte) bool {
	if size == 0 || index >= size || len(proof) > 64 {
		return false
	}
	fn, sn, hash := index, size-1, leaf
	for _, sibling := range proof {
		if fn&1 == 1 || fn == sn {
			hash = transparencyNode(sibling, hash)
			for fn != 0 && fn&1 == 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			hash = transparencyNode(hash, sibling)
		}
		fn >>= 1
		sn >>= 1
	}
	return fn == 0 && sn == 0 && hash == root
}

func verifyConsistency(oldSize, newSize uint64, oldRoot, newRoot [32]byte, proof [][32]byte) bool {
	if oldSize == 0 {
		return len(proof) == 0
	}
	if oldSize > newSize || len(proof) > 64 {
		return false
	}
	if oldSize == newSize {
		return len(proof) == 0 && oldRoot == newRoot
	}
	fn, sn := oldSize-1, newSize-1
	for fn&1 == 1 {
		fn >>= 1
		sn >>= 1
	}
	var first, second [32]byte
	proofIndex := 0
	if fn == 0 {
		first, second = oldRoot, oldRoot
	} else {
		if len(proof) == 0 {
			return false
		}
		first, second = proof[0], proof[0]
		proofIndex = 1
	}
	for ; proofIndex < len(proof); proofIndex++ {
		if sn == 0 {
			return false
		}
		sibling := proof[proofIndex]
		if fn&1 == 1 || fn == sn {
			first = transparencyNode(sibling, first)
			second = transparencyNode(sibling, second)
			for fn != 0 && fn&1 == 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			second = transparencyNode(second, sibling)
		}
		fn >>= 1
		sn >>= 1
	}
	return sn == 0 && first == oldRoot && second == newRoot
}

func VerifyTransparencyProof(config ServiceTrustConfigV1, previous *SignedTreeHeadV1, proof TransparencyProofV1, expectedLeaf TransparencyLeafV1, now time.Time) error {
	if proof.Leaf != expectedLeaf || len(proof.InclusionProof) > 64 || len(proof.ConsistencyProof) > 64 {
		return fmt.Errorf("%w: transparency object mismatch", securityerr.ErrMetadataMismatch)
	}
	head := proof.SignedTreeHead.Head
	logID := sha256.Sum256(config.Config.TransparencyLogKey[:])
	if head.Version != 1 || head.LogID != logID || head.TreeSize == 0 || proof.LeafIndex >= head.TreeSize || head.TimestampUnix > now.Add(5*time.Minute).Unix() {
		return fmt.Errorf("%w: invalid transparency head", securityerr.ErrUntrustedRoster)
	}
	if err := verifySig(config.Config.TransparencyLogKey[:], "aplexica/transparency-tree-head/v1", head, proof.SignedTreeHead.Signature[:]); err != nil {
		return err
	}
	leafHash, err := TransparencyLeafHash(proof.Leaf)
	if err != nil {
		return err
	}
	if !verifyInclusion(leafHash, proof.LeafIndex, head.TreeSize, proof.InclusionProof, head.RootHash) {
		return fmt.Errorf("%w: transparency inclusion failed", securityerr.ErrUntrustedRoster)
	}
	if previous != nil {
		p := previous.Head
		if p.LogID != head.LogID || head.TreeSize < p.TreeSize || head.TimestampUnix < p.TimestampUnix {
			return fmt.Errorf("%w: transparency rollback", securityerr.ErrUntrustedRoster)
		}
		if head.TreeSize == p.TreeSize && head.RootHash != p.RootHash {
			return fmt.Errorf("%w: transparency equivocation", securityerr.ErrUntrustedRoster)
		}
		if !verifyConsistency(p.TreeSize, head.TreeSize, p.RootHash, head.RootHash, proof.ConsistencyProof) {
			return fmt.Errorf("%w: transparency consistency failed", securityerr.ErrUntrustedRoster)
		}
	} else if len(proof.ConsistencyProof) != 0 {
		return fmt.Errorf("%w: unexpected genesis consistency proof", securityerr.ErrUntrustedRoster)
	}
	return nil
}
