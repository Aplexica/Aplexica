package web_test

// Integration test for the W4 API surface. Lives in package web_test
// (rather than web) so it can import internal/web/api without a
// cycle.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/conflicts"
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/onboarding"
	"github.com/aplexica/aplexica/internal/pending"
	"github.com/aplexica/aplexica/internal/syncrules"
	"github.com/aplexica/aplexica/internal/transport"
	"github.com/aplexica/aplexica/internal/web"
	apiweb "github.com/aplexica/aplexica/internal/web/api"
)

type secureLoopbackTransport struct {
	base http.RoundTripper
}

func (t secureLoopbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	url := *req.URL
	url.Scheme = "http"
	clone.URL = &url
	return t.base.RoundTrip(clone)
}

func secureLoopbackClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
	return &http.Client{
		Jar:       jar,
		Transport: secureLoopbackTransport{base: transport},
	}
}

func secureLoopbackOrigin(origin string) string {
	return "https://" + strings.TrimPrefix(origin, "http://")
}

// integDaemonAcc covers DaemonAccessor + AgentsAccessor + Onboarding.
// Onboarding has a State() method; rather than collide with Daemon's
// State() (returning string), we split them into dedicated stub types.
type integDaemonAcc struct{}

func (integDaemonAcc) Version() string      { return "v0.0.0-integ" }
func (integDaemonAcc) PID() int             { return 42 }
func (integDaemonAcc) WatchedDir() string   { return "/tmp" }
func (integDaemonAcc) Paused() bool         { return false }
func (integDaemonAcc) StartedAt() time.Time { return time.Now().Add(-time.Hour) }
func (integDaemonAcc) State() string        { return "idle" }
func (integDaemonAcc) PendingImports() int  { return 0 }
func (integDaemonAcc) Pause() error         { return nil }
func (integDaemonAcc) Resume() error        { return nil }

type integAgentsAcc struct{}

func (integAgentsAcc) List() []apiweb.AgentSummary { return []apiweb.AgentSummary{} }
func (integAgentsAcc) Get(_ string) (apiweb.AgentDetail, bool) {
	return apiweb.AgentDetail{}, false
}

type integEventsAcc struct{}

func (integEventsAcc) Backfill(_ apiweb.EventQuery) (apiweb.EventPage, error) {
	return apiweb.EventPage{Events: []apiweb.EventRecord{}}, nil
}

type integRulesAcc struct{}

func (integRulesAcc) List() ([]syncrules.Rule, error)            { return nil, nil }
func (integRulesAcc) Get(_ string) (syncrules.Rule, bool, error) { return syncrules.Rule{}, false, nil }
func (integRulesAcc) Add(_ syncrules.Rule) error                 { return nil }
func (integRulesAcc) Update(_ string, _ syncrules.Rule) error    { return apiweb.ErrRuleNotFound }
func (integRulesAcc) Delete(_ string) error                      { return apiweb.ErrRuleNotFound }

type integConflictsAcc struct{}

func (integConflictsAcc) List() ([]conflicts.Conflict, error) { return nil, nil }
func (integConflictsAcc) Get(_ string) (conflicts.Conflict, bool, error) {
	return conflicts.Conflict{}, false, nil
}
func (integConflictsAcc) Resolve(_, _, _ string) error { return apiweb.ErrConflictNotFound }

type integConversationBranchesAcc struct{}

type integConversationsAcc struct{}

func (integConversationsAcc) SearchConversations(q apiweb.ConversationSearchQuery) (apiweb.ConversationSearchResponse, error) {
	return apiweb.ConversationSearchResponse{Query: q.Query, Conversations: []apiweb.ConversationSummary{}}, nil
}

func (integConversationBranchesAcc) ListConversationBranches(id string) (apiweb.ConversationBranchesResponse, bool, error) {
	return apiweb.ConversationBranchesResponse{ArtifactID: id, Branches: []apiweb.ConversationBranch{}}, true, nil
}
func (integConversationBranchesAcc) ForkConversation(_ string, req apiweb.ConversationForkRequest) (apiweb.ConversationBranchMutationResponse, error) {
	return apiweb.ConversationBranchMutationResponse{Branch: req.Branch, Agent: req.TargetAgent, Operation: "fork"}, nil
}
func (integConversationBranchesAcc) CheckoutConversation(_ string, req apiweb.ConversationCheckoutRequest) (apiweb.ConversationBranchMutationResponse, error) {
	return apiweb.ConversationBranchMutationResponse{Branch: req.Branch, Agent: req.Agent, Operation: "checkout"}, nil
}

