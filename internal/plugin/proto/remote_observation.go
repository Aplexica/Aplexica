package proto

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
)

const (
	MethodRemoteObserveSyncV1 = "remote.observe_sync_v1"

	RemoteSyncObservationSchemaV1 uint16  = 1
	RemoteSyncObservationMaxValue float64 = 1_000_000_000_000_000

	RemoteSyncMetricAvoidedPerTurnCheckpointBytes = "AvoidedPerTurnCheckpointBytes"
	RemoteSyncMetricHintToFetchLatencyMS          = "HintToFetchLatencyMs"
	RemoteSyncMetricFetchToCanonicalLatencyMS     = "FetchToCanonicalLatencyMs"
	RemoteSyncMetricOldestOutboxAgeSeconds        = "OldestOutboxAgeSeconds"
	RemoteSyncMetricUnfillableGap                 = "UnfillableGap"
	RemoteSyncMetricCheckpointRestoreFailure      = "CheckpointRestoreFailure"
	RemoteSyncMetricDuplicateDelivery             = "DuplicateDelivery"
	RemoteSyncMetricReorderedDelivery             = "ReorderedDelivery"
	RemoteSyncMetricQuarantine                    = "Quarantine"

	RemoteSyncObservationUnitCount        = "Count"
	RemoteSyncObservationUnitBytes        = "Bytes"
	RemoteSyncObservationUnitMilliseconds = "Milliseconds"
	RemoteSyncObservationUnitSeconds      = "Seconds"

	remoteSyncObservationSourceIdentityMaxBytes = 4096
	remoteSyncObservationFloatBitSize           = 64
)

var ErrRemoteSyncObservationInvalid = errors.New("plugin/proto: invalid durable sync observation")

// RemoteSyncObservationV1Params is the complete content-free daemon-to-plugin
// observation wire shape. It deliberately has no arbitrary labels, routing
// identifiers, or human-readable text. The plugin binds the authenticated
// device and account when it uploads the sample.
type RemoteSyncObservationV1Params struct {
	SchemaVersion uint16  `json:"schema_version"`
	SampleID      string  `json:"sample_id"`
	Metric        string  `json:"metric"`
	Value         float64 `json:"value"`
	Unit          string  `json:"unit"`
}

// RemoteSyncObservationV1Result is intentionally a single-bit receipt. True
// means the plugin has accepted responsibility for durable upload; false lets
// the daemon's bounded asynchronous queue retry without blocking sync.
type RemoteSyncObservationV1Result struct {
	Accepted bool `json:"accepted"`
}

func (p RemoteSyncObservationV1Params) Validate() error {
	if p.SchemaVersion != RemoteSyncObservationSchemaV1 || !validRemoteObservationSampleID(p.SampleID) ||
		remoteSyncObservationMetricUnit(p.Metric) != p.Unit || math.IsNaN(p.Value) || math.IsInf(p.Value, 0) ||
		p.Value < 0 || p.Value > RemoteSyncObservationMaxValue {
		return ErrRemoteSyncObservationInvalid
	}
	if p.Unit == RemoteSyncObservationUnitCount && p.Value != 1 {
		return ErrRemoteSyncObservationInvalid
	}
	if p.Unit == RemoteSyncObservationUnitBytes && math.Trunc(p.Value) != p.Value {
		return ErrRemoteSyncObservationInvalid
	}
	return nil
}

func remoteSyncObservationMetricUnit(metric string) string {
	switch metric {
	case RemoteSyncMetricAvoidedPerTurnCheckpointBytes:
		return RemoteSyncObservationUnitBytes
	case RemoteSyncMetricHintToFetchLatencyMS, RemoteSyncMetricFetchToCanonicalLatencyMS:
		return RemoteSyncObservationUnitMilliseconds
	case RemoteSyncMetricOldestOutboxAgeSeconds:
		return RemoteSyncObservationUnitSeconds
	case RemoteSyncMetricUnfillableGap, RemoteSyncMetricCheckpointRestoreFailure,
		RemoteSyncMetricDuplicateDelivery, RemoteSyncMetricReorderedDelivery, RemoteSyncMetricQuarantine:
		return RemoteSyncObservationUnitCount
	default:
		return ""
	}
}

