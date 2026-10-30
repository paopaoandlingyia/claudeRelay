package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTrafficObservationCountsAndPreservesResponses(t *testing.T) {
	for _, encoding := range []string{"identity", "gzip", "broken-gzip"} {
		t.Run(encoding, func(t *testing.T) {
			plain := []byte(strings.Repeat("event: message_stop\ndata: {}\n\n", 200))
			encoded := plain
			if encoding != "identity" {
				encoded = gzipFixture(t, plain)
			}
			if encoding == "broken-gzip" {
				encoded = []byte("invalid gzip header")
			}
			requestBody := []byte(`{"model":"test","messages":[{"role":"user","content":"private-text"}]}`)
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Accept-Encoding") != "gzip" {
					t.Error("missing gzip negotiation")
				}
				got, _ := io.ReadAll(r.Body)
				if !bytes.Equal(got, requestBody) {
					t.Error("diagnostics changed request body")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if encoding != "identity" {
					w.Header().Set("Content-Encoding", "gzip")
				}
				_, _ = w.Write(encoded)
			}))
			defer upstream.Close()
			server := newTestServer(t, upstream.URL, 1<<20)
			observation, err := newTrafficObservation(time.Now().Add(time.Hour).Format(time.RFC3339))
			if err != nil {
				t.Fatal(err)
			}
			server.traffic = observation
			account, _, err := server.store.AccountByAlias(t.Context(), "default")
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				response, err := server.doUpstream(req, requestBody, account)
				if err != nil {
					t.Fatal(err)
				}
				got, readErr := io.ReadAll(response.Body)
				if encoding == "broken-gzip" {
					if readErr == nil {
						t.Error("invalid gzip was accepted")
					}
				} else if readErr != nil || !bytes.Equal(got, plain) {
					t.Fatalf("response changed: size=%d err=%v", len(got), readErr)
				}
				if encoding != "identity" && (response.Header.Get("Content-Encoding") != "" || !response.Uncompressed || response.ContentLength != -1) {
					t.Error("gzip response metadata not normalized")
				}
				_ = response.Body.Close()
			}
			if calls != 2 || len(observation.groups) != 1 {
				t.Fatalf("calls=%d groups=%d", calls, len(observation.groups))
			}
			for _, counts := range observation.groups {
				if counts["attempts"] != 2 || counts["response_encoded_bytes"] != 2*int64(len(encoded)) || counts["upload_body_read_bytes"] != 2*int64(len(requestBody)) {
					t.Fatalf("incorrect body totals: %v", counts)
				}
				if encoding == "broken-gzip" {
					if counts["response_read_errors"] != 2 || counts["response_eof"] != 0 {
						t.Fatalf("incorrect error totals: %v", counts)
					}
				} else {
					if counts["response_decoded_bytes"] != 2*int64(len(plain)) || counts["response_eof"] != 2 {
						t.Fatalf("incorrect decoded totals: %v", counts)
					}
					if counts["connections_reused"] < 1 || counts["connections"] != 2 {
						t.Fatalf("connections not reused: %v", counts)
					}
				}
				wantGzip := int64(2)
				if encoding == "identity" {
					wantGzip = 0
				}
				if counts["response_gzip"] != wantGzip {
					t.Fatalf("incorrect gzip totals: %v", counts)
				}
			}
		})
	}
}

