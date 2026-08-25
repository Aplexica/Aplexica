package main

import (
	"github.com/aplexica/aplexica/internal/daemon"
	"github.com/aplexica/aplexica/internal/plugin/proto"
)

func observeRemoteSyncCount(runner *daemon.RemoteRunner, metric, sourceIdentity string) {
	if runner == nil {
		return
	}
	runner.ObserveSyncV1Async(metric, 1, proto.RemoteSyncObservationUnitCount, sourceIdentity)
}

func remoteInboundAckHasDisposition(ack proto.RemoteInboundAckV2, disposition string) bool {
	for _, outcome := range ack.Outcomes {
		if outcome.Disposition == disposition {
			return true
		}
	}
	return false
}
