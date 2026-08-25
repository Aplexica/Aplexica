package adapter

import "github.com/aplexica/aplexica/internal/acf"

// RefreshMainBranchHead returns the current main-branch parent hash for art,
// repairing stale artifact head metadata from the event-log tail when needed.
// A baseline is a virtual chain reset: its AlignedHead, not the baseline
// wrapper's own Hash, is authoritative for the next append.
func RefreshMainBranchHead(store *acf.Store, kind acf.Kind, art *acf.Artifact) (string, error) {
	if art == nil {
		return "", nil
	}
	bookkeeping := art.HeadEventHash
	if art.BranchHeads != nil && art.BranchHeads[acf.MainBranch] != "" {
		bookkeeping = art.BranchHeads[acf.MainBranch]
	}

	last, found, err := store.LastEvent(kind, art.ArtifactID)
	if err != nil {
		return "", err
	}
	if !found {
		return bookkeeping, nil
	}
	if last.Branch != "" && last.Branch != acf.MainBranch {
		// The append-order tail belongs to a side branch. Existing main-branch
		// bookkeeping remains authoritative; legacy empty metadata needs the
		// bounded backward branch lookup.
		if bookkeeping != "" {
			return bookkeeping, nil
		}
		return store.HeadHashByBranch(kind, art.ArtifactID, acf.MainBranch)
	}

	head := last.Hash
	if last.Type == acf.EventTypeBaseline {
		head = last.AlignedHead
	}
	if bookkeeping != head {
		art.HeadEventHash = head
		if art.BranchHeads == nil {
			art.BranchHeads = map[string]string{}
		}
		art.BranchHeads[acf.MainBranch] = head
	}
	return head, nil
}
