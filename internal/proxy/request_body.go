package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// requestBody keeps the original bytes for unchanged forwarding. Fingerprinting
// reads root before tool normalization; routing reads it afterwards. Per-account
// attribution copies only the maps it changes so retries share the same base.
type requestBody struct {
	raw     []byte
	root    map[string]any
	changed bool
}

func parseRequestBody(raw []byte) (*requestBody, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode request body: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode request body: trailing JSON value")
	}
	if root == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	return &requestBody{raw: raw, root: root}, nil
}

func (b *requestBody) bytes() ([]byte, error) {
	if !b.changed {
		return b.raw, nil
	}
	return json.Marshal(b.root)
}
