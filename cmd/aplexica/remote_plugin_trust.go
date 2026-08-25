package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
)

// remotePluginPublisherPublicKeyHex is the original publisher root retained for
// the transition overlap. Public-key SHA-256 fingerprint:
// 1a2ed90a1da3fc6888c5a1944076a83c91c7a3e8d835b243d01bb6779bb33897.
const remotePluginPublisherPublicKeyHex = "06f7f8f533b98ab7b29a8c64c483ffee88dd38aa1bf634a23961b4ab58d37cde"

// providerNeutralRemotePluginPublisherPublicKeyHex is the remote-plugin
// publisher root introduced for the 2026-07 rotation. It authorizes plugin
// manifests only — it has never signed an Aplexica release, and since
// 2026-08-06 no release is signed by any offline key. Its private key is never
// present in this repository or daemon. Public-key SHA-256 fingerprint:
// ddcaa7baac5957f32d38857a6e551a810975a2e3b3f3b71410b04ebc0174b80f.
const providerNeutralRemotePluginPublisherPublicKeyHex = "77a28c94c159c6c510882cb28549aed18ca315a5ee9de3a8d4b2e445eeb9d2bb"

// balancedDesktopReleasePublisherKeySHA256Hex is the SHA-256 of the canonical
// OpenSSH public-key blob in the root-sealed Balanced generation-1 trust
// bundle. Balanced plugin authorization is carried by the finite exact-byte
// table compiled into this daemon, not by a separate hosted or Keychain
// publisher. The table is only as trustworthy as the binary holding it, and
// that binary's provenance now comes from the release train: the archive is
// listed in a SHA256SUMS manifest carrying an AWS KMS-authorized cosign
// signature and in the release's strict public provenance statement.
// A user who verified their download therefore knows exactly which table they
// are running. Nothing here reads that manifest — the InventorySHA256 values
// below are locally computed CBOR digests of the plugin artifacts and are
// unrelated to any release manifest.
const balancedDesktopReleasePublisherKeySHA256Hex = "7f58bed2a1d14d9049ec50b6a98fb9e564d0621630d60b48a24121fa68c1e6c5"

const maxCompiledRemotePluginPublisherKeys = 8

// compiledRemotePluginPublisherKey describes one publisher root shipped in the
// daemon. The ring deliberately lives in source rather than in runtime config:
// a compromised plugin, relay, repository mirror, or local config file must not
// be able to add a signing authority. Rotation is an overlap release that ships
// both old and new public keys, followed by a later release that retires the old
// key after every supported plugin has moved to the new signer.
type compiledRemotePluginPublisherKey struct {
	Name         string
	PublicKeyHex string
}

var compiledRemotePluginPublisherKeyRing = []compiledRemotePluginPublisherKey{
	{Name: "legacy-publisher-root-2026-07", PublicKeyHex: remotePluginPublisherPublicKeyHex},
	{Name: "provider-neutral-publisher-root-2026-07", PublicKeyHex: providerNeutralRemotePluginPublisherPublicKeyHex},
}

// remotePluginLegacyOverlapEnabled remains true only for the finite Balanced
// transition release. A later explicit retirement commit sets this to false
// and removes the table, so even an already-checkpointed v1 artifact stops
// being executable.
const remotePluginLegacyOverlapEnabled = true

const (
	balancedRemotePluginSequenceV014  = 3
	balancedRemotePluginSequenceV015  = 4
	balancedRemotePluginSequenceV016  = 5
	balancedRemotePluginSequenceV017  = 6
	balancedRemotePluginSequenceV018  = 7
	balancedRemotePluginSequenceV019  = 8
	balancedRemotePluginSequenceV0110 = 9
	balancedRemotePluginSequenceV0111 = 10
	balancedRemotePluginSequenceV0112 = 11
	balancedRemotePluginSequenceV0113 = 12
	balancedRemotePluginSequenceV0114 = 13
)

