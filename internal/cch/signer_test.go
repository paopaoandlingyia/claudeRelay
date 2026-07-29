package cch

import (
	"bytes"
	"regexp"
	"testing"

	xxhash64 "github.com/pierrec/xxHash/xxHash64"
)

func TestSignExistingPreservesEveryOtherByte(t *testing.T) {
	t.Parallel()
	body := []byte("{\n" +
		`  "messages": [{"role":"user","content":"你好; literal cch=12345; stays"}],` + "\n" +
		`  "system": [{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=claude-desktop; cch=abcde;"}]` + "\n" +
		"}")

	signed, info, err := SignExisting(body)
	if err != nil {
		t.Fatalf("SignExisting() error = %v", err)
	}
	if !info.Found || !info.Changed {
		t.Fatalf("info = %+v", info)
	}
	if !bytes.Contains(signed, []byte(`"content":"你好; literal cch=12345; stays"`)) {
		t.Fatal("message content changed")
	}

	pattern := regexp.MustCompile(`(x-anthropic-billing-header:[^\"]*cch=)([0-9a-f]{5})(;)`)
	match := pattern.FindSubmatch(signed)
	if match == nil {
		t.Fatalf("signed billing block not found: %s", signed)
	}
	unsigned := pattern.ReplaceAll(signed, []byte(`${1}00000${3}`))
	want := xxhash64.Checksum(unsigned, seed) & 0xFFFFF
	got := parseFiveHex(t, match[2])
	if got != want {
		t.Fatalf("cch = %05x, want %05x", got, want)
	}

	resigned, secondInfo, err := SignExisting(signed)
	if err != nil {
		t.Fatalf("second SignExisting() error = %v", err)
	}
	if secondInfo.Changed || !bytes.Equal(resigned, signed) {
		t.Fatal("signing is not idempotent")
	}
}

func TestSignExistingLeavesOrdinaryRequestUntouched(t *testing.T) {
	t.Parallel()
	body := []byte(`{"system":[{"type":"text","text":"Be concise."}],"messages":[]}`)
	got, info, err := SignExisting(body)
	if err != nil {
		t.Fatalf("SignExisting() error = %v", err)
	}
	if info.Found || info.Changed || !bytes.Equal(got, body) {
		t.Fatalf("ordinary request changed, info = %+v", info)
	}
}

func TestSignExistingRejectsMalformedBillingCCH(t *testing.T) {
	t.Parallel()
	body := []byte(`{"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.215.574; cc_entrypoint=cli; cch=xyz;"}]}`)
	if _, info, err := SignExisting(body); err == nil || !info.Found {
		t.Fatalf("SignExisting() error = %v, info = %+v", err, info)
	}
}

func parseFiveHex(t *testing.T, raw []byte) uint64 {
	t.Helper()
	var value uint64
	for _, char := range raw {
		value <<= 4
		switch {
		case char >= '0' && char <= '9':
			value |= uint64(char - '0')
		case char >= 'a' && char <= 'f':
			value |= uint64(char-'a') + 10
		default:
			t.Fatalf("invalid hex %q", raw)
		}
	}
	return value
}
