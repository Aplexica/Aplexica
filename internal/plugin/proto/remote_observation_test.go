package proto

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func observationTestKey(value byte) []byte { return bytesOf(value, 32) }

func bytesOf(value byte, count int) []byte {
	out := make([]byte, count)
	for index := range out {
		out[index] = value
	}
	return out
}

func TestRemoteSyncObservationV1ClosedWireAndPrivateDeterministicIdentity(t *testing.T) {
	key := observationTestKey(0x5a)
	params, err := NewRemoteSyncObservationV1(
		key, RemoteSyncMetricOldestOutboxAgeSeconds, 12.5, RemoteSyncObservationUnitSeconds, "local-cadence-bucket",
	)
	require.NoError(t, err)
	require.NoError(t, params.Validate())
	require.Len(t, params.SampleID, 64)
	require.NotContains(t, params.SampleID, "local-cadence-bucket")

	again, err := NewRemoteSyncObservationV1(
		key, RemoteSyncMetricOldestOutboxAgeSeconds, 12.5, RemoteSyncObservationUnitSeconds, "local-cadence-bucket",
	)
	require.NoError(t, err)
	require.Equal(t, params.SampleID, again.SampleID)

	changedValue, err := NewRemoteSyncObservationV1(
		key, RemoteSyncMetricOldestOutboxAgeSeconds, 13, RemoteSyncObservationUnitSeconds, "local-cadence-bucket",
	)
	require.NoError(t, err)
	require.NotEqual(t, params.SampleID, changedValue.SampleID, "sample identity must bind canonical numeric value")

	changedKey, err := NewRemoteSyncObservationV1(
		observationTestKey(0x6b), RemoteSyncMetricOldestOutboxAgeSeconds, 12.5, RemoteSyncObservationUnitSeconds, "local-cadence-bucket",
	)
	require.NoError(t, err)
	require.NotEqual(t, params.SampleID, changedKey.SampleID, "private per-device key prevents dictionary correlation")

	raw, err := json.Marshal(params)
	require.NoError(t, err)
	require.JSONEq(t, `{"schema_version":1,"sample_id":"`+params.SampleID+`","metric":"OldestOutboxAgeSeconds","value":12.5,"unit":"Seconds"}`, string(raw))
	require.NotContains(t, string(raw), "local-cadence-bucket")
}

func TestRemoteSyncObservationV1CanonicalSampleIDVectors(t *testing.T) {
	key := observationTestKey(0x42)
	vectors := []struct {
		name           string
		metric         string
		value          float64
		unit           string
		sourceIdentity string
		want           string
	}{
		{
			name: "integral bytes", metric: RemoteSyncMetricAvoidedPerTurnCheckpointBytes, value: 4096,
			unit: RemoteSyncObservationUnitBytes, sourceIdentity: "checkpoint:turn-0001",
			want: "67b14f6481de90e0c5b6ab6de2de608364c77c3a37fe72831da3fca20911dd8e",
		},
		{
			name: "fractional milliseconds", metric: RemoteSyncMetricHintToFetchLatencyMS, value: 12.5,
			unit: RemoteSyncObservationUnitMilliseconds, sourceIdentity: "hint:cursor-0001",
			want: "b944ebd60cc9079c91c160b0f8b4f92859994b474e24c91e6c77635eb32622f8",
		},
		{
			name: "canonical zero", metric: RemoteSyncMetricFetchToCanonicalLatencyMS, value: 0,
			unit: RemoteSyncObservationUnitMilliseconds, sourceIdentity: "fetch:event-0001",
			want: "7d69f7432b39d7a2671a833a23aa7f9b9925a79562d5054431c1be7974e6cc66",
		},
		{
			name: "count source one", metric: RemoteSyncMetricDuplicateDelivery, value: 1,
			unit: RemoteSyncObservationUnitCount, sourceIdentity: "delivery:0001",
			want: "346feb1075cde3d47a24dceb0786820b795260731ecde62f02448663e244f3c3",
		},
		{
			name: "count source two", metric: RemoteSyncMetricDuplicateDelivery, value: 1,
			unit: RemoteSyncObservationUnitCount, sourceIdentity: "delivery:0002",
			want: "cb2e74f9cfc7647b60edd1fc6f23e3eb52e3dd61fbf14b93d75efbe49f87aa30",
		},
		{
			name: "gauge value sixty", metric: RemoteSyncMetricOldestOutboxAgeSeconds, value: 60,
			unit: RemoteSyncObservationUnitSeconds, sourceIdentity: "outbox:bucket-0001",
			want: "ddc439f276ccf0a20e1c40bd16adaf539d18f94dcf41929e219b822fd711ca76",
		},
		{
			name: "gauge value sixty one", metric: RemoteSyncMetricOldestOutboxAgeSeconds, value: 61,
			unit: RemoteSyncObservationUnitSeconds, sourceIdentity: "outbox:bucket-0001",
			want: "7c2148eabaf20f8208270b78a17cc15cc91e53a75c794684971b58b6a19344d1",
		},
	}
	for _, vector := range vectors {
		t.Run(vector.name, func(t *testing.T) {
			got, err := RemoteSyncObservationSampleID(key, vector.metric, vector.value, vector.unit, vector.sourceIdentity)
			require.NoError(t, err)
			require.Equal(t, vector.want, got)
		})
	}

	positiveZero, err := RemoteSyncObservationSampleID(
		key, RemoteSyncMetricFetchToCanonicalLatencyMS, 0, RemoteSyncObservationUnitMilliseconds, "fetch:event-0001",
	)
	require.NoError(t, err)
	negativeZero, err := RemoteSyncObservationSampleID(
		key, RemoteSyncMetricFetchToCanonicalLatencyMS, math.Copysign(0, -1), RemoteSyncObservationUnitMilliseconds, "fetch:event-0001",
	)
	require.NoError(t, err)
	require.Equal(t, positiveZero, negativeZero, "negative zero must canonicalize to the same sample identity")
	require.NotEqual(t, vectors[3].want, vectors[4].want, "Count=1 observations must bind distinct source identities")
	require.NotEqual(t, vectors[5].want, vectors[6].want, "a gauge value change must produce a distinct sample identity")
}

