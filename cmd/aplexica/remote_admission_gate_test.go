package main

import (
	"testing"
	"time"
)

func TestRemoteAdmissionGateBacksOffAfterFailure(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	var gate remoteAdmissionGate
	if !gate.begin(now) {
		t.Fatal("first admission attempt was blocked")
	}
	if gate.begin(now) {
		t.Fatal("concurrent admission attempt was allowed")
	}
	gate.finish(now, false)
	if gate.begin(now.Add(remoteAdmissionRetryBackoff - time.Nanosecond)) {
		t.Fatal("admission attempt was allowed before backoff elapsed")
	}
	if !gate.begin(now.Add(remoteAdmissionRetryBackoff)) {
		t.Fatal("admission attempt remained blocked after backoff elapsed")
	}
}

func TestRemoteAdmissionGateSuccessReopensImmediately(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	var gate remoteAdmissionGate
	if !gate.begin(now) {
		t.Fatal("first admission attempt was blocked")
	}
	gate.finish(now, true)
	if !gate.begin(now) {
		t.Fatal("successful admission did not reopen gate")
	}
}
