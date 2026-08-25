package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/secureexec"
	"github.com/aplexica/aplexica/internal/plugin/truststate"
	"github.com/spf13/cobra"
)

var (
	repairStoreRoot  string
	repairApply      bool
	repairBackupDir  string
	repairAll        bool
	repairDeviceID   string
	repairStateDir   string
	repairCheckPeers bool
	repairForce      bool
)

// repairTurnPreviewMaxLen bounds how much of a dropped turn's text is echoed
// back in the dry-run/apply diff (see summarize in cmd_show.go). Named so the
// literal lives in a const declaration — magiclint's one documented exemption
// for a tunable that isn't itself a runtime configuration knob.
const repairTurnPreviewMaxLen = 80

var repairCmd = &cobra.Command{
	Use:   "repair",
	Short: "Offline canonical-store repair commands",
}

var repairConversationCmd = &cobra.Command{
	Use:   "conversation [artifact-id]",
	Short: "Collapse echo-duplicated turns in a conversation's main-branch head",
	Long: `Repairs a conversation artifact whose canonical main-branch head was
polluted by a fixed continuation bug: turns copied onto the same or another
agent's session could be appended a second time, echoing a user question, an
assistant answer, or a whole question+answer block back into the thread.

Three shapes are collapsed:

  * a run of two or more consecutive turn events, containing at least one
    user turn, whose complete bodies — timestamps included — reproduce an
    earlier run in the same thread. This is the repeated-BLOCK shape, which
    no other rule catches because the copies are not adjacent;
  * a turn that is an exact adjacent repeat of the turn before it;
  * a trailing user turn that exactly repeats an earlier user turn already
    answered in the thread.

Repeats that are none of those are legitimate and are never touched. Non-turn
events (tool calls, tool results, system notes) are never dropped; exact
duplicates among them are only reported.

By default this prints the proposed collapse without writing anything. Pass
--apply to back up the event log and commit the collapsed turns as one new
canonical event. Only the main branch is inspected and repaired; any side
branches are left untouched.

Pass --all instead of an artifact id to sweep every conversation in the store.

Duplicated threads do not converge on their own — a peer still holding the
duplicates classifies the repaired state as a stale redelivery and skips it, so
repair every device, with every daemon stopped, before restarting any of them.
Pass --device-id so the repaired head can be republished; without it the head
is not eligible for the outbound sweep and the broker keeps serving the
duplicated copy.

--apply proves THIS device's daemon is stopped by taking its instance lock,
but a fleet flag-day needs every OTHER device stopped too — a running peer
still holding the duplicates re-poisons the fleet by republishing its polluted
copy. Pass --check-peers with --apply to have the command query the cloud
plugin's pairing state (the same '--status' path 'aplexica remote status'
relies on) and refuse to write while a live relay pairing is reported;
--force overrides the refusal once you have confirmed by hand that every
peer daemon is stopped.`,
	Args: cobra.MaximumNArgs(1),
	RunE: runRepairConversation,
}

func init() {
	home, _ := os.UserHomeDir()
	repairConversationCmd.Flags().StringVar(&repairStoreRoot, "store",
		filepath.Join(home, ".aplexica", "store"), "Canonical store root directory")
	repairConversationCmd.Flags().BoolVar(&repairApply, "apply", false,
		"Write the repair instead of only printing the proposed collapse (default: dry run)")
	repairConversationCmd.Flags().StringVar(&repairBackupDir, "backup-dir", "",
		"Directory for the pre-repair event-log backup (default: a directory beside --store)")
	repairConversationCmd.Flags().BoolVar(&repairAll, "all", false,
		"Sweep every conversation in the store instead of one artifact id")
	repairConversationCmd.Flags().StringVar(&repairDeviceID, "device-id", "",
		"Stamp this device id on the repair event so the repaired head can be republished")
	repairConversationCmd.Flags().StringVar(&repairStateDir, "state-dir",
		filepath.Join(home, ".aplexica", "state"),
		"Daemon state directory, used to prove the daemon is stopped before --apply and to resolve the plugin --check-peers queries")
	repairConversationCmd.Flags().BoolVar(&repairCheckPeers, "check-peers", false,
		"With --apply: refuse to write while the cloud plugin reports a live relay pairing (other devices may be running)")
	repairConversationCmd.Flags().BoolVar(&repairForce, "force", false,
		"Override the --check-peers refusal after confirming by hand that every peer daemon is stopped")
	repairCmd.AddCommand(repairConversationCmd)
	rootCmd.AddCommand(repairCmd)
}

