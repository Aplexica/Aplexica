package proxy

import (
	"context"

	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func (p *RemoteProxy) ExchangeAuthorityEndorsementsV1(ctx context.Context, params proto.RemoteExchangeAuthorityEndorsementsV1Params) (proto.RemoteExchangeAuthorityEndorsementsV1Result, error) {
	var result proto.RemoteExchangeAuthorityEndorsementsV1Result
	err := p.call(ctx, proto.MethodRemoteExchangeAuthorityEndorsementsV1, params, &result)
	return result, err
}
