package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// canonicalPayload preserves absolute-task numeric lexemes across HTTP ingress.
// Legacy requests retain the previous any/float64 normalization and hash.
func canonicalPayload(raw json.RawMessage, preserveNumbers bool) (string, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if preserveNumbers {
		decoder.UseNumber()
	}
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", err
	}
	if preserveNumbers && value == nil {
		return "", errors.New("invalid 'payload'")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return "", errors.New("invalid 'payload'")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
