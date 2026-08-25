package proxy

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/aplexica/aplexica/internal/plugin/proto"
	"github.com/aplexica/aplexica/internal/syncrules"
)

// TestRemoteProxy_RulesUpdateNotification feeds a raw remote.rules_update
// JSON-RPC NOTIFICATION frame (no id) through the proxy's read pump and
// asserts the OnRulesUpdate callback fires with the change_id and the
// parsed syncrules.Rule. This is the daemon-side path that applies a
// cloud-pushed routing ruleset live.
func TestRemoteProxy_RulesUpdateNotification(t *testing.T) {
	// plugin (host) side writes -> proxy reads. We only need one
	// direction for a notification, so wire a single pipe.
	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pw.Close()
		_ = pr.Close()
	})

	// Build a RemoteProxy by hand (bypassing the initialize handshake,
	// same technique as TestRemoteProxy_CtxCancelInterruptsCall) and
	// start its read pump.
	p := &RemoteProxy{
		fr:         proto.NewFrameReader(pr),
		fw:         proto.NewFrameWriter(io.Discard),
		pending:    map[int64]chan proto.Response{},
		readDoneCh: make(chan struct{}),
	}
	go p.readPump()

	var (
		mu          sync.Mutex
		gotChangeID string
		gotRules    []syncrules.Rule
		fired       = make(chan struct{}, 1)
	)
	p.OnRulesUpdate(func(changeID string, rules []syncrules.Rule) {
		mu.Lock()
		gotChangeID = changeID
		gotRules = rules
		mu.Unlock()
		select {
		case fired <- struct{}{}:
		default:
		}
	})

	// A minimal valid rule. The wire shape mirrors syncrules.Rule's JSON
	// encoding: top-level Go-field keys (Name/Match/Route/Assign/Mode)
	// with nested camelCase keys (match.type, route.agents, …) — exactly
	// what json.Unmarshal([]syncrules.Rule) accepts and what the local
	// rules REST API + the cloud relay both emit.
	frame := []byte(`{"jsonrpc":"2.0","method":"remote.rules_update","params":{"change_id":"c1","rules":[{"Name":"only-claude","Match":{"type":["memory"]},"Route":{"agents":["claude"]},"Mode":"live"}]}}`)

	fw := proto.NewFrameWriter(pw)
	if err := fw.Write(frame); err != nil {
		t.Fatalf("write notification frame: %v", err)
	}

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("OnRulesUpdate did not fire within 2s")
	}

	mu.Lock()
	defer mu.Unlock()
	if gotChangeID != "c1" {
		t.Errorf("changeID = %q, want %q", gotChangeID, "c1")
	}
	if len(gotRules) != 1 {
		t.Fatalf("len(rules) = %d, want 1 (rules=%+v)", len(gotRules), gotRules)
	}
	r := gotRules[0]
	if r.Name != "only-claude" {
		t.Errorf("rule.Name = %q, want %q", r.Name, "only-claude")
	}
	if len(r.Match.Type) != 1 || r.Match.Type[0] != "memory" {
		t.Errorf("rule.Match.Type = %v, want [memory]", r.Match.Type)
	}
	if len(r.Route.Agents) != 1 || r.Route.Agents[0] != "claude" {
		t.Errorf("rule.Route.Agents = %v, want [claude]", r.Route.Agents)
	}
	if r.Mode != "live" {
		t.Errorf("rule.Mode = %q, want %q", r.Mode, "live")
	}

	// The parsed rule must validate + build into an Engine via the same
	// constructor the daemon's OnRulesUpdate handler uses, proving the
	// pushed shape is engine-ready.
	if _, err := syncrules.New(gotRules); err != nil {
		t.Errorf("syncrules.New(pushed rules) = %v, want nil", err)
	}
}
