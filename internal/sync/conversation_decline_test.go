package syncd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
	"github.com/aplexica/aplexica/internal/adapter/claudecode"
	"github.com/stretchr/testify/require"
)

// reasonedDeclineTarget is a ConversationSessionTarget that declines with a
// typed reason, exercising the optional reporting refinement the orchestrator
// type-asserts.
type reasonedDeclineTarget struct {
	fakeConvSource
	dest   string
	reason adapter.SessionDeclineReason
}

func (d *reasonedDeclineTarget) MaterializeConversationSession(
	art acf.Artifact, head acf.Event, sourceAgent string,
) (string, bool, error) {
	path, ok, _, err := d.MaterializeConversationSessionReason(art, head, sourceAgent)
	return path, ok, err
}

func (d *reasonedDeclineTarget) MaterializeConversationSessionReason(
	art acf.Artifact, _ acf.Event, _ string,
) (string, bool, adapter.SessionDeclineReason, error) {
	return filepath.Join(d.dest, art.ArtifactID+".jsonl"), false, d.reason, nil
}

// silentDeclineTarget implements only the original interface, so the
// orchestrator must still work — and must classify the decline as unknown
// rather than inventing a reason.
type silentDeclineTarget struct {
	fakeConvSource
	dest string
}

func (d *silentDeclineTarget) MaterializeConversationSession(
	art acf.Artifact, _ acf.Event, _ string,
) (string, bool, error) {
	return filepath.Join(d.dest, art.ArtifactID+".jsonl"), false, nil
}

func declineFanOutFixture(t *testing.T, target adapter.Adapter) (*Orchestrator, string) {
	t.Helper()
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	source := &fakeConvSource{name: "codex"}
	seedConversations(t, store, "codex", 1)
	conversations, err := store.ListArtifacts(acf.KindConversation)
	require.NoError(t, err)
	require.Len(t, conversations, 1)

	orch, err := NewOrchestrator(Config{
		Dir: root, Adapters: []adapter.Adapter{source, target}, Store: store,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })
	return orch, conversations[0].ArtifactID
}

func strictConversationFanOut(t *testing.T, orch *Orchestrator, artifactID string) error {
	t.Helper()
	origin := "codex"
	var primary adapter.Adapter
	for _, ad := range orch.cfg.Adapters {
		if ad.Name() == origin {
			primary = ad
		}
	}
	return orch.fanOutWithOptions(context.Background(), primary, []string{artifactID}, orch.cfg.Dir, "", false,
		fanOutOptions{
			targets:     map[string]struct{}{"claude-code": {}},
			originAgent: &origin,
			strict:      true,
		})
}

// B1/B6: the typed cause must survive the strict funnel instead of being
// destroyed one frame above the code that built it, while every existing
// caller keeps matching on the bare sentinel.
func TestStrictFanOut_PropagatesTypedConversationDecline(t *testing.T) {
	target := &reasonedDeclineTarget{
		fakeConvSource: fakeConvSource{name: "claude-code"},
		reason:         adapter.SessionDeclineDiverged,
	}
	orch, artifactID := declineFanOutFixture(t, target)
	target.dest = filepath.Join(orch.cfg.Dir, "native")

	err := strictConversationFanOut(t, orch, artifactID)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInboundNativeMaterialization,
		"every existing caller matches the sentinel and must keep matching")
	require.False(t, deferredMaterializationWithheld(err),
		"a target-side decline is a charged failure, never a withheld pass")

	var decline *ConversationDeclineError
	require.ErrorAs(t, err, &decline)
	require.Equal(t, adapter.SessionDeclineDiverged, decline.Reason)
	require.Equal(t, "claude-code", decline.Agent)
	require.Equal(t, artifactID, decline.ArtifactID)
	require.Equal(t, ConversationRetryStructural, decline.RetryClass)
}

// Routing behavior observed end to end: an ahead native session and a diverged one
// must reach the deferral layer in different retry classes.
func TestStrictFanOut_RoutesAheadAndDivergedToDifferentRetryClasses(t *testing.T) {
	for _, tc := range []struct {
		reason adapter.SessionDeclineReason
		class  ConversationRetryClass
	}{
		{adapter.SessionDeclineNativeAhead, ConversationRetryRace},
		{adapter.SessionDeclineRace, ConversationRetryRace},
		{adapter.SessionDeclineDiverged, ConversationRetryStructural},
		{adapter.SessionDeclineForkedMirror, ConversationRetryStructural},
		{adapter.SessionDeclineGraphMalformed, ConversationRetryStructural},
	} {
		t.Run(string(tc.reason), func(t *testing.T) {
			target := &reasonedDeclineTarget{
				fakeConvSource: fakeConvSource{name: "claude-code"},
				reason:         tc.reason,
			}
			orch, artifactID := declineFanOutFixture(t, target)
			target.dest = filepath.Join(orch.cfg.Dir, "native")

			var decline *ConversationDeclineError
			require.ErrorAs(t, strictConversationFanOut(t, orch, artifactID), &decline)
			require.Equal(t, tc.class, decline.RetryClass)
		})
	}
	require.NotEqual(t, ConversationRetryStructural, ConversationRetryRace,
		"ahead and diverged must not collapse into one class")
}