func runRepairConversation(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	if repairAll == (len(args) == 1) {
		return errors.New("repair conversation takes either one artifact id or --all, not both and not neither")
	}
	// Rejecting the meaningless combinations up front keeps the guard flags
	// honest: --check-peers must never be a silent no-op an operator believes
	// protected a dry run, and --force must never read as a generic "yes".
	if repairCheckPeers && !repairApply {
		return errors.New("--check-peers guards --apply; add --apply, or drop --check-peers for a dry run")
	}
	if repairForce && !repairCheckPeers {
		return errors.New("--force only overrides the --check-peers refusal; add --check-peers (or drop --force)")
	}
	store := &acf.Store{Root: repairStoreRoot}

	if repairApply {
		// acf's append lock is an in-process mutex, so a concurrent daemon
		// append is not serialized against this one. The daemon holds its
		// instance lock for its whole lifetime; acquiring it here is the only
		// proof it is really stopped (a failed control probe is not — a wrong
		// --state-dir or the pre-bind startup window looks identical).
		instanceLock, lockErr := daemon.Acquire(filepath.Join(repairStateDir, "aplexicad.lock"))
		if lockErr != nil {
			return fmt.Errorf(
				"refusing to repair while the daemon may be running: stop it first "+
					"(is it running with a different --state-dir?): %w", lockErr)
		}
		defer func() { _ = instanceLock.Release() }()
	}

	// The guard is evaluated lazily at the read-only → mutating boundary and
	// memoized: a repair with nothing to collapse never spawns a plugin
	// process, and an --all sweep queries the plugin at most once (before its
	// first write) rather than per artifact.
	guard := &repairPeerGuardState{ctx: cmd.Context(), out: out}

	if !repairAll {
		_, err := repairOneConversation(out, store, args[0], guard)
		return err
	}
	return repairAllConversations(out, store, guard)
}

// repairAllConversations sweeps every conversation artifact. A store holding
// thousands of threads makes per-artifact invocation impractical, and the
// duplication bug affected them in bulk (one scan-cache schema bump re-imports
// every generated mirror at once). Artifacts are visited in id order so two
// devices produce the same report, and a per-artifact failure is reported and
// stepped over rather than abandoning the sweep partway through.
func repairAllConversations(out io.Writer, store *acf.Store, guard *repairPeerGuardState) error {
	artifacts, err := store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return fmt.Errorf("listing conversations: %w", err)
	}
	ids := make([]string, 0, len(artifacts))
	for _, art := range artifacts {
		ids = append(ids, art.ArtifactID)
	}
	sort.Strings(ids)

	repaired, failed := 0, 0
	for _, id := range ids {
		changed, rerr := repairOneConversation(out, store, id, guard)
		if errors.Is(rerr, errRepairPeerGuardRefused) {
			// The guard's verdict is memoized, so every remaining artifact
			// would refuse identically: abort the sweep with ONE refusal
			// instead of drowning it in per-artifact SKIPPED lines. Nothing
			// has been written — the guard sits before the first backup.
			return rerr
		}
		switch {
		case rerr != nil:
			failed++
			fmt.Fprintf(out, "artifact %s: SKIPPED — %v\n", id, rerr)
		case changed:
			repaired++
		}
	}
	verb := "would repair"
	if repairApply {
		verb = "repaired"
	}
	fmt.Fprintf(out, "\nscanned %d conversation%s; %s %d; could not inspect %d\n",
		len(ids), plural(len(ids), "", "s"), verb, repaired, failed)
	if failed > 0 {
		return fmt.Errorf("%d conversation%s could not be inspected", failed, plural(failed, "", "s"))
	}
	return nil
}

