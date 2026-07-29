package cch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	xxhash64 "github.com/pierrec/xxHash/xxHash64"
)

const seed uint64 = 0x6E52736AC806831E

var (
	billingPrefix = "x-anthropic-billing-header:"
	cchPattern    = regexp.MustCompile(`\bcch=([0-9a-f]{5});`)
)

type Info struct {
	Found   bool
	Changed bool
}

// SignExisting replaces the CCH in the first system billing block with a
// signature over the otherwise byte-identical request body. Bodies without a
// billing block are returned unchanged.
func SignExisting(body []byte) ([]byte, Info, error) {
	var envelope struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, Info{}, fmt.Errorf("decode Anthropic request: %w", err)
	}
	if len(envelope.System) == 0 || !bytes.HasPrefix([]byte(envelope.System[0].Text), []byte(billingPrefix)) {
		return body, Info{}, nil
	}

	billingText := envelope.System[0].Text
	matches := cchPattern.FindAllStringSubmatchIndex(billingText, -1)
	if len(matches) != 1 {
		return nil, Info{Found: true}, fmt.Errorf("first billing block must contain exactly one five-digit lowercase hexadecimal cch")
	}

	encodedBilling, err := json.Marshal(billingText)
	if err != nil {
		return nil, Info{Found: true}, fmt.Errorf("encode billing block: %w", err)
	}
	encodedOffset := bytes.Index(body, encodedBilling)
	if encodedOffset < 0 {
		return nil, Info{Found: true}, errors.New("locate billing block in original JSON bytes")
	}
	if bytes.Index(body[encodedOffset+len(encodedBilling):], encodedBilling) >= 0 {
		return nil, Info{Found: true}, errors.New("billing block is duplicated in request body")
	}

	valueStartInText := matches[0][2]
	valueEndInText := matches[0][3]
	if valueEndInText-valueStartInText != 5 {
		return nil, Info{Found: true}, errors.New("unexpected cch width")
	}
	// The billing header is ASCII and contains no JSON escapes before cch, so
	// the text offset is the encoded-string offset plus the opening quote.
	valueStart := encodedOffset + 1 + valueStartInText
	valueEnd := encodedOffset + 1 + valueEndInText
	if valueEnd > len(body) {
		return nil, Info{Found: true}, errors.New("cch offset exceeds request body")
	}

	unsigned := bytes.Clone(body)
	copy(unsigned[valueStart:valueEnd], "00000")
	signature := fmt.Sprintf("%05x", xxhash64.Checksum(unsigned, seed)&0xFFFFF)

	signed := bytes.Clone(unsigned)
	copy(signed[valueStart:valueEnd], signature)
	return signed, Info{
		Found:   true,
		Changed: !bytes.Equal(signed, body),
	}, nil
}