// An adapter that predates the refinement must still fan out and still fail
// strictly — it simply reports an unknown class, which nothing may read as
// permission to give up.
func TestStrictFanOut_UnreportedDeclineClassifiesAsUnknown(t *testing.T) {
	target := &silentDeclineTarget{fakeConvSource: fakeConvSource{name: "claude-code"}}
	orch, artifactID := declineFanOutFixture(t, target)
	target.dest = filepath.Join(orch.cfg.Dir, "native")

	err := strictConversationFanOut(t, orch, artifactID)
	require.ErrorIs(t, err, ErrInboundNativeMaterialization)
	var decline *ConversationDeclineError
	require.ErrorAs(t, err, &decline)
	require.Equal(t, adapter.SessionDeclineUnspecified, decline.Reason)
	require.Equal(t, ConversationRetryUnknown, decline.RetryClass)
}

// A strict failure with no cause to attach must keep returning exactly the bare
// sentinel, so the wrapping introduced for typed declines cannot change the
// error surface of unrelated failures.
func TestStrictFanOut_CauselessFailureStaysBareSentinel(t *testing.T) {
	orch, _ := declineFanOutFixture(t, &fakeConvTarget{
		fakeConvSource: fakeConvSource{name: "claude-code"},
	})
	err := strictConversationFanOut(t, orch, "missing-artifact-id")
	require.Equal(t, ErrInboundNativeMaterialization, err)
}

// Zero-knowledge: the cause is recorded in the deferral journal and published
// on the event bus, so it may carry a redacted destination and never a turn.
func TestConversationDeclineError_RedactsHomePaths(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	require.NotEmpty(t, home)
	dest := filepath.Join(home, ".claude", "projects", "-Users-someone", "session.jsonl")

	decline := newConversationDeclineError(
		"claude-code", "019e0000-62df", acf.MainBranch, adapter.SessionDeclineForkedMirror, dest)
	require.NotContains(t, decline.Error(), home)
	require.NotContains(t, decline.Path, home)
	require.Contains(t, decline.Error(), string(adapter.SessionDeclineForkedMirror))
	require.True(t, errors.Is(decline, ErrInboundNativeMaterialization))
}

// End to end with the real adapter: a native-origin Claude session that
// diverged must arrive at the deferral layer as a structural decline, not as
// a content-free sentinel that makes distinct entries indistinguishable.
func TestStrictFanOut_RealClaudeNativeDivergenceReachesTheQueue(t *testing.T) {
	root := realTempDir(t)
	store := &acf.Store{Root: filepath.Join(root, "store")}
	require.NoError(t, store.Init())
	claudeProjects := filepath.Join(root, ".claude", "projects")
	sessionID := "diverged-native-session"
	sourcePath := filepath.Join(claudeProjects, "-Users-exampleuser", sessionID+".jsonl")
	shared := []acf.TextTurn{
		{Role: "user", Text: "what is the capital of Canada?"},
		{Role: "assistant", Text: "Ottawa."},
	}
	native := append(append([]acf.TextTurn(nil), shared...),
		acf.TextTurn{Role: "user", Text: "asked only inside the agent"},
	)
	writeNativeClaudeConversation(t, sourcePath, sessionID, native)

	artifactID := acf.NewID()
	now := time.Now().UTC()
	require.NoError(t, store.WriteArtifact(acf.Artifact{
		AcfSchemaVersion: acf.SchemaVersion,
		ArtifactID:       artifactID,
		Kind:             acf.KindConversation,
		Scope:            acf.ScopeGlobal,
		Name:             filepath.Base(sourcePath),
		SourcePath:       sourcePath,
		CreatedAt:        now,
		UpdatedAt:        now,
	}))
	appendConversationEvent(t, store, artifactID, acf.MainBranch, "", "claude-code",
		append(append([]acf.TextTurn(nil), shared...),
			acf.TextTurn{Role: "user", Text: "asked only on the other device"},
		))

	claude := claudecode.New()
	claude.HomeDir = root
	codexSource := &fakeConvSource{name: "codex"}
	orch, err := NewOrchestrator(Config{
		Dir: root, Adapters: []adapter.Adapter{claude, codexSource}, Store: store,
		RootsByAdapter: map[string][]string{"claude-code": {claudeProjects}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = orch.Close() })

	before, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	fanOutErr := strictConversationFanOut(t, orch, artifactID)
	require.ErrorIs(t, fanOutErr, ErrInboundNativeMaterialization)
	var decline *ConversationDeclineError
	require.ErrorAs(t, fanOutErr, &decline)
	require.Equal(t, adapter.SessionDeclineDiverged, decline.Reason)
	require.Equal(t, ConversationRetryStructural, decline.RetryClass)
	after, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	require.Equal(t, before, after, "Stage A never rewrites a native transcript")

	// recordDeferredMaterializationFailure stores redactPaths(cause.Error()) in
	// the on-disk journal, so the cause must stay content-free even though the
	// two sides now hold different turns.
	require.NotContains(t, fanOutErr.Error(), "asked only inside the agent")
	require.NotContains(t, fanOutErr.Error(), "asked only on the other device")
}

// strictMaterializationFailure must never drop the sentinel, whatever cause it
// is handed — that sentinel is the contract three packages match on.
func TestStrictMaterializationFailure_AlwaysCarriesTheSentinel(t *testing.T) {
	require.Equal(t, ErrInboundNativeMaterialization, strictMaterializationFailure(nil))

	foreign := errors.New("read artifact index: permission denied")
	wrapped := strictMaterializationFailure(foreign)
	require.ErrorIs(t, wrapped, ErrInboundNativeMaterialization)
	require.ErrorIs(t, wrapped, foreign)

	already := fmt.Errorf("%w (%w)", ErrInboundNativeMaterialization, errDeferredMaterializationWithheld)
	require.Equal(t, already, strictMaterializationFailure(already),
		"a cause that already carries the sentinel must pass through untouched")
}