// repairOneConversation reports whether artifactID needs (or received) a
// repair. A quiet artifact prints nothing under --all: a sweep over thousands
// of threads must surface the handful that matter.
func repairOneConversation(out io.Writer, store *acf.Store, artifactID string, guard *repairPeerGuardState) (bool, error) {
	payload, head, ok, err := store.MaterializedConversationHeadFromStore(artifactID)
	if err != nil {
		return false, fmt.Errorf("reading conversation head for %s: %w", artifactID, err)
	}
	if !ok {
		if repairAll {
			// Most of a real store's artifact records are shells: metadata
			// registered from a peer's catalog or a desktop listing, with no
			// event log yet. Out of scope for a sweep, not a fault.
			return false, nil
		}
		return false, fmt.Errorf("artifact %s: no conversation found in the canonical store", artifactID)
	}
	if payload.Format != acf.ConversationFormatV1 {
		if repairAll {
			// A sweep meets hermes SessionBundles and other non-event-list
			// payloads constantly; they are simply out of scope, not a fault.
			return false, nil
		}
		return false, fmt.Errorf(
			"artifact %s: payload format %q has no event list to repair (repair only understands %q)",
			artifactID, payload.Format, acf.ConversationFormatV1)
	}

	// Compute both projections up front — the turn-level diff is what a human
	// approves below, the event-level collapse is what --apply actually
	// commits to the store. They must be cross-checked BEFORE anything is
	// printed, in both dry-run and apply, so a dry run is a truthful preview
	// of what apply would do (including refusing).
	beforeTurns := acf.ExtractTextTurns(payload.Events)
	afterTurns, turnsChanged := collapseDuplicateConversationTurns(payload.Events)
	collapsedEvents, _ := collapseConversationEvents(payload.Events)

	// The addendum called this mapping "the only thing standing between you
	// and a silent drift where the command deletes the wrong event." Refuse
	// outright rather than ever committing an event-level result that doesn't
	// reproduce the turn-level diff a human is about to approve.
	if !acf.TextTurnsEqual(acf.ExtractTextTurns(collapsedEvents), afterTurns) {
		return false, fmt.Errorf(
			"artifact %s: refusing to repair — the event-level collapse does not reproduce the previewed turn-level diff; this indicates a bug in the repair tool, not a problem with your data",
			artifactID)
	}

	if !turnsChanged {
		if !repairAll {
			fmt.Fprintf(out, "artifact %s (main branch): %d turns, %d events\n",
				artifactID, len(beforeTurns), len(payload.Events))
			fmt.Fprintln(out, "no echo-duplicated turns found; nothing to repair")
		}
		return false, nil
	}

	sideBranches, err := sideBranchNames(store, artifactID)
	if err != nil {
		return false, fmt.Errorf("checking for side branches on %s: %w", artifactID, err)
	}

	fmt.Fprintf(out, "artifact %s (main branch): %d turns before, %d turns after (%d events before, %d events after)\n",
		artifactID, len(beforeTurns), len(afterTurns), len(payload.Events), len(collapsedEvents))
	if len(sideBranches) > 0 {
		fmt.Fprintf(out, "note: side branch(es) present (%s) — left untouched by this repair\n",
			strings.Join(sideBranches, ", "))
	}
	for _, note := range duplicateNonTurnEventNotes(payload.Events) {
		fmt.Fprintf(out, "  %s\n", note)
	}

	keep := conversationTurnKeepMask(payload.Events)
	for i, turn := range beforeTurns {
		if !keep[i] {
			fmt.Fprintf(out, "  drop turn [%d] %s: %s\n", i, turn.Role, summarize(turn.Text, repairTurnPreviewMaxLen))
		}
	}

	if !repairApply {
		fmt.Fprintf(out, "store root: %s\n", repairStoreRoot)
		fmt.Fprintf(out, "backup would be written under %s\n", resolveRepairBackupDir(repairStoreRoot, repairBackupDir))
		fmt.Fprintln(out, "dry run only (default); rerun with --apply to write the repair")
		return true, nil
	}

	// The peer guard sits exactly at the read-only → mutating boundary: the
	// proposed collapse above is safe to print regardless, and a repair with
	// nothing to collapse returned before ever spawning a plugin process.
	if err := guard.check(); err != nil {
		return false, err
	}

	backupPath, err := backupConversationEventLog(repairStoreRoot, repairBackupDir, artifactID)
	if err != nil {
		return false, fmt.Errorf("backing up event log for %s: %w", artifactID, err)
	}
	fmt.Fprintf(out, "backed up event log to %s\n", backupPath)

	encoded, err := acf.EncodePayload(acf.ConversationPayload{
		Format:      acf.ConversationFormatV1,
		Events:      collapsedEvents,
		Attachments: payload.Attachments,
	})
	if err != nil {
		return false, fmt.Errorf("encoding repaired payload for %s: %w", artifactID, err)
	}

	parentHash := head.Hash
	if head.Type == acf.EventTypeBaseline && head.AlignedHead != "" {
		parentHash = head.AlignedHead
	}

	ev := acf.Event{
		EventID:    acf.NewID(),
		ArtifactID: artifactID,
		Type:       acf.EventTypeUpdate,
		Branch:     acf.MainBranch,
		Timestamp:  time.Now().UTC(),
		// DeviceID is what makes the repaired head eligible for the outbound
		// sweep (remoteSweepLocalHead skips a head this device did not author).
		// Without it the repair stays local and the broker's retained slot keeps
		// serving the duplicated copy back to the fleet.
		Provenance: acf.Provenance{
			DeviceID:       repairDeviceID,
			SourceAgent:    "aplexica:repair",
			AdapterVersion: "aplexica:repair",
		},
		Payload:    encoded,
		ParentHash: parentHash,
		EventTags:  []string{acf.ConversationRepairCommandEventTag},
	}
	// The parent above is derived from the LOG tail (with the baseline
	// AlignedHead override), but a plain AppendEvent validates it against the
	// artifact's head bookkeeping — which can sit one event behind its own log
	// and otherwise wedge repair with a ParentHash mismatch.
	// AppendEventWithRefreshedParent accepts the
	// refreshed log head for exactly this stale-bookkeeping shape, under the
	// per-artifact lock, so a real concurrent append is still rejected.
	if err := store.AppendEventWithRefreshedParent(acf.KindConversation, ev); err != nil {
		return false, fmt.Errorf("appending repaired event for %s: %w", artifactID, err)
	}
	fmt.Fprintln(out, "repair applied")
	return true, nil
}

