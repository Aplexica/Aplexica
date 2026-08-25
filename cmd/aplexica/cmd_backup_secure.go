package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/audit"
	"github.com/aplexica/aplexica/internal/filelock"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/privatefs"
	"github.com/aplexica/aplexica/internal/project"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var backupUnsigned bool
var backupPassphraseStdin bool

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.n += int64(n)
	return n, err
}

type backupPairJournal struct {
	Version                                                           int `json:"version"`
	DataFinal, SigFinal, DataTemp, SigTemp, DataRollback, SigRollback string
	HadData, HadSig                                                   bool
	DataDigest, SigDigest                                             string
	AuditTransactionID                                                string `json:"auditTransactionId"`
	Phase                                                             string `json:"phase"`
	Sequence                                                          uint64 `json:"sequence"`
	Checksum                                                          string `json:"checksum"`
}

const backupPairJournalDomain = "aplexica/backup-pair-journal/v2\x00"

func checksumBackupJournal(j backupPairJournal) (string, error) {
	j.Checksum = ""
	b, err := json.Marshal(j)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte(backupPairJournalDomain), b...))
	return hex.EncodeToString(digest[:]), nil
}

func validateBackupJournal(j backupPairJournal) error {
	if j.Version != 2 || (j.Phase != "prepared" && j.Phase != "state-committed" && j.Phase != "audit-committed") || j.Sequence == 0 || j.AuditTransactionID == "" {
		return fmt.Errorf("backup: invalid recovery journal")
	}
	want, err := checksumBackupJournal(j)
	if err != nil || want != j.Checksum {
		return fmt.Errorf("backup: recovery journal checksum mismatch")
	}
	return nil
}

func regularExists(root *privatefs.Root, rel string) (bool, error) {
	f, err := root.OpenReadRegularIntegrity(rel)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, f.Close()
}

func recoverBackupPair(root *privatefs.Root, journalName string, recorder audit.Recorder) error {
	f, err := root.OpenReadRegular(journalName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j backupPairJournal
	dec := json.NewDecoder(io.LimitReader(f, 64<<10))
	dec.DisallowUnknownFields()
	err = dec.Decode(&j)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("backup: invalid recovery journal")
	}
	if err := validateBackupJournal(j); err != nil {
		return err
	}
	if j.Phase == "state-committed" || j.Phase == "audit-committed" {
		for rel, want := range map[string]string{j.DataFinal: j.DataDigest, j.SigFinal: j.SigDigest} {
			if rel == "" || want == "" {
				continue
			}
			f, e := root.OpenReadRegular(rel)
			if e != nil {
				return fmt.Errorf("backup: committed pair incomplete: %w", e)
			}
			h := sha256.New()
			_, e = io.Copy(h, f)
			_ = f.Close()
			if e != nil || hex.EncodeToString(h.Sum(nil)) != want {
				return fmt.Errorf("backup: committed pair digest mismatch")
			}
		}
	} else {
		if j.HadData {
			if ok, _ := regularExists(root, j.DataFinal); ok {
				_ = root.RemoveRegular(j.DataFinal)
			}
			if ok, _ := regularExists(root, j.DataRollback); ok {
				if err := root.Rename(j.DataRollback, j.DataFinal); err != nil {
					return err
				}
			}
		} else if ok, _ := regularExists(root, j.DataFinal); ok {
			_ = root.RemoveRegular(j.DataFinal)
		}
		if j.HadSig {
			if ok, _ := regularExists(root, j.SigFinal); ok {
				_ = root.RemoveRegular(j.SigFinal)
			}
			if ok, _ := regularExists(root, j.SigRollback); ok {
				if err := root.Rename(j.SigRollback, j.SigFinal); err != nil {
					return err
				}
			}
		} else if j.SigFinal != "" {
			if ok, _ := regularExists(root, j.SigFinal); ok {
				_ = root.RemoveRegular(j.SigFinal)
			}
		}
	}
	if j.Phase != "audit-committed" {
		if recorder == nil {
			return fmt.Errorf("backup: audit recorder unavailable during recovery")
		}
		outcome := "rolled-back"
		if j.Phase == "state-committed" {
			outcome = "success"
		}
		if err := recorder.CompleteTransaction(context.Background(), j.AuditTransactionID, outcome); err != nil {
			return err
		}
		j.Phase = "audit-committed"
		j.Sequence++
		if err := writeBackupJournal(root, journalName, j); err != nil {
			return err
		}
	}
	for _, rel := range []string{j.DataTemp, j.SigTemp, j.DataRollback, j.SigRollback} {
		if rel != "" {
			if ok, _ := regularExists(root, rel); ok {
				_ = root.RemoveRegular(rel)
			}
		}
	}
	return root.RemoveRegular(journalName)
}