func validRemoteObservationSampleID(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

// RemoteSyncObservationSampleID HMACs a daemon-local source identity under a
// versioned, metric/value-specific domain. sourceIdentity is never copied into
// the result or onto the wire. It must identify one logical observation (for
// example a delivery ID or a fixed cadence bucket), not artifact content.
func RemoteSyncObservationSampleID(sampleKey []byte, metric string, value float64, unit, sourceIdentity string) (string, error) {
	if len(sampleKey) != sha256.Size || remoteSyncObservationMetricUnit(metric) != unit || sourceIdentity == "" || len(sourceIdentity) > remoteSyncObservationSourceIdentityMaxBytes ||
		math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > RemoteSyncObservationMaxValue {
		return "", ErrRemoteSyncObservationInvalid
	}
	if unit == RemoteSyncObservationUnitCount && value != 1 || unit == RemoteSyncObservationUnitBytes && math.Trunc(value) != value {
		return "", ErrRemoteSyncObservationInvalid
	}
	var keyNonzero byte
	for _, value := range sampleKey {
		keyNonzero |= value
	}
	if keyNonzero == 0 {
		return "", ErrRemoteSyncObservationInvalid
	}
	if value == 0 {
		value = math.Abs(value) // canonicalize negative zero exactly as the cloud claim digest does
	}
	canonicalValue := strconv.FormatFloat(value, 'g', -1, remoteSyncObservationFloatBitSize)
	mac := hmac.New(sha256.New, sampleKey)
	_, _ = mac.Write([]byte("aplexica/durable-sync-observation/v1\x00" + metric + "\x00" + canonicalValue + "\x00" + unit + "\x00" + sourceIdentity))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// NewRemoteSyncObservationV1 constructs and validates one exact wire sample.
// Only the hash of sourceIdentity is retained.
func NewRemoteSyncObservationV1(sampleKey []byte, metric string, value float64, unit, sourceIdentity string) (RemoteSyncObservationV1Params, error) {
	sampleID, err := RemoteSyncObservationSampleID(sampleKey, metric, value, unit, sourceIdentity)
	if err != nil {
		return RemoteSyncObservationV1Params{}, err
	}
	params := RemoteSyncObservationV1Params{
		SchemaVersion: RemoteSyncObservationSchemaV1,
		SampleID:      sampleID,
		Metric:        metric,
		Value:         value,
		Unit:          unit,
	}
	if err := params.Validate(); err != nil {
		return RemoteSyncObservationV1Params{}, err
	}
	return params, nil
}

// UnmarshalJSON rejects additive labels and missing members. This protocol is
// intentionally closed: accepting arbitrary fields would turn it into a
// content/metadata exfiltration channel.
func (p *RemoteSyncObservationV1Params) UnmarshalJSON(input []byte) error {
	fields, err := decodeExactRemoteObservationObject(input, []string{"schema_version", "sample_id", "metric", "value", "unit"})
	if err != nil {
		return ErrRemoteSyncObservationInvalid
	}
	var decoded RemoteSyncObservationV1Params
	if err := json.Unmarshal(fields["schema_version"], &decoded.SchemaVersion); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteSyncObservationInvalid, err)
	}
	if err := json.Unmarshal(fields["sample_id"], &decoded.SampleID); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteSyncObservationInvalid, err)
	}
	if err := json.Unmarshal(fields["metric"], &decoded.Metric); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteSyncObservationInvalid, err)
	}
	if err := json.Unmarshal(fields["value"], &decoded.Value); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteSyncObservationInvalid, err)
	}
	if err := json.Unmarshal(fields["unit"], &decoded.Unit); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteSyncObservationInvalid, err)
	}
	*p = decoded
	return p.Validate()
}

func (r *RemoteSyncObservationV1Result) UnmarshalJSON(input []byte) error {
	fields, err := decodeExactRemoteObservationObject(input, []string{"accepted"})
	if err != nil {
		return ErrRemoteSyncObservationInvalid
	}
	var decoded RemoteSyncObservationV1Result
	if err := json.Unmarshal(fields["accepted"], &decoded.Accepted); err != nil {
		return fmt.Errorf("%w: %v", ErrRemoteSyncObservationInvalid, err)
	}
	*r = decoded
	return nil
}

func decodeExactRemoteObservationObject(input []byte, expected []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(input))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return nil, ErrRemoteSyncObservationInvalid
	}
	fields := make(map[string]json.RawMessage, len(expected))
	for decoder.More() {
		nameToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := nameToken.(string)
		if !ok {
			return nil, ErrRemoteSyncObservationInvalid
		}
		if _, duplicate := fields[name]; duplicate {
			return nil, ErrRemoteSyncObservationInvalid
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		fields[name] = raw
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return nil, ErrRemoteSyncObservationInvalid
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrRemoteSyncObservationInvalid
	}
	if len(fields) != len(expected) {
		return nil, ErrRemoteSyncObservationInvalid
	}
	for _, name := range expected {
		if _, ok := fields[name]; !ok {
			return nil, ErrRemoteSyncObservationInvalid
		}
	}
	return fields, nil
}