// These are the exact six immutable v0.1.1 overlap targets.  A publisher-valid
// but different v1 manifest is not migration authority.  v2 releases no longer
// use this table: their signed inventory and durable sequence checkpoint are
// mandatory instead.
var remotePluginLegacyOverlapArtifacts = []truststate.LegacyIdentity{
	legacyRemotePlugin("darwin", "amd64", "6348ab00f3a60c319e83539bca9c9f62fe7fe4968e56d671af797d2e701fb270", "2ecaa972f23d989a4eb353421c409af6d3a07f03ce5c8e997c193eb6d94c7942"),
	legacyRemotePlugin("darwin", "arm64", "6f59f982cac737543e4fadbcb279b92f648b8f2061b57d96ada7637596eeffc2", "cacf6fed7c5b50d7ef9dce781a3146143279bc4a641c1cce7a28af113e9ea1e4"),
	legacyRemotePlugin("linux", "amd64", "aed359f4e2d84eb40cbaa40d909e2e25f7a32ba9eaefc4eef46130f7963c8e6a", "121b05a844c197963921385099c7612143d6a7be6a984bdb9aac7cfc5f8d27c0"),
	legacyRemotePlugin("linux", "arm64", "80b6bc3c44bd98403401f338d0a1d5b931c34f8189189c14ab2a67d76d11e0ee", "167e57a0b974f9a1203b5cdd90d32416c30e28cb885f9afef7448bebe7cb54fe"),
	legacyRemotePlugin("windows", "amd64", "a7cb5d6e3d6ea610b6ea587ed29244e5f9888e12303d34133a079a47899f5c4c", "6fa83ee6dc0e171130e96813dcd667daa0292c79442f0f613e8fe0114d61a952"),
	legacyRemotePlugin("windows", "arm64", "83aa479885dbc683ae8035e5a0f3711aae540f7aebd67b6aee986075f8d9146b", "4fb93d5d71f2f4c44fd8ca0b256af37154117bb7fc75d6738c8f4264e7cb2244"),
}