func TestRemoteSyncObservationV1ValidationMatrix(t *testing.T) {
	key := observationTestKey(1)
	valid := []struct {
		metric string
		value  float64
		unit   string
	}{
		{RemoteSyncMetricAvoidedPerTurnCheckpointBytes, 42, RemoteSyncObservationUnitBytes},
		{RemoteSyncMetricHintToFetchLatencyMS, 1.25, RemoteSyncObservationUnitMilliseconds},
		{RemoteSyncMetricFetchToCanonicalLatencyMS, 2.5, RemoteSyncObservationUnitMilliseconds},
		{RemoteSyncMetricOldestOutboxAgeSeconds, 0, RemoteSyncObservationUnitSeconds},
		{RemoteSyncMetricUnfillableGap, 1, RemoteSyncObservationUnitCount},
		{RemoteSyncMetricCheckpointRestoreFailure, 1, RemoteSyncObservationUnitCount},
		{RemoteSyncMetricDuplicateDelivery, 1, RemoteSyncObservationUnitCount},
		{RemoteSyncMetricReorderedDelivery, 1, RemoteSyncObservationUnitCount},
		{RemoteSyncMetricQuarantine, 1, RemoteSyncObservationUnitCount},
	}
	for _, test := range valid {
		params, err := NewRemoteSyncObservationV1(key, test.metric, test.value, test.unit, "source:"+test.metric)
		require.NoError(t, err, test.metric)
		require.NoError(t, params.Validate(), test.metric)
	}

	invalid := []struct {
		name   string
		metric string
		value  float64
		unit   string
		key    []byte
	}{
		{"unknown metric", "Freeform", 1, RemoteSyncObservationUnitCount, key},
		{"wrong unit", RemoteSyncMetricQuarantine, 1, RemoteSyncObservationUnitSeconds, key},
		{"count not one", RemoteSyncMetricQuarantine, 2, RemoteSyncObservationUnitCount, key},
		{"fractional bytes", RemoteSyncMetricAvoidedPerTurnCheckpointBytes, 1.5, RemoteSyncObservationUnitBytes, key},
		{"negative", RemoteSyncMetricOldestOutboxAgeSeconds, -1, RemoteSyncObservationUnitSeconds, key},
		{"too large", RemoteSyncMetricOldestOutboxAgeSeconds, RemoteSyncObservationMaxValue + 1, RemoteSyncObservationUnitSeconds, key},
		{"nan", RemoteSyncMetricOldestOutboxAgeSeconds, math.NaN(), RemoteSyncObservationUnitSeconds, key},
		{"infinite", RemoteSyncMetricOldestOutboxAgeSeconds, math.Inf(1), RemoteSyncObservationUnitSeconds, key},
		{"zero key", RemoteSyncMetricQuarantine, 1, RemoteSyncObservationUnitCount, make([]byte, 32)},
		{"short key", RemoteSyncMetricQuarantine, 1, RemoteSyncObservationUnitCount, make([]byte, 31)},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRemoteSyncObservationV1(test.key, test.metric, test.value, test.unit, "source")
			require.ErrorIs(t, err, ErrRemoteSyncObservationInvalid)
		})
	}
}

func TestRemoteSyncObservationV1StrictJSONRejectsUnknownMissingAndDuplicateMembers(t *testing.T) {
	params, err := NewRemoteSyncObservationV1(observationTestKey(2), RemoteSyncMetricQuarantine, 1, RemoteSyncObservationUnitCount, "delivery")
	require.NoError(t, err)
	valid, err := json.Marshal(params)
	require.NoError(t, err)

	var decoded RemoteSyncObservationV1Params
	require.NoError(t, json.Unmarshal(valid, &decoded))
	require.Equal(t, params, decoded)

	for name, raw := range map[string]string{
		"unknown":   strings.TrimSuffix(string(valid), "}") + `,"label":"content"}`,
		"missing":   `{"schema_version":1,"sample_id":"` + params.SampleID + `","metric":"Quarantine","unit":"Count"}`,
		"duplicate": strings.TrimSuffix(string(valid), "}") + `,"metric":"Quarantine"}`,
		"trailing":  string(valid) + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			require.Error(t, json.Unmarshal([]byte(raw), &decoded))
		})
	}

	var result RemoteSyncObservationV1Result
	require.NoError(t, json.Unmarshal([]byte(`{"accepted":false}`), &result))
	require.False(t, result.Accepted)
	for _, raw := range []string{`{}`, `{"accepted":true,"reason":"x"}`, `{"accepted":true,"accepted":false}`} {
		require.Error(t, json.Unmarshal([]byte(raw), &result))
	}
}