// errRepairPeerGuardRefused marks every --check-peers refusal (a reported
// live pairing AND an unanswerable query alike) so the --all sweep can tell
// "this artifact could not be inspected" from "the fleet guard said stop" and
// abort instead of stepping over it per artifact.
var errRepairPeerGuardRefused = errors.New("--check-peers refused the apply")

// repairPeerStatusQuery reports the configured cloud plugin's pairing state.
// A package-level var so tests can substitute a fake fleet; production always
// points at queryConfiguredRemotePluginPairing.
var repairPeerStatusQuery = queryConfiguredRemotePluginPairing

// repairPeerGuardState memoizes the --check-peers verdict for one command
// invocation: the guard sits at the read-only → mutating boundary inside
// repairOneConversation, and an --all sweep crosses that boundary once per
// polluted artifact — the fleet must be asked once, not thousands of times.
type repairPeerGuardState struct {
	ctx  context.Context
	out  io.Writer
	done bool
	err  error
}

// check enforces --check-peers at the mutating boundary. First call decides
// (or, under --force, warns and waives); later calls replay the memoized
// verdict.
func (g *repairPeerGuardState) check() error {
	if !repairCheckPeers {
		return nil
	}
	if g.done {
		return g.err
	}
	g.done = true
	if repairForce {
		fmt.Fprintln(g.out, "warning: --force set; skipping the --check-peers live-relay guard")
		return nil
	}
	g.err = repairConversationPeerGuard(g.ctx)
	return g.err
}

