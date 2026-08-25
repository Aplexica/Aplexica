package acf

import (
	"encoding/json"
	"errors"
	"fmt"
)

var (
	ErrBranchNotFound   = errors.New("acf: branch not found")
	ErrMalformedBranch  = errors.New("acf: malformed branch")
	ErrForkPointMissing = errors.New("acf: fork point missing")
	ErrRedactionBarrier = errors.New("acf: redaction barrier")
)

type BranchProjectionOpts struct {
	IncludeCompacted bool
}

func (s *Store) ProjectEventsForBranch(k Kind, artifactID string, branchID string, opts BranchProjectionOpts) ([]Event, error) {
	branchID, err := normalizeProjectionBranch(branchID)
	if err != nil {
		return nil, err
	}
	events, err := s.ReadEvents(k, artifactID)
	if err != nil {
		return nil, err
	}
	if opts.IncludeCompacted {
		events, err = s.ReadEventsIncludingCompacted(k, artifactID)
		if err != nil {
			return nil, err
		}
		return projectEventsForBranch(events, branchID, nil)
	}
	projected, err := projectEventsForBranch(events, branchID, nil)
	if err == nil {
		return projected, nil
	}
	if !errors.Is(err, ErrForkPointMissing) && !errors.Is(err, ErrBranchNotFound) {
		return nil, err
	}
	merged, merr := s.ReadEventsIncludingCompacted(k, artifactID)
	if merr != nil {
		return nil, merr
	}
	if len(merged) == len(events) {
		return nil, err
	}
	return projectEventsForBranch(merged, branchID, nil)
}

func (s *Store) ProjectConversationPayloadForBranch(
	artifactID string,
	branchID string,
	opts BranchProjectionOpts,
) (ConversationPayload, []Event, bool, error) {
	events, err := s.ProjectEventsForBranch(KindConversation, artifactID, branchID, opts)
	if err != nil {
		return ConversationPayload{}, nil, false, err
	}
	payload, ok, err := MaterializedConversationPayload(events)
	if err != nil {
		return ConversationPayload{}, events, false, err
	}
	return payload, events, ok, nil
}

func (s *Store) MaterializedConversationPayloadForBranch(
	artifactID string,
	branchID string,
) (json.RawMessage, []Event, bool, error) {
	payload, events, ok, err := s.ProjectConversationPayloadForBranch(
		artifactID,
		branchID,
		BranchProjectionOpts{},
	)
	if err != nil || !ok {
		return nil, events, ok, err
	}
	raw, err := EncodePayload(payload)
	if err != nil {
		return nil, events, false, fmt.Errorf("acf: encode branch conversation payload: %w", err)
	}
	return raw, events, true, nil
}

func (s *Store) BranchHeadEvent(k Kind, artifactID string, branchID string) (Event, bool, error) {
	events, err := s.ProjectEventsForBranch(k, artifactID, branchID, BranchProjectionOpts{})
	if err != nil {
		if errors.Is(err, ErrBranchNotFound) {
			return Event{}, false, nil
		}
		return Event{}, false, err
	}
	if len(events) == 0 {
		return Event{}, false, nil
	}
	return events[len(events)-1], true, nil
}

func projectEventsForBranch(events []Event, branchID string, visiting map[string]bool) ([]Event, error) {
	branchID, err := normalizeProjectionBranch(branchID)
	if err != nil {
		return nil, err
	}
	if visiting == nil {
		visiting = map[string]bool{}
	}
	if visiting[branchID] {
		return nil, fmt.Errorf("%w: cycle involving branch %q", ErrMalformedBranch, branchID)
	}
	visiting[branchID] = true
	defer delete(visiting, branchID)

	if branchID == MainBranch {
		out := make([]Event, 0, len(events))
		for _, e := range events {
			evBranch, err := eventBranchID(e)
			if err != nil {
				return nil, err
			}
			if evBranch == MainBranch {
				out = append(out, e)
			}
		}
		return out, nil
	}

	branchEvents := make([]Event, 0)
	for _, e := range events {
		evBranch, err := eventBranchID(e)
		if err != nil {
			return nil, err
		}
		if evBranch == branchID {
			branchEvents = append(branchEvents, e)
		}
	}
	if len(branchEvents) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrBranchNotFound, branchID)
	}
	// A retained checkpoint can be the first authenticated state this device
	// has ever seen for a side branch. Such a payload-bearing baseline is a
	// self-contained virtual recovery root: it deliberately needs no local fork
	// ancestry, and subsequent verbatim origin deltas extend AlignedHead. If a
	// branch already has a normal fork prefix, its first event is still the fork
	// and the established source-prefix projection below remains unchanged.
	if branchEvents[0].Type == EventTypeBaseline {
		root := branchEvents[0]
		if root.ParentHash != "" || root.AlignedHead == "" || root.AlignedEventID == "" || !HasPayload(root.Payload) {
			return nil, fmt.Errorf("%w: invalid recovery baseline on branch %q", ErrMalformedBranch, branchID)
		}
		return branchEvents, nil
	}
	fork := branchEvents[0]
	if fork.Type != EventTypeForkOuter {
		return nil, fmt.Errorf("%w: first event on branch %q is %q", ErrMalformedBranch, branchID, fork.Type)
	}
	sourceBranch, err := normalizeProjectionBranch(fork.ForkSourceBranch)
	if err != nil {
		return nil, fmt.Errorf("%w: source branch for %q: %w", ErrMalformedBranch, branchID, err)
	}
	sourceProjection, err := projectEventsForBranch(events, sourceBranch, visiting)
	if err != nil {
		return nil, err
	}
	prefix, idx := truncateThroughHash(sourceProjection, fork.ParentHash)
	if prefix == nil {
		return nil, fmt.Errorf("%w: parent %q for branch %q", ErrForkPointMissing, fork.ParentHash, branchID)
	}
	for _, e := range sourceProjection[idx+1:] {
		if e.Type == EventTypeRedaction {
			return nil, fmt.Errorf("%w: branch %q forks before redaction %s on %q",
				ErrRedactionBarrier, branchID, e.EventID, sourceBranch)
		}
	}
	out := make([]Event, 0, len(prefix)+len(branchEvents))
	out = append(out, prefix...)
	out = append(out, branchEvents...)
	return out, nil
}

func truncateThroughHash(events []Event, hash string) ([]Event, int) {
	if hash == "" {
		return nil, -1
	}
	for i, e := range events {
		if e.Hash == hash {
			out := make([]Event, i+1)
			copy(out, events[:i+1])
			return out, i
		}
	}
	return nil, -1
}

func normalizeProjectionBranch(branchID string) (string, error) {
	if branchID == "" {
		branchID = MainBranch
	}
	return NormalizeBranchName(branchID)
}

func eventBranchID(e Event) (string, error) {
	return normalizeProjectionBranch(e.Branch)
}