func TestTrafficContentPartitionAndRecentImages(t *testing.T) {
	o, err := newTrafficObservation(time.Now().Add(time.Hour).Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	image := base64.StdEncoding.EncodeToString([]byte("private-image-bytes"))
	body := []byte(`{"model":"test","system":"secret-system","tools":[{"name":"x"}],"messages":[{"content":[{"type":"text","text":"secret-prompt"},{"type":"tool_use","input":{"type":"image","source":{"type":"base64","data":"not-a-real-image"}}},{"type":"tool_result","content":[{"type":"text","text":"tool-output"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + image + `"}}]},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}},{"type":"thinking","thinking":"private-thinking"}]}]}`)
	a := &trafficAttempt{counts: make(map[string]int64)}
	o.describeBody(body, 1, a)
	counts := a.counts
	var sum int64
	for _, key := range []string{"system_bytes", "tool_definitions_bytes", "message_text_bytes", "tool_result_text_bytes", "tool_call_bytes", "thinking_bytes", "image_json_bytes", "document_json_bytes", "other_json_bytes"} {
		if counts[key] <= 0 {
			t.Errorf("missing category %s", key)
		}
		sum += counts[key]
	}
	if sum != int64(len(body)) {
		t.Fatalf("partition=%d body=%d", sum, len(body))
	}
	if counts["images"] != 1 || counts["image_binary_bytes"] != int64(len("private-image-bytes")) || counts["repeated_images"] != 0 {
		t.Fatalf("image totals=%v", counts)
	}
	for _, tc := range []struct {
		account  int64
		repeated bool
	}{{1, true}, {2, false}} {
		_, repeated := o.repeatedImage(image, tc.account)
		if repeated != tc.repeated {
			t.Errorf("account=%d repeated=%v", tc.account, repeated)
		}
	}
	o.mu.Lock()
	for key := range o.images {
		o.images[key] = time.Now().Add(-2 * time.Hour)
	}
	o.mu.Unlock()
	if _, repeated := o.repeatedImage(image, 1); repeated {
		t.Error("expired fingerprint matched")
	}
	sampled := make(map[string]int64)
	before := append([]byte(nil), body...)
	sampleContentCompression(body, sampled)
	if sampled["compression_samples"] != 1 || sampled["sample_image_marginal_bytes"] <= 0 || sampled["sample_document_marginal_bytes"] <= 0 {
		t.Fatalf("sample=%v", sampled)
	}
	if !bytes.Equal(before, body) {
		t.Error("sampling mutated original body")
	}
	serialized, _ := json.Marshal(counts)
	if bytes.Contains(serialized, []byte("private")) || bytes.Contains(serialized, []byte(image)) {
		t.Error("content escaped into counters")
	}
}

func TestTrafficDeadlineAndTransportFailure(t *testing.T) {
	for _, deadline := range []string{"", time.Now().Add(-time.Hour).Format(time.RFC3339)} {
		o, err := newTrafficObservation(deadline)
		if err != nil || o != nil {
			t.Fatalf("expired/disabled observation: %v %v", o, err)
		}
	}
	if _, err := newTrafficObservation("invalid"); err == nil {
		t.Fatal("accepted invalid deadline")
	}
	server := newTestServer(t, "https://upstream.invalid", 1<<20)
	o, _ := newTrafficObservation(time.Now().Add(time.Hour).Format(time.RFC3339))
	server.traffic = o
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := []byte(`{"model":"test","messages":[]}`)
	wire, _ := http.NewRequest(http.MethodPost, "https://upstream.invalid/v1/messages", bytes.NewReader(body))
	_, a := server.observeTraffic(wire, body, account)
	_, _ = a.response(nil, io.ErrUnexpectedEOF)
	a.finish() // A failure must not be counted twice.
	for _, counts := range o.groups {
		if counts["attempts"] != 1 || counts["transport_errors"] != 1 {
			t.Fatal(counts)
		}
	}
	o.until = time.Now().Add(-time.Hour)
	wire, _ = http.NewRequestWithContext(req.Context(), http.MethodPost, "https://upstream.invalid/v1/messages", bytes.NewReader(body))
	_, a = server.observeTraffic(wire, body, account)
	if a != nil || wire.Header.Get("Accept-Encoding") != "" {
		t.Fatal("expired diagnostics changed request")
	}
}

// Ensure the response byte counter does not mistake decompressed SSE for wire bytes.
var _ io.ReadCloser = (*trafficResponseBody)(nil)

func TestTrafficGzipStreamCanBeReadBeforeCompletion(t *testing.T) {
	first := []byte("event: message_start\ndata: {}\n\n")
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/event-stream")
		writer := gzip.NewWriter(w)
		_, _ = writer.Write(first)
		_ = writer.Flush()
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_ = writer.Close()
	}))
	defer upstream.Close()
	defer close(release)
	server := newTestServer(t, upstream.URL, 1<<20)
	server.traffic, _ = newTrafficObservation(time.Now().Add(time.Hour).Format(time.RFC3339))
	account, _, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	incoming := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	response, err := server.doUpstream(incoming, []byte(`{"model":"test","messages":[]}`), account)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(first))
	if _, err := io.ReadFull(response.Body, got); err != nil || !bytes.Equal(first, got) {
		t.Fatalf("first event not delivered: %q %v", got, err)
	}
	// Closing early must neither wait for EOF nor count the partial stream as complete.
	_ = response.Body.Close()
	for _, counts := range server.traffic.groups {
		if counts["response_eof"] != 0 || counts["response_decoded_bytes"] != int64(len(first)) {
			t.Fatalf("partial stream totals=%v", counts)
		}
	}
}