// repairConversationPeerGuard enforces --check-peers: a repair --apply while
// any peer daemon is running re-poisons the fleet (the peer skips the
// shrunken head as a stale redelivery and republishes its polluted copy), so
// the guard refuses while the plugin reports a live relay pairing. It fails
// CLOSED — an unanswerable query is a refusal, not a pass — because a guard
// that degrades to "allowed" on error guards nothing; --force is the explicit
// escape hatch for both refusal shapes.
func repairConversationPeerGuard(ctx context.Context) error {
	paired, deviceID, err := repairPeerStatusQuery(ctx)
	if err != nil {
		return fmt.Errorf(
			"%w: cannot query the cloud plugin for relay state: %w — the repair flag-day requires every daemon in the fleet stopped simultaneously; confirm that by hand, then rerun with --force to override",
			errRepairPeerGuardRefused, err)
	}
	if !paired {
		return nil
	}
	return fmt.Errorf(
		"%w: the cloud plugin reports a live relay pairing (device_id %s), so other devices on this account may be running; applying this repair is a fleet flag-day: every daemon must be stopped simultaneously, or a running peer still holding the duplicates will skip the repaired head as a stale redelivery and republish its polluted copy; stop every daemon in the fleet, then rerun (--force overrides once you have confirmed that by hand)",
		errRepairPeerGuardRefused, deviceID)
}

// queryConfiguredRemotePluginPairing resolves the daemon's configured remote
// plugin and asks it for its pairing state over the same `<plugin> --status`
// path the daemon boot sequence and `aplexica remote status` rely on
// (queryPluginStatus), with the same authorization the standalone CLI applies
// before executing a plugin binary (compiled publisher trust + the on-disk
// trust store — mirroring authenticatedExistingGenesisStatus). The plugin
// configuration is read from --state-dir — the same directory whose instance
// lock proved the local daemon stopped — so the guard queries the plugin of
// the daemon actually being repaired. No configured plugin means this store
// participates in no relay, so no peer can redeliver against it: that reports
// unpaired rather than erroring.
func queryConfiguredRemotePluginPairing(ctx context.Context) (paired bool, deviceID string, err error) {
	stateRoot := repairStateDir
	cfg, err := daemon.LoadConfig(filepath.Join(stateRoot, "config.json"))
	if err != nil {
		return false, "", fmt.Errorf("load daemon configuration: %w", err)
	}
	if cfg.StateDir != "" {
		stateRoot, err = filepath.Abs(cfg.StateDir)
		if err != nil {
			return false, "", err
		}
	}
	execPath := cfg.Remote.Executable
	if execPath == "" {
		return false, "", nil
	}
	verified, err := verifyRemotePluginWithCompiledTrust(execPath)
	if err != nil {
		return false, "", fmt.Errorf("verify configured remote plugin: %w", err)
	}
	trustStore := truststate.Store{Root: filepath.Join(stateRoot, "remote-plugin-trust")}
	if _, err := trustStore.VerifyCurrent(execPath, verified, remotePluginTrustPolicy()); err != nil {
		return false, "", fmt.Errorf("authorize configured remote plugin: %w", err)
	}
	// The checked variant, not queryPluginStatus: the degrading form maps a
	// crashed/hung/unparseable plugin to paired=false, which this guard would
	// read as "no peers" — the opposite of failing closed.
	paired, deviceID, _, err = queryPluginStatusChecked(ctx, execPath,
		func(callCtx context.Context, candidate string, args ...string) (preparedRemotePluginCommand, error) {
			if candidate != execPath {
				return nil, errors.New("configured plugin path changed before status")
			}
			return secureexec.Prepare(callCtx, candidate, verified.Manifest.BinarySHA256, args...)
		})
	if err != nil {
		return false, "", fmt.Errorf("query configured remote plugin status: %w", err)
	}
	return paired, deviceID, nil
}

// sideBranchNames returns the distinct non-main branch names present in an
// artifact's physical event log. It reads the raw event log directly
// (Store.ReadEvents is a plain read with no index side effects) rather than
// Store.ListBranches/RefreshBranchIndex, which lazily writes a branch-index
// cache file on every call — a write this command's dry-run mode must never
// trigger.
func sideBranchNames(store *acf.Store, artifactID string) ([]string, error) {
	events, err := store.ReadEvents(acf.KindConversation, artifactID)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, ev := range events {
		if ev.Branch == "" || ev.Branch == acf.MainBranch {
			continue
		}
		if !seen[ev.Branch] {
			seen[ev.Branch] = true
			names = append(names, ev.Branch)
		}
	}
	return names, nil
}

