package acf

// Branch is a summary record of one conversation branch as observed by
// walking the ConversationEvent log. Used by tooling that wants to display
// conversation topology (CLI list, future tray indicator, etc.).
type Branch struct {
	ID                string
	ParentBranchID    string
	ForkedFromEventID string
	MergedIntoMain    bool
}

// ReplayBranches walks the canonical event stream and returns the set of
// branches discovered. "main" is always present as the implicit root branch
// (even when no events name it). Fork events introduce child branches;
// merge events mark a branch as MergedIntoMain when its ID appears in the
// merge event's MergedBranchIDs list.
//
// O(N) over events; allocates O(B) where B is the number of distinct
// branches observed.
func ReplayBranches(events []ConversationEvent) []Branch {
	known := map[string]*Branch{
		"main": {ID: "main"},
	}
	for _, e := range events {
		bid := e.BranchID
		if bid == "" {
			bid = "main"
		}
		if _, ok := known[bid]; !ok {
			known[bid] = &Branch{ID: bid}
		}
		switch e.Type {
		case EventTypeFork:
			if e.BranchID == "" {
				continue // malformed; skip
			}
			b := known[e.BranchID]
			b.ParentBranchID = "main"
			b.ForkedFromEventID = e.SourceEventID
		case EventTypeMerge:
			for _, merged := range e.MergedBranchIDs {
				if _, ok := known[merged]; !ok {
					known[merged] = &Branch{ID: merged}
				}
				known[merged].MergedIntoMain = true
			}
		}
	}
	out := make([]Branch, 0, len(known))
	for _, b := range known {
		out = append(out, *b)
	}
	return out
}
