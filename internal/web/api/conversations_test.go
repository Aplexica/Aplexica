package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeConversationsAccessor struct {
	gotQuery ConversationSearchQuery
	resp     ConversationSearchResponse
	err      error
}

func (f *fakeConversationsAccessor) SearchConversations(q ConversationSearchQuery) (ConversationSearchResponse, error) {
	f.gotQuery = q
	return f.resp, f.err
}

func TestConversationsSearch(t *testing.T) {
	acc := &fakeConversationsAccessor{
		resp: ConversationSearchResponse{Conversations: []ConversationSummary{{
			ArtifactID:  "c1",
			Title:       "What is the luna size?",
			SourceAgent: "claude-code",
			UpdatedAt:   time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC),
			TurnCount:   2,
		}}},
	}
	h := NewConversationsHandler(acc)
	req := httptest.NewRequest(http.MethodGet, "/api/conversations?q=luna&limit=10", nil)
	rr := httptest.NewRecorder()

	h.Search(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
	if acc.gotQuery.Query != "luna" || acc.gotQuery.Limit != 10 {
		t.Fatalf("query = %+v", acc.gotQuery)
	}
	var got ConversationSearchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Conversations) != 1 || got.Conversations[0].Title != "What is the luna size?" {
		t.Fatalf("response = %+v", got)
	}
}

func TestConversationsSearch_InvalidLimit(t *testing.T) {
	h := NewConversationsHandler(&fakeConversationsAccessor{})
	req := httptest.NewRequest(http.MethodGet, "/api/conversations?limit=nope", nil)
	rr := httptest.NewRecorder()

	h.Search(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; body=%s", rr.Code, rr.Body.String())
	}
}
