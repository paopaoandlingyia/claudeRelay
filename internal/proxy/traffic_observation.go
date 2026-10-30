package proxy

import (
	"compress/gzip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/claude-relay/internal/store"
)

// Diagnostics retain counts only. Image fingerprints are process-local, account-scoped,
// bounded, expire after an hour, and are never emitted. Deadlines survive restarts.
type trafficObservation struct {
	until      time.Time
	salt       [32]byte
	mu         sync.Mutex
	images     map[[32]byte]time.Time
	groups     map[trafficGroup]map[string]int64
	sequence   atomic.Uint64
	nextSample time.Time
}

type trafficGroup struct {
	Account, Exit, Model, Path string
	Hour                       int64
}

func newTrafficObservation(deadline string) (*trafficObservation, error) {
	if deadline == "" {
		return nil, nil
	}
	until, err := time.Parse(time.RFC3339, deadline)
	if err != nil {
		return nil, fmt.Errorf("traffic_observation_until must be RFC3339: %w", err)
	}
	if !time.Now().Before(until) {
		return nil, nil
	}
	o := &trafficObservation{until: until, images: make(map[[32]byte]time.Time), groups: make(map[trafficGroup]map[string]int64)}
	if _, err := rand.Read(o.salt[:]); err != nil {
		return nil, fmt.Errorf("initialize traffic observation: %w", err)
	}
	return o, nil
}

func (o *trafficObservation) run(ctx context.Context) {
	if o == nil {
		return
	}
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	defer o.flush()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			o.flush()
			if !now.Before(o.until) {
				o.mu.Lock()
				clear(o.images)
				o.mu.Unlock()
			}
		}
	}
}

func (o *trafficObservation) flush() {
	o.mu.Lock()
	groups := o.groups
	o.groups = make(map[trafficGroup]map[string]int64)
	o.mu.Unlock()
	for key, counts := range groups {
		slog.Info("traffic observation", "hour", key.Hour, "account", key.Account, "exit", key.Exit,
			"model", key.Model, "path", key.Path, "counts", counts)
	}
}

func (o *trafficObservation) add(key trafficGroup, counts map[string]int64) {
	o.mu.Lock()
	// Flush before admitting a new key to bound arbitrary model-name cardinality.
	for o.groups[key] == nil && len(o.groups) >= 256 {
		o.mu.Unlock()
		o.flush()
		o.mu.Lock()
	}
	if o.groups[key] == nil {
		o.groups[key] = make(map[string]int64)
	}
	for name, n := range counts {
		o.groups[key][name] += n
	}
	o.mu.Unlock()
}

type trafficAttempt struct {
	observation                                          *trafficObservation
	key                                                  trafficGroup
	counts                                               map[string]int64
	upload, download, decoded, connections, reused, idle atomic.Int64
	once                                                 sync.Once
}

type trafficCountingBody struct {
	io.ReadCloser
	count *atomic.Int64
}

func (b *trafficCountingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.count.Add(int64(n))
	return n, err
}

// Explicit gzip negotiation disables Go's automatic decoder only for observed
// requests. This lazy decoder preserves header timing and streaming behavior.
type trafficResponseBody struct {
	raw     io.ReadCloser
	reader  io.Reader
	gzip    bool
	decoder *gzip.Reader
	readErr error
	attempt *trafficAttempt
}

func (b *trafficResponseBody) Read(p []byte) (int, error) {
	if b.readErr != nil {
		return 0, b.readErr
	}
	if b.reader == nil {
		if b.gzip {
			decoder, err := gzip.NewReader(b.raw)
			if err != nil {
				b.attempt.counts["response_read_errors"] = 1
				b.readErr = err
				return 0, err
			}
			b.decoder = decoder
			b.reader = decoder
		} else {
			b.reader = b.raw
		}
	}
	n, err := b.reader.Read(p)
	b.attempt.decoded.Add(int64(n))
	if err == io.EOF {
		b.attempt.counts["response_eof"] = 1
	} else if err != nil {
		b.attempt.counts["response_read_errors"] = 1
	}
	b.readErr = err
	return n, err
}
func (b *trafficResponseBody) Close() error {
	// Closing gzip does not drain or close the original HTTP body.
	if b.decoder != nil {
		_ = b.decoder.Close()
	}
	err := b.raw.Close()
	b.attempt.finish()
	return err
}
func (a *trafficAttempt) finish() {
	a.once.Do(func() {
		a.counts["attempts"] = 1
		a.counts["upload_body_read_bytes"] = a.upload.Load()
		a.counts["response_encoded_bytes"] = a.download.Load()
		a.counts["response_decoded_bytes"] = a.decoded.Load()
		a.counts["connections"] = a.connections.Load()
		a.counts["connections_reused"] = a.reused.Load()
		a.counts["connections_idle"] = a.idle.Load()
		a.observation.add(a.key, a.counts)
	})
}