// conversationTurnKeepMask is the single source of truth for the collapse
// rule: index i of the returned mask reports whether the i'th VISIBLE TURN of
// events survives the repair. collapseDuplicateConversationTurns (turn-level)
// and collapseConversationEvents (event-level) are both thin wrappers over
// this so the two projections can never drift apart.
//
// It takes events rather than turns because pass 0 needs the canonical event
// body; the later passes still see only role+text.
//
// Pass 0 drops a replayed BLOCK: a contiguous run of turns whose COMPLETE
// canonical event bodies — timestamps included — reproduce, in order, a
// contiguous run that already occurred earlier in the thread. That is the
// shape the continuation bug produces and the one neither other pass can see,
// because the copies are neither adjacent nor trailing.
//
// Pass 1 drops a turn that is byte-identical (role + text) to the turn
// immediately preceding it among pass-0 survivors — an adjacent echo. Pass 2
// then inspects whichever turn survives pass 1 in the last position: if it is
// a user turn whose text exactly repeats an earlier surviving user turn, it is
// dropped too — a trailing echo of a question already answered. Repeats that
// are none of those are legitimate (a user may genuinely ask the same thing
// twice) and are never touched.
func conversationTurnKeepMask(events []acf.ConversationEvent) []bool {
	turns := acf.ExtractTextTurns(events)
	keep := replayedTurnBlockKeepMask(turns, visibleTurnIdentities(events, len(turns)))

	// Pass 1: adjacent echo, measured against the previous SURVIVING turn so a
	// collapsed block cannot hide an adjacency it created.
	prev := -1
	for i := range turns {
		if !keep[i] {
			continue
		}
		if prev >= 0 && turns[prev].Role == turns[i].Role && turns[prev].Text == turns[i].Text {
			keep[i] = false
			continue
		}
		prev = i
	}

	// Pass 2: trailing user turn repeating an earlier answered question.
	last := -1
	for i := len(turns) - 1; i >= 0; i-- {
		if keep[i] {
			last = i
			break
		}
	}
	if last <= 0 || turns[last].Role != "user" {
		return keep
	}
	for i := 0; i < last; i++ {
		if keep[i] && turns[i].Role == "user" && turns[i].Text == turns[last].Text {
			keep[last] = false
			break
		}
	}
	return keep
}

// visibleTurnIdentities returns one identity string per visible turn, aligned
// with acf.ExtractTextTurns's output. An event with no encodable identity gets
// a unique placeholder so it can never match anything.
func visibleTurnIdentities(events []acf.ConversationEvent, turnCount int) []string {
	identities := make([]string, 0, turnCount)
	for i, ev := range events {
		if !isVisibleTurnEvent(ev) || len(identities) >= turnCount {
			continue
		}
		identity, ok := conversationRepairEventIdentity(ev)
		if !ok {
			identity = fmt.Sprintf("\x00unencodable#%d", i)
		}
		identities = append(identities, identity)
	}
	return identities
}

// replayedTurnBlockKeepMask implements pass 0: drop each contiguous run of
// turns that reproduces, in order, a contiguous run occurring strictly earlier
// in the thread. The scan is greedy and left-to-right, so the first occurrence
// of a repeated block is always the one kept, which makes the result
// deterministic (every device computes the same mask from the same log),
// idempotent, and prefix-preserving.
//
// Two conditions keep it away from legitimate content:
//
//   - the run must be at least two turns long. An isolated repeated turn is
//     not the bug's signature and is far more likely to be real.
//   - the run must contain a user turn. A continuation always begins with a
//     user prompt, so every block the bug replays has one; a run of identical
//     assistant rows does not qualify. This matters because a flattened
//     external-agent transcript stamps a whole imported batch with ONE
//     timestamp, so a genuinely repeated tool row (the same file read twice)
//     is byte-identical to its predecessor through no fault of sync. Those
//     runs are assistant-only and are preserved.
func replayedTurnBlockKeepMask(turns []acf.TextTurn, identities []string) []bool {
	keep := make([]bool, len(turns))
	for i := range keep {
		keep[i] = true
	}
	if len(identities) != len(turns) {
		return keep // cursor drift; the caller's cross-check refuses the repair
	}
	firstAt := make(map[string]int, len(identities))
	for i, identity := range identities {
		if _, seen := firstAt[identity]; !seen {
			firstAt[identity] = i
		}
	}
	for i := 0; i < len(turns); {
		origin, seen := firstAt[identities[i]]
		if !seen || origin >= i {
			i++
			continue
		}
		// Extend while the earlier run keeps matching and stays clear of this one.
		length := 0
		for i+length < len(turns) && origin+length < i &&
			identities[i+length] == identities[origin+length] {
			length++
		}
		if length < 2 || !containsUserTurn(turns[i:i+length]) {
			i++
			continue
		}
		for j := i; j < i+length; j++ {
			keep[j] = false
		}
		i += length
	}
	return keep
}

