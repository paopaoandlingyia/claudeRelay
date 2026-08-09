package accounting

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestStreamingObserverPreservesBytesAndCollectsCumulativeUsage(t *testing.T) {
	raw := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-sonnet-5","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":40,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20}}}}` + "\n\n" +
		"event: message_delta\n" + `data: {"type":"message_delta","usage":{"output_tokens":12}}` + "\n\n" +
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	observer := NewObserver(&chunkReader{raw: []byte(raw), size: 7}, "text/event-stream; charset=utf-8")
	forwarded, err := io.ReadAll(observer)
	if err != nil {
		t.Fatal(err)
	}
	if string(forwarded) != raw {
		t.Fatal("observer changed the forwarded SSE bytes")
	}
	usage, model := observer.Result(nil, "fallback")
	if model != "claude-sonnet-5" || !usage.Complete || usage.InputTokens != 100 || usage.OutputTokens != 12 ||
		usage.CacheReadTokens != 40 || usage.CacheCreation5mTokens != 10 || usage.CacheCreation1hTokens != 20 {
		t.Fatalf("observed model=%q usage=%+v", model, usage)
	}
}

func TestNonStreamingObserverReadsLegacyCacheUsage(t *testing.T) {
	raw := `{"id":"m","model":"claude-haiku-4-5","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":30,"output_tokens":4,"cache_creation_input_tokens":8,"cache_read_input_tokens":6}}`
	observer := NewObserver(strings.NewReader(raw), "application/json")
	if _, err := io.ReadAll(observer); err != nil {
		t.Fatal(err)
	}
	usage, model := observer.Result(nil, "fallback")
	if model != "claude-haiku-4-5" || !usage.Complete || usage.CacheCreation5mTokens != 8 || usage.CacheReadTokens != 6 {
		t.Fatalf("observed model=%q usage=%+v", model, usage)
	}
}

func TestInterruptedStreamKeepsPartialUsageButMarksIncomplete(t *testing.T) {
	raw := `data: {"type":"message_start","message":{"model":"m","usage":{"input_tokens":5}}}` + "\n\n"
	observer := NewObserver(strings.NewReader(raw), "text/event-stream")
	_, _ = io.ReadAll(observer)
	usage, _ := observer.Result(errors.New("downstream closed"), "fallback")
	if !usage.Seen || usage.Complete || usage.InputTokens != 5 {
		t.Fatalf("usage=%+v", usage)
	}
}

type chunkReader struct {
	raw  []byte
	size int
}

var benchmarkSSE = func() []byte {
	var raw bytes.Buffer
	raw.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"m\",\"usage\":{\"input_tokens\":100}}}\n\n")
	for range 10_000 {
		raw.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"fragment\"}}\n\n")
	}
	raw.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":10000}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return raw.Bytes()
}()

func BenchmarkSSEObserver(b *testing.B) {
	b.SetBytes(int64(len(benchmarkSSE)))
	for range b.N {
		observer := NewObserver(bytes.NewReader(benchmarkSSE), "text/event-stream")
		_, _ = io.Copy(io.Discard, observer)
		_, _ = observer.Result(nil, "m")
	}
}

func BenchmarkPlainCopy(b *testing.B) {
	b.SetBytes(int64(len(benchmarkSSE)))
	for range b.N {
		_, _ = io.Copy(io.Discard, bytes.NewReader(benchmarkSSE))
	}
}

func (r *chunkReader) Read(buffer []byte) (int, error) {
	if len(r.raw) == 0 {
		return 0, io.EOF
	}
	n := r.size
	if n > len(buffer) {
		n = len(buffer)
	}
	if n > len(r.raw) {
		n = len(r.raw)
	}
	copy(buffer, r.raw[:n])
	r.raw = r.raw[n:]
	return n, nil
}
