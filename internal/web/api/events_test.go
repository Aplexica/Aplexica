package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeEventsAccessor struct {
	events    []EventRecord
	lastQuery EventQuery
}

// Backfill mirrors the real accessor's newest-first contract: f.events is
// kept ascending by Seq, and the fake returns it descending, paging backward
// via the Before cursor (Before <= 0 means "from the newest").
func (f *fakeEventsAccessor) Backfill(q EventQuery) (EventPage, error) {
	f.lastQuery = q
	desc := make([]EventRecord, len(f.events))
	for i, e := range f.events {
		desc[len(f.events)-1-i] = e
	}
	from := 0
	if q.Before > 0 {
		for from < len(desc) && desc[from].Seq >= q.Before {
			from++
		}
	}
	rest := desc[from:]
	to := q.Limit
	if to > len(rest) {
		to = len(rest)
	}
	page := EventPage{Events: rest[:to]}
	if to > 0 {
		page.NextBefore = rest[to-1].Seq
	} else {
		page.NextBefore = q.Before
	}
	return page, nil
}

func TestEventsBackfill_HappyPath(t *testing.T) {
	acc := &fakeEventsAccessor{
		events: []EventRecord{
			{Seq: 1, Type: "artifact.synced", ArtifactID: "a"},
			{Seq: 2, Type: "agent.activity", ArtifactID: "b"},
			{Seq: 3, Type: "rule.fired"},
		},
	}
	h := NewEventsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=10", nil)
	rr := httptest.NewRecorder()
	h.Backfill(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var page EventPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("got %d events, want 3", len(page.Events))
	}
	// Newest-first: highest Seq leads.
	if page.Events[0].Seq != 3 {
		t.Errorf("events[0].Seq = %d, want 3 (newest first)", page.Events[0].Seq)
	}
	if page.NextBefore != 1 {
		t.Errorf("nextBefore = %d, want 1 (oldest Seq in page)", page.NextBefore)
	}
}

func TestEventsBackfill_LimitAndBefore(t *testing.T) {
	acc := &fakeEventsAccessor{
		events: []EventRecord{
			{Seq: 1}, {Seq: 2}, {Seq: 3}, {Seq: 4}, {Seq: 5},
		},
	}
	h := NewEventsHandler(acc)

	// before=4 -> events older than Seq 4, newest-first, limited to 2.
	req := httptest.NewRequest(http.MethodGet, "/api/events?before=4&limit=2", nil)
	rr := httptest.NewRecorder()
	h.Backfill(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	var page EventPage
	if err := json.Unmarshal(rr.Body.Bytes(), &page); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("got %d, want 2", len(page.Events))
	}
	if page.Events[0].Seq != 3 || page.Events[1].Seq != 2 {
		t.Errorf("seqs = [%d %d], want [3 2]", page.Events[0].Seq, page.Events[1].Seq)
	}
	if page.NextBefore != 2 {
		t.Errorf("nextBefore = %d, want 2", page.NextBefore)
	}
	if acc.lastQuery.Limit != 2 {
		t.Errorf("Limit propagated as %d, want 2", acc.lastQuery.Limit)
	}
	if acc.lastQuery.Before != 4 {
		t.Errorf("Before propagated as %d, want 4", acc.lastQuery.Before)
	}
}

func TestEventsBackfill_DefaultsLimit(t *testing.T) {
	acc := &fakeEventsAccessor{}
	h := NewEventsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	rr := httptest.NewRecorder()
	h.Backfill(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if acc.lastQuery.Limit != 100 {
		t.Errorf("default limit = %d, want 100", acc.lastQuery.Limit)
	}
}

func TestEventsBackfill_InvalidLimit(t *testing.T) {
	acc := &fakeEventsAccessor{}
	h := NewEventsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=notanumber", nil)
	rr := httptest.NewRecorder()
	h.Backfill(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "validation" {
		t.Errorf("code = %q, want validation", body.Code)
	}
}

func TestEventsBackfill_InvalidBefore(t *testing.T) {
	acc := &fakeEventsAccessor{}
	h := NewEventsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/events?before=-1", nil)
	rr := httptest.NewRecorder()
	h.Backfill(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	var body ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != "validation" {
		t.Errorf("code = %q, want validation", body.Code)
	}
}

func TestEventsBackfill_ClampsLimit(t *testing.T) {
	acc := &fakeEventsAccessor{}
	h := NewEventsHandler(acc)

	req := httptest.NewRequest(http.MethodGet, "/api/events?limit=99999", nil)
	rr := httptest.NewRecorder()
	h.Backfill(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if acc.lastQuery.Limit != 1000 {
		t.Errorf("clamped limit = %d, want 1000", acc.lastQuery.Limit)
	}
}