func containsUserTurn(turns []acf.TextTurn) bool {
	for _, turn := range turns {
		if turn.Role == "user" {
			return true
		}
	}
	return false
}

// conversationRepairEventIdentity fingerprints the COMPLETE canonical body of
// one conversation event, Timestamp included, so pass 0 collapses only a
// literal replay of the same logical event.
//
// It is deliberately not syncd's conversationEventKey, which hashes just role,
// type and content: two distinct parallel tool calls sharing their assistant
// message's timestamp collide under that key, and a repair that deletes
// content must never run on an identity coarser than the content itself. It is
// also not acf's legacyConversationContentKey, which zeroes the Timestamp on
// purpose for the re-timestamp repair — the opposite equivalence to this one.
//
// ok=false on an unencodable event means "no identity", so such an event is
// always kept rather than silently matching another.
func conversationRepairEventIdentity(ev acf.ConversationEvent) (string, bool) {
	encoded, err := json.Marshal(ev)
	if err != nil {
		return "", false
	}
	return string(encoded), true
}

// duplicateNonTurnEventNotes reports exact-identity repeats among the events
// the repair never drops — tool calls, tool results, system notes. A duplicated
// tool block is the same corruption wearing a different hat, but collapsing it
// is not safe from this distance (a retried tool call is legitimate), so it is
// surfaced for a human instead of being acted on.
func duplicateNonTurnEventNotes(events []acf.ConversationEvent) []string {
	seen := make(map[string]struct{}, len(events))
	counts := map[string]int{}
	var order []string
	for _, ev := range events {
		if isVisibleTurnEvent(ev) {
			continue
		}
		identity, ok := conversationRepairEventIdentity(ev)
		if !ok {
			continue
		}
		if _, dup := seen[identity]; dup {
			if counts[identity] == 0 {
				order = append(order, identity)
			}
			counts[identity]++
			continue
		}
		seen[identity] = struct{}{}
	}
	notes := make([]string, 0, len(order))
	for _, identity := range order {
		var ev acf.ConversationEvent
		_ = json.Unmarshal([]byte(identity), &ev)
		notes = append(notes, fmt.Sprintf(
			"note: %s event repeated %d extra time(s) — reported only, never dropped by repair",
			ev.Type, counts[identity]))
	}
	return notes
}

// collapseDuplicateConversationTurns removes replayed turn events, ADJACENT
// identical turns, and a trailing user turn that exactly repeats a user turn
// already answered earlier in the thread. All three are echo shapes. Repeats
// that are none of them are legitimate and are never touched.
func collapseDuplicateConversationTurns(events []acf.ConversationEvent) ([]acf.TextTurn, bool) {
	turns := acf.ExtractTextTurns(events)
	keep := conversationTurnKeepMask(events)
	out := make([]acf.TextTurn, 0, len(turns))
	for i, turn := range turns {
		if keep[i] {
			out = append(out, turn)
		}
	}
	return out, len(out) != len(turns)
}

// collapseConversationEvents applies the same collapse rule at the event
// level: it walks payload.Events, advancing a visible-turn cursor only when
// an event contributes a visible turn (isVisibleTurnEvent — mirroring
// acf.ExtractTextTurns's own predicate exactly), and drops only the events
// whose visible-turn index conversationTurnKeepMask rejects. Every
// non-visible event (tool calls/results, system notes, non-user/assistant
// roles, and turns acf.NormalizeTextTurn filters out) is preserved untouched
// regardless of the mask, since the mask has no opinion on them.
func collapseConversationEvents(events []acf.ConversationEvent) ([]acf.ConversationEvent, bool) {
	keep := conversationTurnKeepMask(events)
	out := make([]acf.ConversationEvent, 0, len(events))
	cursor := 0
	for _, ev := range events {
		if isVisibleTurnEvent(ev) {
			if cursor >= len(keep) {
				// isVisibleTurnEvent has drifted out of sync with ExtractTextTurns and
				// over-counted relative to the mask computed from its own output.
				// Reporting no change here (rather than indexing out of bounds) lets
				// the caller's cross-check against the turn-level diff catch the drift
				// and refuse the repair instead of panicking.
				return events, false
			}
			k := keep[cursor]
			cursor++
			if !k {
				continue
			}
		}
		out = append(out, ev)
	}
	return out, len(out) != len(events)
}

