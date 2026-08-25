package privatefs

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

type auditCompletion struct {
	id, outcome string
}

func (a *auditCompletion) CompleteTransaction(_ context.Context, id, outcome string) error {
	if a.id != "" && (a.id != id || a.outcome != outcome) {
		return errors.New("conflicting audit completion")
	}
	a.id, a.outcome = id, outcome
	return nil
}

func testJournalRoot(t *testing.T) *Root {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "root")
	root, err := OpenRoot(dir, DirPolicy{Access: AccessPrivate, AllowExisting: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, root.Close()) })
	return root
}

func TestRecoverApplyingJournalRollsBack(t *testing.T) {
	root := testJournalRoot(t)
	content := []byte("new")
	require.NoError(t, root.WriteFile("temp", content, FilePolicy{RejectWritableByOthers: true}))
	txn := "0197f30a-3c58-7000-8000-000000000011"
	auditID := "0197f30a-3c58-7000-8000-000000000012"
	j, err := BeginJournal(root, JournalPlan{Kind: "native-restore", TransactionID: txn, AuditTransactionID: auditID, Entries: []JournalEntry{{
		RootID: "target", ObjectKind: "file", Operation: "install", FinalRel: "final", TempRel: "temp", ExpectedFinalSHA256: sha256.Sum256(content),
	}}})
	require.NoError(t, err)
	require.NoError(t, root.Rename("temp", "final"))
	require.NoError(t, j.MarkApplied(0))
	audit := &auditCompletion{}
	require.NoError(t, RecoverJournals(context.Background(), root, map[string]*Root{"target": root}, "native-restore", audit))
	_, err = root.OpenReadRegular("final")
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, "rolled-back", audit.outcome)
}

func TestRecoverCommittedJournalRollsForwardOnce(t *testing.T) {
	root := testJournalRoot(t)
	content := []byte("new")
	require.NoError(t, root.WriteFile("temp", content, FilePolicy{RejectWritableByOthers: true}))
	txn := "0197f30a-3c58-7000-8000-000000000021"
	auditID := "0197f30a-3c58-7000-8000-000000000022"
	j, err := BeginJournal(root, JournalPlan{Kind: "native-restore", TransactionID: txn, AuditTransactionID: auditID, Entries: []JournalEntry{{
		RootID: "target", ObjectKind: "file", Operation: "install", FinalRel: "final", TempRel: "temp", ExpectedFinalSHA256: sha256.Sum256(content),
	}}})
	require.NoError(t, err)
	require.NoError(t, root.Rename("temp", "final"))
	require.NoError(t, j.MarkApplied(0))
	require.NoError(t, j.MarkStateCommitted())
	audit := &auditCompletion{}
	require.NoError(t, RecoverJournals(context.Background(), root, map[string]*Root{"target": root}, "native-restore", audit))
	f, err := root.OpenReadRegular("final")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.Equal(t, "success", audit.outcome)
	require.NoError(t, RecoverJournals(context.Background(), root, map[string]*Root{"target": root}, "native-restore", audit))
}

func TestJournalNativePathsAreExplicitlyScoped(t *testing.T) {
	content := sha256.Sum256([]byte("new"))
	plan := JournalPlan{Kind: "native-restore", TransactionID: "0197f30a-3c58-7000-8000-000000000031", Entries: []JournalEntry{{
		RootID: "target", ObjectKind: "file", Operation: "install",
		FinalRel: "rollout-18:16:48.jsonl", TempRel: "temp", ExpectedFinalSHA256: content,
	}}}
	require.Error(t, validateJournalPlan(plan), "ordinary journals retain portable component rules")
	plan.NativePaths = true
	if runtime.GOOS == "windows" {
		require.Error(t, validateJournalPlan(plan), "Windows native paths must reject ADS/volume colons")
		return
	}
	require.NoError(t, validateJournalPlan(plan))
}

func TestRecoverNativePathJournalWithColon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("colon is not a legal Windows filename component")
	}
	for _, committed := range []bool{false, true} {
		name := "applying"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			control, err := OpenRoot(filepath.Join(base, "control"), DirPolicy{Access: AccessPrivate, AllowExisting: true})
			require.NoError(t, err)
			defer control.Close()
			target, err := OpenNativeRoot(filepath.Join(base, "target"), DirPolicy{Access: AccessPrivate, AllowExisting: true})
			require.NoError(t, err)
			defer target.Close()
			content := []byte("conversation")
			require.NoError(t, target.WriteFile("temp", content, FilePolicy{RejectWritableByOthers: true}))
			plan := JournalPlan{Kind: "native-restore", TransactionID: "0197f30a-3c58-7000-8000-000000000041", NativePaths: true, Entries: []JournalEntry{{
				RootID: "target", ObjectKind: "file", Operation: "install",
				FinalRel: "rollout-18:16:48.jsonl", TempRel: "temp", ExpectedFinalSHA256: sha256.Sum256(content),
			}}}
			journal, err := BeginJournal(control, plan)
			require.NoError(t, err)
			require.NoError(t, target.Rename("temp", plan.Entries[0].FinalRel))
			require.NoError(t, journal.MarkApplied(0))
			if committed {
				require.NoError(t, journal.MarkStateCommitted())
			}
			require.NoError(t, RecoverJournals(context.Background(), control, map[string]*Root{"target": target}, "native-restore", nil))
			opened, err := target.OpenReadRegular(plan.Entries[0].FinalRel)
			if committed {
				require.NoError(t, err)
				require.NoError(t, opened.Close())
			} else {
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}
