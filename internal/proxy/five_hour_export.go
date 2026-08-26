package proxy

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/local/claude-relay/internal/store"
)

func (s *Server) exportFiveHourObservations(w http.ResponseWriter, r *http.Request) {
	if err := s.accounting.Flush(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to persist pending observations")
		return
	}
	windows, err := s.store.AllFiveHourWindows(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query five-hour windows")
		return
	}
	events, err := s.store.AllFiveHourEvents(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query five-hour events")
		return
	}
	prices, err := s.store.ModelPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query model prices")
		return
	}

	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	exportedAt := time.Now().UTC()
	manifest := map[string]any{
		"schema_version": 1,
		"exported_at":    exportedAt.Format(time.RFC3339Nano),
		"windows":        len(windows),
		"events":         len(events),
		"time_zone":      "UTC",
		"privacy":        "Contains account aliases and billing metadata only; no credentials, prompts, or responses.",
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to encode export manifest")
		return
	}
	if err := writeZipFile(archive, "manifest.json", manifestJSON); err != nil ||
		writeWindowsCSV(archive, windows) != nil || writeEventsCSV(archive, events) != nil ||
		writeModelPricesCSV(archive, prices) != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to build five-hour export")
		return
	}
	if err := archive.Close(); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to finalize five-hour export")
		return
	}

	filename := "claude-relay-five-hour-" + exportedAt.Format("20060102-150405") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(output.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(output.Bytes())
}

func writeZipFile(archive *zip.Writer, name string, content []byte) error {
	file, err := archive.Create(name)
	if err != nil {
		return err
	}
	_, err = file.Write(content)
	return err
}

func writeZipCSV(archive *zip.Writer, name string, header []string, rows [][]string) error {
	var content bytes.Buffer
	writer := csv.NewWriter(&content)
	if err := writer.Write(header); err != nil {
		return err
	}
	writer.WriteAll(rows)
	writer.Flush()
	if err := writer.Error(); err != nil {
		return err
	}
	return writeZipFile(archive, name, content.Bytes())
}

func writeWindowsCSV(archive *zip.Writer, windows []store.FiveHourWindow) error {
	rows := make([][]string, 0, len(windows))
	for _, window := range windows {
		resetSeconds, _ := strconv.ParseInt(window.ResetsAt, 10, 64)
		rows = append(rows, []string{
			window.Account, window.ResetsAt, utcSeconds(resetSeconds),
			strconv.FormatInt(window.FirstObservedAt, 10), utcMillis(window.FirstObservedAt),
			strconv.FormatInt(window.LastObservedAt, 10), utcMillis(window.LastObservedAt),
			formatFloat(window.FirstUsedPercent), formatFloat(window.LastUsedPercent), formatFloat(window.MaxUsedPercent),
			strconv.FormatInt(window.ExhaustedAt, 10), utcMillis(window.ExhaustedAt), window.ExhaustionReason,
		})
	}
	return writeZipCSV(archive, "windows.csv", []string{
		"account", "resets_at_epoch_s", "resets_at_utc", "first_observed_at_epoch_ms", "first_observed_at_utc",
		"last_observed_at_epoch_ms", "last_observed_at_utc", "first_used_percent", "last_used_percent",
		"max_used_percent", "exhausted_at_epoch_ms", "exhausted_at_utc", "exhaustion_reason",
	}, rows)
}

func writeEventsCSV(archive *zip.Writer, events []store.FiveHourEvent) error {
	rows := make([][]string, 0, len(events))
	for _, event := range events {
		usage := event.Usage
		rows = append(rows, []string{
			event.EventKey, event.Account, event.ResetsAt, utcReset(event.ResetsAt), event.Kind,
			strconv.FormatInt(event.ObservedAt, 10), utcMillis(event.ObservedAt),
			strconv.FormatInt(event.CompletedAt, 10), utcMillis(event.CompletedAt), event.Model,
			strconv.Itoa(event.Status), formatFloat(event.UsedPercent), strconv.FormatBool(event.UsageSeen),
			strconv.FormatBool(event.Complete), strconv.FormatInt(usage.InputTokens, 10),
			strconv.FormatInt(usage.OutputTokens, 10), strconv.FormatInt(usage.CacheCreation5mTokens, 10),
			strconv.FormatInt(usage.CacheCreation1hTokens, 10), strconv.FormatInt(usage.CacheReadTokens, 10),
		})
	}
	return writeZipCSV(archive, "events.csv", []string{
		"event_key", "account", "resets_at_epoch_s", "resets_at_utc", "kind", "observed_at_epoch_ms",
		"observed_at_utc", "completed_at_epoch_ms", "completed_at_utc", "model", "status", "used_percent",
		"usage_seen", "complete", "input_tokens", "output_tokens", "cache_creation_5m_tokens",
		"cache_creation_1h_tokens", "cache_read_tokens",
	}, rows)
}

func writeModelPricesCSV(archive *zip.Writer, prices []store.ModelPrice) error {
	rows := make([][]string, 0, len(prices))
	for _, price := range prices {
		rows = append(rows, []string{
			price.ModelPattern, strconv.FormatInt(price.EffectiveFrom, 10), utcSeconds(price.EffectiveFrom),
			formatFloat(price.InputUSDPerMTok), formatFloat(price.OutputUSDPerMTok),
			formatFloat(price.CacheCreation5mUSDPerMTok), formatFloat(price.CacheCreation1hUSDPerMTok),
			formatFloat(price.CacheReadUSDPerMTok), price.Source,
		})
	}
	return writeZipCSV(archive, "model_prices.csv", []string{
		"model_pattern", "effective_from_epoch_s", "effective_from_utc", "input_usd_per_mtok",
		"output_usd_per_mtok", "cache_creation_5m_usd_per_mtok", "cache_creation_1h_usd_per_mtok",
		"cache_read_usd_per_mtok", "source",
	}, rows)
}

func utcReset(raw string) string {
	seconds, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return ""
	}
	return utcSeconds(seconds)
}

func utcSeconds(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339Nano)
}

func utcMillis(milliseconds int64) string {
	if milliseconds <= 0 {
		return ""
	}
	return time.UnixMilli(milliseconds).UTC().Format(time.RFC3339Nano)
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func (s *Server) clearFiveHourObservations(w http.ResponseWriter, r *http.Request) {
	if err := s.accounting.ClearFiveHourObservations(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", fmt.Sprintf("failed to clear five-hour observations: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}
