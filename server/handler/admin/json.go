package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

var errMalformedJSON = errors.New("malformed JSON")

func decodeStrictJSON(reader io.Reader, limit int64, target any) error {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return fmt.Errorf("read JSON request: %w", err)
	}
	if int64(len(raw)) > limit || !utf8.Valid(raw) {
		return errMalformedJSON
	}
	if err := rejectDuplicateJSONFields(raw); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%w: %w", errMalformedJSON, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errMalformedJSON
	}
	return nil
}

func rejectDuplicateJSONFields(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := consumeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errMalformedJSON
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("%w: %w", errMalformedJSON, err)
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		return consumeJSONObject(decoder)
	case '[':
		return consumeJSONArray(decoder)
	default:
		return errMalformedJSON
	}
}

func consumeJSONObject(decoder *json.Decoder) error {
	fields := make(map[string]struct{})
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return errMalformedJSON
		}
		name, ok := token.(string)
		if !ok {
			return errMalformedJSON
		}
		if _, exists := fields[name]; exists {
			return errMalformedJSON
		}
		fields[name] = struct{}{}
		if err := consumeJSONValue(decoder); err != nil {
			return err
		}
	}
	token, err := decoder.Token()
	if err != nil || token != json.Delim('}') {
		return errMalformedJSON
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
	if err != nil || token != json.Delim(']') {
		return errMalformedJSON
	}
	return nil
}