// isVisibleTurnEvent reports whether ev contributes one entry to
// acf.ExtractTextTurns's projection. It must mirror ExtractTextTurns's own
// predicate exactly, or collapseConversationEvents's visible-turn cursor
// drifts out of sync with the mask computed from ExtractTextTurns's output.
func isVisibleTurnEvent(ev acf.ConversationEvent) bool {
	if ev.Type != acf.EventTypeTurn {
		return false
	}
	if ev.Role != "user" && ev.Role != "assistant" {
		return false
	}
	_, ok := acf.NormalizeTextTurn(ev.Role, joinConversationTextBlocks(ev.Content))
	return ok
}

// joinConversationTextBlocks reproduces acf's unexported joinTextBlocks
// exactly (internal/acf/conversation_turns.go:182): concatenate the Text of
// "text" content blocks, joined by a blank line. isVisibleTurnEvent needs the
// identical reduction ExtractTextTurns uses internally to stay in lockstep
// with it.
func joinConversationTextBlocks(blocks []acf.ContentBlock) string {
	var parts []string
	for _, c := range blocks {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// conversationEventLogPath mirrors acf's private eventsRel/kindDir for
// KindConversation (internal/acf/store.go:199-213: kindDir(KindConversation)
// == "conversations"). acf does not export a path accessor and repair needs
// the raw JSONL path to back it up before any mutating append.
func conversationEventLogPath(storeRoot, artifactID string) string {
	return filepath.Join(storeRoot, "events", "conversations", artifactID+".jsonl")
}

// resolveRepairBackupDir returns the directory a repair backup will be
// written to: backupDir if the caller set one via --backup-dir, else the
// default location beside storeRoot (never inside it). Shared by the dry-run
// preview and the actual --apply backup so the two can never drift apart.
func resolveRepairBackupDir(storeRoot, backupDir string) string {
	if backupDir != "" {
		return backupDir
	}
	return filepath.Join(filepath.Dir(storeRoot), "aplexica-repair-backups")
}

// backupConversationEventLog copies an artifact's event log to backupDir
// (defaulting to a directory beside storeRoot, never inside it) before any
// mutating append, naming the copy so repeated runs never collide. It
// fails if the event log does not exist — the caller must never append
// without a backup successfully written first. The copy is streamed with
// io.Copy rather than read whole into memory, since an event log can be
// very large.
func backupConversationEventLog(storeRoot, backupDir, artifactID string) (string, error) {
	src, err := os.Open(conversationEventLogPath(storeRoot, artifactID))
	if err != nil {
		return "", fmt.Errorf("reading event log: %w", err)
	}
	defer func() { _ = src.Close() }()

	backupDir = resolveRepairBackupDir(storeRoot, backupDir)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", fmt.Errorf("creating backup directory %s: %w", backupDir, err)
	}
	name := fmt.Sprintf("%s-%s.jsonl", artifactID, time.Now().UTC().Format("20060102T150405.000000000Z"))
	dst := filepath.Join(backupDir, name)
	// O_EXCL makes "repeated runs never overwrite each other" an enforced
	// guarantee rather than one resting on nanosecond-timestamp uniqueness:
	// a genuine collision fails loudly instead of silently truncating an
	// existing backup.
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("creating backup file %s: %w", dst, err)
	}
	fileOpen := true
	defer func() {
		if fileOpen {
			_ = f.Close()
		}
	}()
	if _, err := io.Copy(f, src); err != nil {
		return "", fmt.Errorf("writing backup %s: %w", dst, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("closing backup %s: %w", dst, err)
	}
	fileOpen = false
	return dst, nil
}
