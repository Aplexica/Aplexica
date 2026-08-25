package syncd

import (
	"os"
	"sort"

	"github.com/aplexica/aplexica/internal/acf"
	"github.com/aplexica/aplexica/internal/adapter"
)

// missingRemoteProjectionScanLimit bounds the Windows startup audit. The
// reported failure is canonical state that arrived while its native Claude
// projection was unavailable; scanning only the newest conversations keeps
// startup independent of total store size.
const missingRemoteProjectionScanLimit = 50

func (o *Orchestrator) seedPlatformMissingRemoteConversationProjections() {
	if repairMissingRemoteConversationProjectionsAtStartup {
		o.seedMissingRemoteConversationProjections()
	}
}

// seedMissingRemoteConversationProjections queues only peer-device
// conversations whose target adapter can predict a stable native session path
// and whose predicted path is absent. It deliberately does not touch existing
// native files, locally-owned conversations, non-session targets, cloud
// cursors, or canonical event identity.
//
// The startup hook is Windows-only. The implementation remains in an ordinary
// file so its safety properties can be exercised by the platform-neutral test
// suite without changing macOS or Linux runtime behavior.
func (o *Orchestrator) seedMissingRemoteConversationProjections() {
	if o == nil || o.cfg.Store == nil {
		return
	}
	conversations, err := o.cfg.Store.ListArtifacts(acf.KindConversation)
	if err != nil {
		return
	}
	sort.SliceStable(conversations, func(i, j int) bool {
		return conversations[i].UpdatedAt.After(conversations[j].UpdatedAt)
	})
	if len(conversations) > missingRemoteProjectionScanLimit {
		conversations = conversations[:missingRemoteProjectionScanLimit]
	}

	type target struct {
		name    string
		planner adapter.ConversationSessionPathTarget
	}
	targets := make([]target, 0, len(o.cfg.Adapters))
	for _, ad := range o.cfg.Adapters {
		planner, ok := ad.(adapter.ConversationSessionPathTarget)
		if !ok {
			continue
		}
		targets = append(targets, target{name: ad.Name(), planner: planner})
	}

	for _, art := range conversations {
		if art.RemoteOriginDeviceID == "" ||
			o.localDeviceID() != "" && art.RemoteOriginDeviceID == o.localDeviceID() {
			continue
		}
		for _, target := range targets {
			branch := selectedBranchForAgent(art, target.name)
			head, ok, headErr := conversationHeadForBranch(o.cfg.Store, art.ArtifactID, branch)
			if headErr != nil || !ok {
				continue
			}
			origin := head.Provenance.SourceAgent
			if _, withheld := o.deferredMaterializationTargetWithheld(target.name, art, origin, true); withheld {
				continue
			}
			path, supported, pathErr := target.planner.ConversationSessionPath(art, head, origin)
			if pathErr != nil || !supported || path == "" {
				continue
			}
			if _, statErr := os.Lstat(path); statErr == nil || !os.IsNotExist(statErr) {
				// Existing files, symlinks, and paths whose identity cannot be
				// established are all fail-closed. The adapter is never asked
				// to rewrite them from this recovery path.
				continue
			}
			o.deferMaterialization(target.name, art.ArtifactID, origin, true, false, true)
		}
	}
}
