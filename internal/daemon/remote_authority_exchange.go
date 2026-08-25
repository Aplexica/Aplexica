package daemon

import (
	"context"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func (r *RemoteRunner) ExchangeAuthorityEndorsementsV1(ctx context.Context, params proto.RemoteExchangeAuthorityEndorsementsV1Params) (proto.RemoteExchangeAuthorityEndorsementsV1Result, error) {
	r.proxyMu.Lock()
	p := r.proxy
	r.proxyMu.Unlock()
	if p == nil {
		return proto.RemoteExchangeAuthorityEndorsementsV1Result{}, ErrRemoteReconnecting
	}
	return p.ExchangeAuthorityEndorsementsV1(ctx, params)
}