type integPendingAcc struct{}

func (integPendingAcc) List() ([]pending.Project, error) { return nil, nil }
func (integPendingAcc) Link(_, _ string) error           { return apiweb.ErrPendingNotFound }

type integConfigAcc struct{}

func (integConfigAcc) Load() (*daemon.Config, error) { return &daemon.Config{}, nil }
func (integConfigAcc) Patch(_ map[string]any) error  { return nil }
func (integConfigAcc) RawPath() string               { return "/tmp/config.json" }

type integTransportAcc struct{}

func (integTransportAcc) Get() transport.Info                   { return transport.LocalOnly }
func (integTransportAcc) Set(_ string) error                    { return nil }
func (integTransportAcc) SetBYO(_ transport.BYORelayOpts) error { return transport.ErrBYONotInOSS }

type integOnboardingAcc struct{}

func (integOnboardingAcc) State() onboarding.State {
	return onboarding.Compute(onboarding.Inputs{})
}

func TestW4_EveryGroupReachableThroughMiddleware(t *testing.T) {
	dir := t.TempDir()
	srv, err := web.NewServer(web.Options{
		Bind:        "127.0.0.1",
		Port:        0,
		PortInfoDir: dir,
		Version:     "v0.0.0-integ",
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv.UseProtected(apiweb.NewDaemonHandler(integDaemonAcc{}))
	srv.UseProtected(apiweb.NewAgentsHandler(integAgentsAcc{}))
	srv.UseProtected(apiweb.NewEventsHandler(integEventsAcc{}))
	srv.UseProtected(apiweb.NewRulesHandler(integRulesAcc{}))
	srv.UseProtected(apiweb.NewConflictsHandler(integConflictsAcc{}))
	srv.UseProtected(apiweb.NewConversationsHandler(integConversationsAcc{}))
	srv.UseProtected(apiweb.NewConversationBranchesHandler(integConversationBranchesAcc{}))
	srv.UseProtected(apiweb.NewPendingHandler(integPendingAcc{}))
	srv.UseProtected(apiweb.NewConfigHandler(integConfigAcc{}))
	srv.UseProtected(apiweb.NewTransportHandler(integTransportAcc{}))
	srv.UseProtected(apiweb.NewOnboardingHandler(integOnboardingAcc{}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()
	integWaitForPort(t, srv)

	// Authenticate end-to-end so the request flow exercises
	// RequireSession + RequireCSRF.
	client := secureLoopbackClient()
	origin := secureLoopbackOrigin(srv.Origin())
	urlOut, err := srv.IssueTokenURL()
	if err != nil {
		t.Fatalf("IssueTokenURL: %v", err)
	}
	rawToken := strings.SplitN(urlOut, "bootstrap=", 2)[1]
	body, _ := json.Marshal(map[string]string{"token": rawToken})
	bsURL := origin + "/api/auth/bootstrap"
	if resp, err := client.Post(bsURL, "application/json", bytes.NewReader(body)); err != nil {
		t.Fatalf("bootstrap: %v", err)
	} else {
		resp.Body.Close()
	}

	// One GET per group — proves the routes mount on the protected
	// mux behind RequireSession and the handlers respond happily on
	// the empty-state path.
	gets := []string{
		"/api/daemon",
		"/api/agents",
		"/api/events",
		"/api/rules",
		"/api/conflicts",
		"/api/conversations",
		"/api/conversations/demo/branches",
		"/api/pending",
		"/api/config",
		"/api/config/raw-path",
		"/api/transport",
		"/api/onboarding/state",
	}
	for _, p := range gets {
		resp, err := client.Get(origin + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		buf, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, body=%s", p, resp.StatusCode, buf)
		}
	}
}

// integWaitForPort polls until the server's listener has bound — same
// pattern as the in-package waitForPort helper.
func integWaitForPort(t *testing.T, srv *web.Server) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := srv.Port(); p != 0 {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never reported a port within 3s")
	return 0
}
