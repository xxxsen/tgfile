package backupfmt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

func decodeStrictJSON(raw []byte, target any) error {
	if !utf8.Valid(raw) {
		return fmt.Errorf("JSON is not valid UTF-8: %w", errMalformedJSON)
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("read trailing JSON token %v: %w", token, err)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("read JSON token: %w", err)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		return consumeJSONObject(decoder)
	case '[':
		return consumeJSONArray(decoder)
	default:
		return fmt.Errorf("unexpected JSON delimiter %q: %w", delimiter, errMalformedJSON)
	}
}

func consumeJSONObject(decoder *json.Decoder) error {
	fields := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("read JSON object field: %w", err)
		}
		name, ok := token.(string)
		if !ok {
			return fmt.Errorf("JSON object field is not a string: %w", errMalformedJSON)
		}
		if _, exists := fields[name]; exists {
			return fmt.Errorf("duplicate JSON field %q: %w", name, errMalformedJSON)
		}
		fields[name] = struct{}{}
		if err := consumeJSONValue(decoder); err != nil {
			return err
		}
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("close JSON object: %w", err)
	}
	if token != json.Delim('}') {
		return fmt.Errorf("unexpected JSON object terminator %v: %w", token, errMalformedJSON)
	}
	return nil
}

func consumeJSONArray(decoder *json.Decoder) error {
	for decoder.More() {
		if err := consumeJSONValue(decoder); err != nil {
			return err
		}
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("close JSON array: %w", err)
	}
	if token != json.Delim(']') {
		return fmt.Errorf("unexpected JSON array terminator %v: %w", token, errMalformedJSON)
	}
	return nil
}
