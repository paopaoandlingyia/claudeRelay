package proxy

import (
	"archive/zip"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func TestFiveHourExportContainsAnalysisFilesWithoutAccountSecrets(t *testing.T) {
	server := newTestServer(t, "https://upstream.invalid", 4096)
	account, found, err := server.store.AccountByAlias(t.Context(), "default")
	if err != nil || !found {
		t.Fatalf("account found=%v err=%v", found, err)
	}
	now := time.Now()
	event := store.FiveHourEvent{
		EventKey: "request-export", AccountID: account.ID, ResetsAt: "1786386600",
		Kind: store.FiveHourEventMessages, ObservedAt: now.UnixMilli(), CompletedAt: now.Add(time.Second).UnixMilli(),
		Model: "claude-sonnet-5", Status: 200, UsedPercent: 31,
		Usage:     store.UsageCounters{InputTokens: 10, CacheCreation5mTokens: 20, CacheCreation1hTokens: 30, CacheReadTokens: 40, OutputTokens: 50, Requests: 1},
		UsageSeen: true, Complete: true,
	}
	if err := server.store.AddFiveHourEvents(t.Context(), []store.FiveHourEvent{event}); err != nil {
		t.Fatal(err)
	}
	recorder := adminRequest(t, server, http.MethodGet, "/admin/v1/usage/five-hour/export", "")
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("status=%d headers=%v body=%s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
	archive, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	contents := make(map[string]string)
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		contents[file.Name] = string(raw)
	}
	for _, name := range []string{"manifest.json", "windows.csv", "events.csv", "model_prices.csv"} {
		if _, ok := contents[name]; !ok {
			t.Errorf("export is missing %s", name)
		}
	}
	eventsCSV := contents["events.csv"]
	if !strings.Contains(eventsCSV, "request-export") || !strings.Contains(eventsCSV, "cache_creation_1h_tokens") {
		t.Fatalf("events.csv=%s", eventsCSV)
	}
	for _, secret := range []string{account.Email, account.AccountUUID, account.AccessToken} {
		if secret != "" && strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("export leaked account secret %q", secret)
		}
	}
}