func writeBackupJournal(root *privatefs.Root, name string, j backupPairJournal) error {
	checksum, err := checksumBackupJournal(j)
	if err != nil {
		return err
	}
	j.Checksum = checksum
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := root.WriteFile(name, b, privatefs.FilePolicy{RejectWritableByOthers: true}); err != nil {
		return err
	}
	return root.SyncDir(".")
}

func secureBackupRunE(cmd *cobra.Command, args []string) error {
	if backupAnonymizeDryRun {
		return runAnonymizeDryRun(cmd, backupStoreRoot)
	}
	if len(args) != 1 {
		return fmt.Errorf("backup requires exactly one bundle-path argument")
	}
	if backupUnsigned && backupSign {
		return fmt.Errorf("--unsigned and --sign are mutually exclusive")
	}
	if backupPassphraseEnvVar != "" {
		return fmt.Errorf("--passphrase-from-env is disabled because environment variables expose secrets; use --passphrase-stdin")
	}
	if backupEncrypt && (backupRecipientPath == "") == !backupPassphraseStdin {
		return fmt.Errorf("--encrypt requires exactly one of --recipient or --passphrase-stdin")
	}
	if !backupEncrypt && (backupRecipientPath != "" || backupPassphraseStdin) {
		return fmt.Errorf("--recipient/--passphrase-stdin require --encrypt")
	}
	stateDir := backupStateDir
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".aplexica", "state")
	}
	auditRecorder := &audit.FileRecorder{Root: filepath.Join(stateDir, "audit")}
	if backupUnsigned {
		field, _ := audit.SafeID("command", "backup")
		if err := auditRecorder.Record(cmd.Context(), audit.Event{Code: "bundle.unsigned_acknowledged", Outcome: "success", Fields: []audit.Field{field}}); err != nil {
			return fmt.Errorf("backup: record unsigned acknowledgement: %w", err)
		}
	}

	var recipients []age.Recipient
	if backupRecipientPath != "" {
		r, err := acf.LoadAgeRecipient(backupRecipientPath)
		if err != nil {
			return err
		}
		recipients = []age.Recipient{r}
	}
	if backupPassphraseStdin {
		if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
			return fmt.Errorf("--passphrase-stdin refuses a terminal; pipe the secret on stdin")
		}
		b, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 513))
		if err != nil {
			return err
		}
		if len(b) == 0 || len(b) > 512 {
			return fmt.Errorf("backup passphrase must be 1..512 bytes")
		}
		pass := strings.TrimSuffix(string(b), "\n")
		for i := range b {
			b[i] = 0
		}
		r, err := age.NewScryptRecipient(pass)
		pass = ""
		if err != nil {
			return err
		}
		recipients = []age.Recipient{r}
	}

	sign := !backupUnsigned
	var signingKey ed25519.PrivateKey
	var signingKeyID [32]byte
	if sign {
		keyPath := backupKeyPath
		if keyPath == "" {
			home, _ := os.UserHomeDir()
			keyPath = filepath.Join(home, ".aplexica", "keys", "backup-signing.ed25519")
		}
		var err error
		signingKey, _, signingKeyID, err = acf.LoadOrCreateBackupSigningKey(keyPath)
		if err != nil {
			return fmt.Errorf("backup signing key: %w", err)
		}
	}

	opts := acf.BundleOpts{AplexicaVersion: aplexicaCLIVersion, RespectSyncFlags: backupRespectSyncFlags, IncludePending: backupIncludePendingProjects}
	if backupIncludeSecrets {
		opts.SecretsRoot = backupSecretsRoot
	}
	if backupScope != "" {
		opts.ScopeFilter = acf.Scope(backupScope)
	}
	if len(backupProjects) > 0 {
		opts.ProjectFilter = backupProjects
	}
	if !backupIncludePendingProjects {
		home, _ := os.UserHomeDir()
		stateDir := backupStateDir
		if stateDir == "" {
			stateDir = filepath.Join(home, ".aplexica", "state")
		}
		reg, err := project.NewRegistry(filepath.Join(stateDir, "projects.json"))
		if err != nil {
			return fmt.Errorf("backup: project registry: %w", err)
		}
		list, err := pending.List(&acf.Store{Root: backupStoreRoot}, reg)
		if err != nil {
			return err
		}
		opts.PendingIDs = map[string]struct{}{}
		for _, p := range list {
			opts.PendingIDs[p.ID] = struct{}{}
		}
	}
	if backupAnonymize {
		opts.Anonymize = true
		home, _ := os.UserHomeDir()
		opts.AnonymizeHomeDir = home
		if opts.SecretsRoot != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: --anonymize disables --include-secrets")
			opts.SecretsRoot = ""
		}
	}

	dest, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	parent, dataFinal := filepath.Dir(dest), filepath.Base(dest)
	if err := privatefs.EnsureDir(parent, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly}); err != nil {
		return fmt.Errorf("backup destination parent: %w", err)
	}
	root, err := privatefs.OpenRoot(parent, privatefs.DirPolicy{Access: privatefs.AccessIntegrityOnly})
	if err != nil {
		return err
	}
	defer root.Close()
	journalID := sha256.Sum256([]byte(dataFinal))
	journalName := ".aplexica-backup-journal-" + hex.EncodeToString(journalID[:8]) + ".json"
	lock, err := filelock.Acquire(filepath.Join(parent, journalName+".lock"), 30*time.Second)
	if err != nil {
		return fmt.Errorf("backup destination is busy: %w", err)
	}
	defer lock.Close()
	if err := recoverBackupPair(root, journalName, auditRecorder); err != nil {
		return err
	}
	hadData, err := regularExists(root, dataFinal)
	if err != nil {
		return fmt.Errorf("backup destination: %w", err)
	}
	sigFinal := dataFinal + ".sig"
	hadSig, err := regularExists(root, sigFinal)
	if err != nil {
		return fmt.Errorf("backup signature destination: %w", err)
	}

	out, dataTemp, err := root.CreateTemp(".", ".aplexica-backup-data-")
	if err != nil {
		return err
	}
	cleanup := func() {
		if ok, _ := regularExists(root, dataTemp); ok {
			_ = root.RemoveRegular(dataTemp)
		}
	}
	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(out, h)}
	var bundleWriter io.Writer = cw
	var ageWriter io.WriteCloser
	if backupEncrypt {
		ageWriter, err = age.Encrypt(cw, recipients...)
		if err != nil {
			_ = out.Close()
			cleanup()
			return err
		}
		bundleWriter = ageWriter
	}
	err = (&acf.Store{Root: backupStoreRoot}).Bundle(bundleWriter, opts)
	if err == nil && ageWriter != nil {
		err = ageWriter.Close()
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return err
	}
	var dataDigest [32]byte
	copy(dataDigest[:], h.Sum(nil))

	sigTemp := ""
	var sigDigest [32]byte
	if sign {
		sig, err := acf.SignDigest(signingKey, dataDigest)
		if err != nil {
			cleanup()
			return err
		}
		f, rel, err := root.CreateTemp(".", ".aplexica-backup-sig-")
		if err != nil {
			cleanup()
			return err
		}
		sigTemp = rel
		if _, err = f.Write(sig); err == nil {
			err = f.Sync()
		}
		if ce := f.Close(); err == nil {
			err = ce
		}
		if err != nil {
			cleanup()
			_ = root.RemoveRegular(sigTemp)
			return err
		}
		sigDigest = sha256.Sum256(sig)
	}
	sigDigestHex := ""
	if sign {
		sigDigestHex = hex.EncodeToString(sigDigest[:])
	}
	auditTxnID := acf.NewID()
	if err := auditRecorder.BeginTransaction(cmd.Context(), auditTxnID, audit.Event{Code: "backup.created", Fields: []audit.Field{audit.Count("bytes", uint64(cw.n))}}); err != nil {
		cleanup()
		if sigTemp != "" {
			_ = root.RemoveRegular(sigTemp)
		}
		return fmt.Errorf("backup: persist audit intent: %w", err)
	}
	j := backupPairJournal{Version: 2, DataFinal: dataFinal, SigFinal: sigFinal, DataTemp: dataTemp, SigTemp: sigTemp, DataRollback: ".aplexica-backup-old-data-" + randomLocalID(), SigRollback: ".aplexica-backup-old-sig-" + randomLocalID(), HadData: hadData, HadSig: hadSig, DataDigest: hex.EncodeToString(dataDigest[:]), SigDigest: sigDigestHex, AuditTransactionID: auditTxnID, Phase: "prepared", Sequence: 1}
	if err := writeBackupJournal(root, journalName, j); err != nil {
		cleanup()
		if sigTemp != "" {
			_ = root.RemoveRegular(sigTemp)
		}
		_ = auditRecorder.CompleteTransaction(cmd.Context(), auditTxnID, "failed")
		return err
	}
	rollback := func() { _ = recoverBackupPair(root, journalName, auditRecorder) }
	if hadData {
		if err := root.Rename(dataFinal, j.DataRollback); err != nil {
			rollback()
			return err
		}
	}
	if hadSig {
		if err := root.Rename(sigFinal, j.SigRollback); err != nil {
			rollback()
			return err
		}
	}
	if err := root.Rename(dataTemp, dataFinal); err != nil {
		rollback()
		return err
	}
	if sign {
		if err := root.Rename(sigTemp, sigFinal); err != nil {
			rollback()
			return err
		}
	} else if hadSig { /* old signature remains rollback-only until commit cleanup */
	}
	j.Phase = "state-committed"
	j.Sequence++
	if err := writeBackupJournal(root, journalName, j); err != nil {
		return err
	}
	if err := recoverBackupPair(root, journalName, auditRecorder); err != nil {
		return fmt.Errorf("backup: finalize committed pair: %w", err)
	}

	if backupJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"bundlePath": dest, "bytes": cw.n, "includesSecrets": backupIncludeSecrets && !backupAnonymize, "anonymized": backupAnonymize, "signaturePath": func() string {
			if sign {
				return dest + ".sig"
			}
			return ""
		}(), "encrypted": backupEncrypt, "signingKeyId": func() string {
			if sign {
				return hex.EncodeToString(signingKeyID[:])
			}
			return ""
		}()})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "wrote %s (%d bytes)\n", dest, cw.n)
	if sign {
		fmt.Fprintf(cmd.OutOrStdout(), "wrote signature %s (key %s)\n", dest+".sig", hex.EncodeToString(signingKeyID[:]))
	} else {
		fmt.Fprintln(cmd.ErrOrStderr(), "warning: unsigned backup explicitly requested")
	}
	return nil
}

func randomLocalID() string {
	var b [8]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func init() {
	backupCmd.Flags().BoolVar(&backupUnsigned, "unsigned", false, "Explicitly create an unsigned backup")
	backupCmd.Flags().BoolVar(&backupPassphraseStdin, "passphrase-stdin", false, "Read an age scrypt passphrase from non-terminal stdin")
}
