package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/spf13/cobra"
)

// Per FR-01.26 the CLI MUST support `aplexica verify <bundle>` as the
// entry point for validating that:
//  1. The bundle file is a structurally well-formed ACF .tar.gz.
//  2. Every artifact JSON parses against the ACF schema (round-trip
//     through the typed Artifact reader is a strong proxy for "matches
//     the published JSON Schema" until we wire up a real validator).
//  3. Every event's content hash re-computes correctly, and the parent-
//     hash chain is intact (`VerifyChain`).
//
// Per FR-01.31, when a sibling <bundle>.sig file exists we verify the
// Ed25519 signature against --pubkey. When the .sig is absent OR the
// pubkey isn't a trusted-key, we warn instead of failing — operating on
// an unsigned bundle requires --unsigned-ok per the BRD.

var (
	verifyPubKey        string
	verifyExpectedKeyID string
	verifyUnsignedOK    bool
	verifyVerbose       bool
)

var verifyCmd = &cobra.Command{
	Use:   "verify <bundle>",
	Short: "Validate a bundle against the ACF schema and re-hash every event",
	Long: `Validate a .tar.gz bundle. Checks performed:

  1. The file is a well-formed gzipped tar.
  2. meta.json is present and the bundle version is supported.
  3. Every artifact JSON parses cleanly through the typed reader.
  4. Every event's content hash re-computes (SHA-256) and the parent
     hash chain is intact (VerifyChain).
  5. Optional: if <bundle>.sig exists, the Ed25519 signature is
     verified against --pubkey. If .sig is absent, warns unless
     --unsigned-ok is passed.

A corrupted bundle (modified after the fact, broken event chain,
tampered artifact JSON) fails with a clear error pointing at the
offending entry. Exit code is non-zero on any failure.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return runVerify(cmd, args[0])
	},
}

func runVerify(cmd *cobra.Command, bundlePath string) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	// Stage the restore into a tempdir so we don't write into the user's
	// canonical store. Restore is the strongest available structural
	// validator: it parses meta.json, version-checks, tar-extracts every
	// entry, and writes the JSON files we can then re-read through the
	// typed reader.
	stage, err := os.MkdirTemp("", "aplexica-verify-*")
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	defer os.RemoveAll(stage)

	stageStore := &acf.Store{Root: filepath.Join(stage, "store")}
	if err := stageStore.Init(); err != nil {
		return fmt.Errorf("init stage store: %w", err)
	}

	f, err := os.Open(bundlePath)
	if err != nil {
		return fmt.Errorf("open bundle: %w", err)
	}
	defer f.Close()
	if err := stageStore.Restore(f, filepath.Join(stage, "secrets")); err != nil {
		return fmt.Errorf("verify: bundle restore failed: %w", err)
	}

	// Walk every kind, re-read every artifact through the typed reader,
	// re-verify every event's chain.
	var stats verifyStats
	for _, kind := range []acf.Kind{acf.KindMemory, acf.KindSkill, acf.KindTool, acf.KindConversation} {
		arts, err := stageStore.ListArtifacts(kind)
		if err != nil {
			return fmt.Errorf("verify: list %s: %w", kind, err)
		}
		for _, art := range arts {
			stats.artifacts++
			// ReadArtifact is a strict typed parse; if it succeeds the
			// artifact JSON matches the expected shape. (A future strict
			// JSON Schema validator would slot in here.)
			if _, err := stageStore.ReadArtifact(kind, art.ArtifactID); err != nil {
				stats.failArtifacts = append(stats.failArtifacts,
					verifyFailure{Kind: kind, ID: art.ArtifactID, Reason: err.Error()})
				continue
			}

			events, err := stageStore.ReadEvents(kind, art.ArtifactID)
			if err != nil {
				stats.failArtifacts = append(stats.failArtifacts,
					verifyFailure{Kind: kind, ID: art.ArtifactID, Reason: "read events: " + err.Error()})
				continue
			}
			stats.events += len(events)
			if err := acf.VerifyChain(events); err != nil {
				stats.failChain = append(stats.failChain,
					verifyFailure{Kind: kind, ID: art.ArtifactID, Reason: err.Error()})
				continue
			}
			stats.okArtifacts++
		}
	}

	// Signature handling — FR-01.31. The .sig is a sibling file.
	sigPath := bundlePath + ".sig"
	sigData, sigErr := os.ReadFile(sigPath)
	switch {
	case errors.Is(sigErr, os.ErrNotExist):
		if !verifyUnsignedOK {
			fmt.Fprintf(errOut,
				"WARN: bundle is unsigned (%s.sig not found). Pass --unsigned-ok to acknowledge.\n",
				bundlePath)
			stats.signature = "unsigned (warning)"
		} else {
			stats.signature = "unsigned (acknowledged via --unsigned-ok)"
		}
	case sigErr != nil:
		return fmt.Errorf("verify: read signature: %w", sigErr)
	default:
		if verifyPubKey == "" {
			fmt.Fprintln(errOut,
				"WARN: .sig present but --pubkey not supplied; signature not verified.")
			stats.signature = "present but unverified (no --pubkey)"
		} else if err := acf.VerifyBundle(verifyPubKey, bundlePath, sigData); err != nil {
			stats.failSig = err.Error()
		} else {
			stats.signature = "verified"
		}
	}

	// Print the verify report.
	printVerifyReport(out, bundlePath, &stats, verifyVerbose)

	if stats.failArtifacts != nil || stats.failChain != nil || stats.failSig != "" {
		return fmt.Errorf("verify: %d artifact failure(s), %d chain failure(s); signature: %s",
			len(stats.failArtifacts), len(stats.failChain),
			stats.failSig+stats.signature)
	}
	return nil
}

type verifyStats struct {
	artifacts     int
	okArtifacts   int
	events        int
	signature     string
	failArtifacts []verifyFailure
	failChain     []verifyFailure
	failSig       string
}

type verifyFailure struct {
	Kind   acf.Kind
	ID     string
	Reason string
}

func printVerifyReport(out interface{ Write(p []byte) (int, error) }, bundlePath string, s *verifyStats, verbose bool) {
	w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintf(w, "verify: %s\n", bundlePath)
	fmt.Fprintf(w, "  artifacts:\t%d (%d ok, %d failed)\n",
		s.artifacts, s.okArtifacts, len(s.failArtifacts)+len(s.failChain))
	fmt.Fprintf(w, "  events:\t%d\n", s.events)
	fmt.Fprintf(w, "  signature:\t%s\n", verifyStr(s))
	_ = w.Flush()

	if verbose && s.okArtifacts > 0 {
		fmt.Fprintf(out, "  (all %d ok artifacts pass parse + chain verification)\n", s.okArtifacts)
	}
	for _, f := range s.failArtifacts {
		fmt.Fprintf(out, "  ! parse fail  %s %s: %s\n", f.Kind, f.ID, f.Reason)
	}
	for _, f := range s.failChain {
		fmt.Fprintf(out, "  ! chain fail  %s %s: %s\n", f.Kind, f.ID, f.Reason)
	}
}

func verifyStr(s *verifyStats) string {
	if s.failSig != "" {
		return "FAILED — " + s.failSig
	}
	if s.signature == "" {
		return "(skipped)"
	}
	return s.signature
}

func init() {
	verifyCmd.Flags().StringVar(&verifyPubKey, "pubkey", "",
		"path to the Ed25519 public key (.pub) for signature verification")
	verifyCmd.Flags().StringVar(&verifyExpectedKeyID, "key-id", "",
		"pinned full SHA-256 key ID for the trusted public key")
	verifyCmd.Flags().BoolVar(&verifyUnsignedOK, "unsigned-ok", false,
		"acknowledge that the bundle is unsigned")
	verifyCmd.Flags().BoolVar(&verifyVerbose, "verbose", false,
		"itemize passing artifacts in addition to failures")
	verifyCmd.RunE = secureVerifyRunE
	rootCmd.AddCommand(verifyCmd)
}

func secureVerifyRunE(cmd *cobra.Command, args []string) error {
	bundlePath := args[0]
	sigPath := bundlePath + ".sig"
	_, sigErr := os.Lstat(sigPath)
	hasSignature := sigErr == nil
	if sigErr != nil && !os.IsNotExist(sigErr) {
		return fmt.Errorf("verify: inspect signature: %w", sigErr)
	}
	opts := acf.RestoreOptions{UnsignedOK: verifyUnsignedOK}
	if hasSignature {
		if verifyPubKey == "" || verifyExpectedKeyID == "" {
			return fmt.Errorf("verify: signed bundle requires --pubkey and --key-id")
		}
		raw, err := hex.DecodeString(verifyExpectedKeyID)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("verify: --key-id must be exactly 64 hexadecimal characters")
		}
		copy(opts.ExpectedKeyID[:], raw)
		pub, id, err := acf.LoadTrustedPublicKey(verifyPubKey, opts.ExpectedKeyID, acf.DefaultBundleLimits())
		if err != nil {
			return err
		}
		opts.TrustedPubKey, opts.ExpectedKeyID = pub, id
	} else {
		if !verifyUnsignedOK {
			return fmt.Errorf("verify: bundle is unsigned; pass --unsigned-ok to acknowledge")
		}
		sigPath = ""
	}
	stage, err := os.MkdirTemp("", "aplexica-verify-target-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	b, err := acf.OpenValidatedBundleFile(bundlePath, sigPath,
		acf.BundleTarget{Store: &acf.Store{Root: filepath.Join(stage, "store")}, SecretsRoot: filepath.Join(stage, "secrets")}, opts)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := b.VerifySemantic(); err != nil {
		return fmt.Errorf("verify: %w", err)
	}
	artifacts, events := b.SemanticStats()
	stats := verifyStats{artifacts: artifacts, okArtifacts: artifacts, events: events}
	if hasSignature {
		stats.signature = "verified"
	} else {
		stats.signature = "unsigned (acknowledged via --unsigned-ok)"
	}
	printVerifyReport(cmd.OutOrStdout(), bundlePath, &stats, verifyVerbose)
	return nil
}
