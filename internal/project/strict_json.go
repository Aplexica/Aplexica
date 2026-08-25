package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// decodeStrictJSON accepts exactly one JSON value, rejects duplicate object
// member names at every nesting level, and rejects fields not declared by the
// destination type. Security-sensitive registry and migration documents use
// this instead of json.Unmarshal, whose last-duplicate-wins behavior can make
// an operator approve different semantics from those a decoder consumes.
func decodeStrictJSON(data []byte, dst any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("project: JSON input is not valid UTF-8")
	}
	duplicateDecoder := json.NewDecoder(bytes.NewReader(data))
	duplicateDecoder.UseNumber()
	if err := consumeStrictJSONValue(duplicateDecoder); err != nil {
		return err
	}
	if token, err := duplicateDecoder.Token(); err != io.EOF {
		if err != nil {
			return fmt.Errorf("project: parse JSON trailer: %w", err)
		}
		return fmt.Errorf("project: multiple JSON values (unexpected %v)", token)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("project: parse JSON: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return err
	}
	return nil
}

func consumeStrictJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("project: parse JSON: %w", err)
	}
	delim, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("project: parse JSON object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("project: non-string JSON object key")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("project: duplicate JSON member name %q", key)
			}
			seen[key] = struct{}{}
			if err := consumeStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return fmt.Errorf("project: malformed JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeStrictJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return fmt.Errorf("project: malformed JSON array")
		}
	default:
		return fmt.Errorf("project: unexpected JSON delimiter %q", delim)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return fmt.Errorf("project: parse JSON trailer: %w", err)
		}
		return fmt.Errorf("project: multiple JSON values")
	}
	return nil
}