func (s *Server) observeTraffic(request *http.Request, body []byte, account store.Account) (*http.Request, *trafficAttempt) {
	o := s.traffic
	if o == nil || !time.Now().Before(o.until) {
		return request, nil
	}
	exit := "direct"
	if account.ProxyMode == "custom" {
		// ProxyDisplay removes credentials but leaves the scheme/host/port.
		exit = store.ProxyDisplay(account.ProxyURL)
	} else if account.ProxyMode == "global" && s.exits.global.Proxy != nil {
		if address, err := s.exits.global.Proxy(request); err == nil && address != nil {
			exit = address.Scheme + "://" + address.Host
		}
	}
	a := &trafficAttempt{observation: o, key: trafficGroup{Account: account.Alias, Exit: exit, Path: request.URL.Path, Hour: time.Now().UTC().Truncate(time.Hour).Unix()}, counts: make(map[string]int64)}
	a.counts["request_raw_bytes"] = int64(len(body))
	a.counts["request_encoded_bytes"] = request.ContentLength
	if request.Header.Get("Content-Encoding") == "gzip" {
		a.counts["request_gzip"] = 1
	}
	o.describeBody(body, account.ID, a)
	request.Body = &trafficCountingBody{ReadCloser: request.Body, count: &a.upload}
	if getBody := request.GetBody; getBody != nil {
		request.GetBody = func() (io.ReadCloser, error) {
			b, err := getBody()
			if err != nil {
				return nil, err
			}
			return &trafficCountingBody{ReadCloser: b, count: &a.upload}, nil
		}
	}
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		a.connections.Add(1)
		if info.Reused {
			a.reused.Add(1)
		}
		if info.WasIdle {
			a.idle.Add(1)
		}
	}}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	request.Header.Set("Accept-Encoding", "gzip")
	return request, a
}

func (a *trafficAttempt) response(response *http.Response, err error) (*http.Response, error) {
	if err != nil {
		a.counts["transport_errors"] = 1
		a.finish()
		return response, err
	}
	a.counts["status_"+strconv.Itoa(response.StatusCode)] = 1
	if response.ProtoMajor == 2 {
		a.counts["responses_http2"] = 1
	} else {
		a.counts["responses_http1"] = 1
	}
	encoded := &trafficCountingBody{ReadCloser: response.Body, count: &a.download}
	compressed := response.Header.Get("Content-Encoding") == "gzip"
	if compressed {
		a.counts["response_gzip"] = 1
		response.Header.Del("Content-Encoding")
		response.Header.Del("Content-Length")
		response.ContentLength = -1
		response.Uncompressed = true
	}
	response.Body = &trafficResponseBody{raw: encoded, gzip: compressed, attempt: a}
	return response, nil
}

func (o *trafficObservation) repeatedImage(data string, accountID int64) (int64, bool) {
	h := hmac.New(sha256.New, o.salt[:])
	var id [8]byte
	binary.LittleEndian.PutUint64(id[:], uint64(accountID))
	_, _ = h.Write(id[:])
	n, err := io.Copy(h, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil {
		return 0, false
	}
	var key [32]byte
	copy(key[:], h.Sum(nil))
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	last, found := o.images[key]
	repeated := found && now.Sub(last) < time.Hour
	if !found && len(o.images) >= 4096 {
		// TTL cleanup first; arbitrary eviction is sufficient for an approximate,
		// bounded recent-image cache, never a deduplication of actual requests.
		for k, t := range o.images {
			if now.Sub(t) >= time.Hour {
				delete(o.images, k)
			}
		}
		if len(o.images) >= 4096 {
			for k := range o.images {
				delete(o.images, k)
				break
			}
		}
	}
	if now.Before(o.until) {
		o.images[key] = now
	}
	return n, repeated
}