// balancedRemotePluginArtifacts are exact plugin bytes authorized by the
// matching signed Balanced desktop-suite daemon. A plugin update changes this
// table and therefore requires a new signed daemon release. Sequence 1 is the
// monotonic transition from the retained v0.1.1 publisher-manifest overlap.
var balancedRemotePluginArtifacts = []proto.BalancedRemotePluginAuthorization{
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.2", Sequence: 1, RollbackFloor: 1,
		BinarySHA256:       mustRemotePluginDigest("5c62a56622f508fd36a87dae3941a06f5f8fe0662bb8dde4f141363b61e693d5"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.2", Sequence: 1, RollbackFloor: 1,
		BinarySHA256:       mustRemotePluginDigest("513f8e49ea04835f21f9ac8e223336c8fe23e71ec14cac7cf40f304d6c0aa7ce"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.3", Sequence: 2, RollbackFloor: 2,
		Previous: &proto.RemotePluginPrevious{Sequence: 1, PluginVersion: "v0.1.2",
			InventorySHA256: mustRemotePluginDigest("59212a81548ff6d613c6487bded511424d28ecb5e60f574964094ea9d7f56965")},
		BinarySHA256:       mustRemotePluginDigest("9aa70f67af209ee98978cf3fed80f850e7e3d1ae9662cc011484de29626eebe7"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.3", Sequence: 2, RollbackFloor: 2,
		Previous: &proto.RemotePluginPrevious{Sequence: 1, PluginVersion: "v0.1.2",
			InventorySHA256: mustRemotePluginDigest("a3e240766e4fac6cda57f40a154ea3b73c4d0aff7d945495e0e46cdebf34e383")},
		BinarySHA256:       mustRemotePluginDigest("818e6feefad21eefc6ae6159425befc2c14a243f85425357b2cbbca41f92ef7a"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.4", Sequence: balancedRemotePluginSequenceV014, RollbackFloor: balancedRemotePluginSequenceV014,
		Previous: &proto.RemotePluginPrevious{Sequence: 2, PluginVersion: "v0.1.3",
			InventorySHA256: mustRemotePluginDigest("720bb0e2658c3d556d7537ff9f10639650ec081224d1b0d65c5a818d32924c02")},
		BinarySHA256:       mustRemotePluginDigest("17aa3ce6ce55d61786a9b65d8f78c3447c8bd8eca0b6973e336f5a966e410c02"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.4", Sequence: balancedRemotePluginSequenceV014, RollbackFloor: balancedRemotePluginSequenceV014,
		Previous: &proto.RemotePluginPrevious{Sequence: 2, PluginVersion: "v0.1.3",
			InventorySHA256: mustRemotePluginDigest("d0d83fde5af7ddfb65407a9af8edf9985807e6d08b23f7d0885ea270c37375cf")},
		BinarySHA256:       mustRemotePluginDigest("3ae04e86a33485929ac2f3d1b743a9eed766f008943044d7e1a736d1c9b92454"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.5", Sequence: balancedRemotePluginSequenceV015, RollbackFloor: balancedRemotePluginSequenceV015,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV014, PluginVersion: "v0.1.4",
			InventorySHA256: mustRemotePluginDigest("1cf7902416e13ac711b0416e39957e7b570ba614c4d0ea4948eef464ba342472")},
		BinarySHA256:       mustRemotePluginDigest("ae32a83d900664d68d59de109db62ac594ab2ef93eee59b8f4374242cb4645dc"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.5", Sequence: balancedRemotePluginSequenceV015, RollbackFloor: balancedRemotePluginSequenceV015,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV014, PluginVersion: "v0.1.4",
			InventorySHA256: mustRemotePluginDigest("04256d37c7534deade217c70e4fef97586069d2ea9c27d838d43cb8cf46a2676")},
		BinarySHA256:       mustRemotePluginDigest("fadd843182e9ac030bc1cf006c5425cf73a45ac349336a3faeb17a5075a0e432"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.6", Sequence: balancedRemotePluginSequenceV016, RollbackFloor: balancedRemotePluginSequenceV016,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV015, PluginVersion: "v0.1.5",
			InventorySHA256: mustRemotePluginDigest("2d6b21026b9d811c31edabc50ccdeb7fa6b51e493b14e3fc88f020d189844628")},
		BinarySHA256:       mustRemotePluginDigest("78a9af725ae7dc0c2e169908e4f5a43ee00955a3fa5d0f7810c8289c6574ee73"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.6", Sequence: balancedRemotePluginSequenceV016, RollbackFloor: balancedRemotePluginSequenceV016,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV015, PluginVersion: "v0.1.5",
			InventorySHA256: mustRemotePluginDigest("0d0cd691572aee93aa9ad72925e6996d21d167fc97de9812a8b59e20d6df2535")},
		BinarySHA256:       mustRemotePluginDigest("fddd2af4b099fa3051cb8d60c6d7d6d400eb72465934483c045b481991556026"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.7", Sequence: balancedRemotePluginSequenceV017, RollbackFloor: balancedRemotePluginSequenceV017,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV016, PluginVersion: "v0.1.6",
			InventorySHA256: mustRemotePluginDigest("5ae95b7d54bb7755a158f31c6a9b2d6d5e95de44268552a2a8e5e31c4d536e22")},
		Capabilities: []string{
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityPairStdinV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("3428911de9aaea1e5186239b916650ede65a6adfd076812397a8e659185706f8"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.7", Sequence: balancedRemotePluginSequenceV017, RollbackFloor: balancedRemotePluginSequenceV017,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV016, PluginVersion: "v0.1.6",
			InventorySHA256: mustRemotePluginDigest("634b107724342676fff63b8dcbd397cb186e84b8afc18389a88cdb46b36f5815")},
		Capabilities: []string{
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityPairStdinV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("f43de1c0985c0771208b5f7cb0582a05c2f97ff0348fb22af314f3f590ce223a"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.8", Sequence: balancedRemotePluginSequenceV018, RollbackFloor: balancedRemotePluginSequenceV018,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV017, PluginVersion: "v0.1.7",
			InventorySHA256: mustRemotePluginDigest("cfc43b22dabd17b65bf11f31cdde95484b349b646461c24f294bfe00e9ad9c2d")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("0fcd15c1d0011ebe272174e344fbe60dee657db1dd3085dc4534df8d4c16952b"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.8", Sequence: balancedRemotePluginSequenceV018, RollbackFloor: balancedRemotePluginSequenceV018,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV017, PluginVersion: "v0.1.7",
			InventorySHA256: mustRemotePluginDigest("2cb7beaa0726e53d85d2a55cbd539c8445747245e0d8cc1ad6a890f990de145e")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("ccbb85dc7c8595bb80d0f09b3080421d47c093e66e36c24591624d5a9f51ae2a"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.9", Sequence: balancedRemotePluginSequenceV019, RollbackFloor: balancedRemotePluginSequenceV019,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV018, PluginVersion: "v0.1.8",
			InventorySHA256: mustRemotePluginDigest("7eea91fefd044c665c4c021ad8010ed4eb28a4f52ae98464297f7a0288c0d67f")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("c8f794f5e4602dc658cba24e1a61c00813e711b10b4e8529257d7b24e6729803"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.9", Sequence: balancedRemotePluginSequenceV019, RollbackFloor: balancedRemotePluginSequenceV019,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV018, PluginVersion: "v0.1.8",
			InventorySHA256: mustRemotePluginDigest("867afac762343977cf4f51132f0104318724dbf061a33366a7fc1ec41e3cf65c")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("fa4e8bbf80039d776b4df1427b8254ed2dc41e800b5a20dc7e8e6766f5387c8f"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.10", Sequence: balancedRemotePluginSequenceV0110, RollbackFloor: balancedRemotePluginSequenceV0110,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV019, PluginVersion: "v0.1.9",
			InventorySHA256: mustRemotePluginDigest("39bdd0917785a10c1f7606a7d54040de692da9fb9539f48cb41edd74b725c0a4")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("a22cf06a23b03b3d4687a12ef178a5152f0af66e9f0174b837d9f6de3d65ab87"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.10", Sequence: balancedRemotePluginSequenceV0110, RollbackFloor: balancedRemotePluginSequenceV0110,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV019, PluginVersion: "v0.1.9",
			InventorySHA256: mustRemotePluginDigest("523c573dc24780dc53d72f0779b12b8214c19804ce8bf6888e3c3be801814c3d")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("330d12131addc48e5b431dd47939100ad54fea49c9a35707e6051e623bcde15c"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.11", Sequence: balancedRemotePluginSequenceV0111, RollbackFloor: balancedRemotePluginSequenceV0111,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0110, PluginVersion: "v0.1.10",
			InventorySHA256: mustRemotePluginDigest("0c4ca296c54042ad4720254ff036e1a5bb92afd138b96fa999f1a839ed4de752")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("a0edded216312fbb55f260a988f982c52c9552da07810abde380c161a0414d7f"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.11", Sequence: balancedRemotePluginSequenceV0111, RollbackFloor: balancedRemotePluginSequenceV0111,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0110, PluginVersion: "v0.1.10",
			InventorySHA256: mustRemotePluginDigest("97aa2ca6802f88b849d5802c485a2faeb97d7eac0bd8ef5d90dece1126b515a4")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("b670d6cc0efd87508748bde798d59bb2153fd9e031d4890eb9ad9e4d64015dcf"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.12", Sequence: balancedRemotePluginSequenceV0112, RollbackFloor: balancedRemotePluginSequenceV0112,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0111, PluginVersion: "v0.1.11",
			InventorySHA256: mustRemotePluginDigest("f81c12f0c8c3474e6a2e884a2cca4a846fe4eb311bcdfba165d0703fbd979a06")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("a51d6ab9863a9df9b75eec1c458bcf3f2f26c462b28a284345a4dcf76d514c9f"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.12", Sequence: balancedRemotePluginSequenceV0112, RollbackFloor: balancedRemotePluginSequenceV0112,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0111, PluginVersion: "v0.1.11",
			InventorySHA256: mustRemotePluginDigest("f979bf84a8c0e0150c05b601e2f58e891f63aa0242833d3a760967ad9f1ee86c")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("5aaaf080d137991feb0c861c5b166f45d440df5e793c4762272d7bcf840df7e3"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.13", Sequence: balancedRemotePluginSequenceV0113, RollbackFloor: balancedRemotePluginSequenceV0113,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0112, PluginVersion: "v0.1.12",
			InventorySHA256: mustRemotePluginDigest("ddc93d919649909b522081211aa35b553b57df7912a40af39be470a71f91da13")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("223032e9b50c28642399155347b792b5767050bd9b454e7a13bc260c7286944f"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.13", Sequence: balancedRemotePluginSequenceV0113, RollbackFloor: balancedRemotePluginSequenceV0113,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0112, PluginVersion: "v0.1.12",
			InventorySHA256: mustRemotePluginDigest("369126936ad422bdaba12505cb9f022f225aab2d8c32163f086c6c46bfbac168")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("26540e6150127bb2eea45489edef96bb601012797a26abb3622ce8ec5d53cb81"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "darwin", GOARCH: "arm64", PluginVersion: "v0.1.14", Sequence: balancedRemotePluginSequenceV0114, RollbackFloor: balancedRemotePluginSequenceV0114,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0113, PluginVersion: "v0.1.13",
			InventorySHA256: mustRemotePluginDigest("a6468416dfc1da5525b6ccac4f29685c874184f5abf291a03adb5cd3990904af")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("aedc263d02e568c2d0ceec2fef0937a30fcd50c3912f044d5970347318432300"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
	{GOOS: "windows", GOARCH: "amd64", PluginVersion: "v0.1.14", Sequence: balancedRemotePluginSequenceV0114, RollbackFloor: balancedRemotePluginSequenceV0114,
		Previous: &proto.RemotePluginPrevious{Sequence: balancedRemotePluginSequenceV0113, PluginVersion: "v0.1.13",
			InventorySHA256: mustRemotePluginDigest("e64c44df410cfaa99429e1d41b0da4d44a9222296188ca014f7e7e01f009763d")},
		Capabilities: []string{
			proto.CapabilityDurableCursorResumeV1,
			proto.CapabilityDurableDeltaSyncV1,
			proto.CapabilityDurableMultiStreamV1,
			proto.CapabilityDurableSyncObservationV1,
			proto.CapabilityInboundAckV2,
			proto.CapabilityInboundFinalizeV1,
			proto.CapabilityPairStdinV1,
			proto.CapabilityRedactionSafeBatchV1,
			proto.CapabilityStagedCheckpointV1,
			proto.CapabilityTrustProtocolV1,
		},
		BinarySHA256:       mustRemotePluginDigest("75d5845cd434de18ad4211dd4fd93ba0dfa171515aa395e75f1f5fdfc1b78198"),
		PublisherKeySHA256: mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex)},
}

func legacyRemotePlugin(goos, goarch, binary, manifest string) truststate.LegacyIdentity {
	return truststate.LegacyIdentity{GOOS: goos, GOARCH: goarch, PluginVersion: "v0.1.1",
		BinarySHA256: mustRemotePluginDigest(binary), ManifestSHA256: mustRemotePluginDigest(manifest),
		PublisherKeySHA256: mustRemotePluginDigest("ddcaa7baac5957f32d38857a6e551a810975a2e3b3f3b71410b04ebc0174b80f")}
}

func mustRemotePluginDigest(value string) [32]byte {
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != sha256.Size || hex.EncodeToString(raw) != value {
		panic("invalid compiled remote plugin digest")
	}
	var digest [32]byte
	copy(digest[:], raw)
	return digest
}

func remotePluginTrustPolicy() truststate.Policy {
	return truststate.Policy{AllowLegacyV1: remotePluginLegacyOverlapEnabled,
		LegacyV1: append([]truststate.LegacyIdentity(nil), remotePluginLegacyOverlapArtifacts...),
		V2Publishers: [][32]byte{
			mustRemotePluginDigest("ddcaa7baac5957f32d38857a6e551a810975a2e3b3f3b71410b04ebc0174b80f"),
			mustRemotePluginDigest(balancedDesktopReleasePublisherKeySHA256Hex),
		}}
}

func verifyRemotePluginWithCompiledTrust(execPath string) (proto.VerifiedRemotePlugin, error) {
	providerVerified, providerErr := proto.VerifyRemotePluginDetailed(execPath, remotePluginPublisherKeys())
	if providerErr == nil {
		return providerVerified, nil
	}
	balancedVerified, balancedErr := verifyBalancedRemotePluginWithAuthorizations(execPath, balancedRemotePluginArtifacts)
	if balancedErr == nil {
		return balancedVerified, nil
	}
	return proto.VerifiedRemotePlugin{}, fmt.Errorf("provider manifest: %v; balanced authorization: %v", providerErr, balancedErr)
}

func verifyBalancedRemotePluginWithAuthorizations(execPath string, authorizations []proto.BalancedRemotePluginAuthorization) (proto.VerifiedRemotePlugin, error) {
	var lastErr error
	for _, authorization := range authorizations {
		verified, err := proto.VerifyBalancedRemotePluginDetailed(execPath, authorization)
		if err == nil {
			return verified, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no compiled Balanced plugin authorization")
	}
	return proto.VerifiedRemotePlugin{}, lastErr
}

func remotePluginPublisherKeys() []ed25519.PublicKey {
	keys, err := validateCompiledRemotePluginPublisherKeyRing(compiledRemotePluginPublisherKeyRing)
	if err != nil {
		panic(fmt.Sprintf("invalid compiled remote-plugin publisher key ring: %v", err))
	}
	return keys
}

func validateCompiledRemotePluginPublisherKeyRing(ring []compiledRemotePluginPublisherKey) ([]ed25519.PublicKey, error) {
	if len(ring) == 0 || len(ring) > maxCompiledRemotePluginPublisherKeys {
		return nil, fmt.Errorf("must contain between 1 and %d keys", maxCompiledRemotePluginPublisherKeys)
	}
	names := make(map[string]struct{}, len(ring))
	publicKeys := make(map[string]struct{}, len(ring))
	keys := make([]ed25519.PublicKey, 0, len(ring))
	for i, entry := range ring {
		if entry.Name == "" || entry.Name != strings.TrimSpace(entry.Name) {
			return nil, fmt.Errorf("entry %d has an invalid name", i)
		}
		if _, duplicate := names[entry.Name]; duplicate {
			return nil, fmt.Errorf("entry %d duplicates name %q", i, entry.Name)
		}
		names[entry.Name] = struct{}{}

		decoded, err := hex.DecodeString(entry.PublicKeyHex)
		if err != nil || len(decoded) != ed25519.PublicKeySize || hex.EncodeToString(decoded) != entry.PublicKeyHex {
			return nil, fmt.Errorf("entry %d has an invalid Ed25519 public key", i)
		}
		if bytes.Equal(decoded, make([]byte, ed25519.PublicKeySize)) {
			return nil, fmt.Errorf("entry %d has an all-zero Ed25519 public key", i)
		}
		canonical := string(decoded)
		if _, duplicate := publicKeys[canonical]; duplicate {
			return nil, fmt.Errorf("entry %d duplicates a public key", i)
		}
		publicKeys[canonical] = struct{}{}
		keys = append(keys, ed25519.PublicKey(append([]byte(nil), decoded...)))
	}
	return keys, nil
}
