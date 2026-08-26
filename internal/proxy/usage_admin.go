package proxy

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/local/claude-relay/internal/store"
)

type valuedUsage struct {
	Account  string              `json:"account,omitempty"`
	Model    string              `json:"model,omitempty"`
	Usage    store.UsageCounters `json:"usage"`
	CostUSD  float64             `json:"cost_usd"`
	Unpriced bool                `json:"unpriced,omitempty"`
}

type fiveHourWindowUsage struct {
	Account           string        `json:"account"`
	FirstObservedAt   int64         `json:"first_observed_at"`
	LastObservedAt    int64         `json:"last_observed_at"`
	ResetsAt          int64         `json:"resets_at"`
	FirstUsedPercent  float64       `json:"first_used_percent"`
	LastUsedPercent   float64       `json:"last_used_percent"`
	MaxUsedPercent    float64       `json:"max_used_percent"`
	ExhaustedAt       int64         `json:"exhausted_at,omitempty"`
	ExhaustionReason  string        `json:"exhaustion_reason,omitempty"`
	ObservedCostUSD   float64       `json:"observed_cost_usd"`
	EventCount        int64         `json:"event_count"`
	MissingUsageCount int64         `json:"missing_usage_count"`
	IncompleteCount   int64         `json:"incomplete_count"`
	ByModel           []valuedUsage `json:"by_model"`
	Unpriced          bool          `json:"unpriced,omitempty"`
}

type usageDashboardResponse struct {
	From              int64                          `json:"from"`
	To                int64                          `json:"to"`
	Totals            valuedUsage                    `json:"totals"`
	ByModel           []valuedUsage                  `json:"by_model"`
	ByAccount         []valuedUsage                  `json:"by_account"`
	UnpricedModels    []string                       `json:"unpriced_models"`
	FiveHourCurrent   []fiveHourWindowUsage          `json:"five_hour_current"`
	FiveHourExhausted []fiveHourWindowUsage          `json:"five_hour_exhausted"`
	FiveHourStats     store.FiveHourObservationStats `json:"five_hour_stats"`
}

func (s *Server) usageDashboard(w http.ResponseWriter, r *http.Request) {
	if err := s.accounting.Flush(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to persist pending usage")
		return
	}
	now := time.Now()
	from := now.Add(-7 * 24 * time.Hour).Unix()
	if raw := strings.TrimSpace(r.URL.Query().Get("from")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "from must be an epoch timestamp in milliseconds")
			return
		}
		from = parsed / 1000
	}
	buckets, err := s.store.UsageBuckets(r.Context(), from)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query usage")
		return
	}
	prices, err := s.store.ModelPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query model prices")
		return
	}
	response := buildUsageDashboard(buckets, prices, from, now.Unix())
	current, err := s.store.FiveHourWindows(r.Context(), false, now.UnixMilli(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query current five-hour windows")
		return
	}
	exhausted, err := s.store.FiveHourWindows(r.Context(), true, now.UnixMilli(), 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query exhausted five-hour windows")
		return
	}
	stats, err := s.store.FiveHourObservationStats(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query five-hour observation statistics")
		return
	}
	response.FiveHourCurrent = valueFiveHourWindows(current, prices)
	response.FiveHourExhausted = valueFiveHourWindows(exhausted, prices)
	response.FiveHourStats = stats
	writeJSON(w, http.StatusOK, response)
}

func buildUsageDashboard(buckets []store.UsageBucket, prices []store.ModelPrice, from, to int64) usageDashboardResponse {
	byModel := make(map[string]*valuedUsage)
	byAccount := make(map[string]*valuedUsage)
	unpriced := make(map[string]bool)
	response := usageDashboardResponse{From: from * 1000, To: to * 1000, ByModel: []valuedUsage{}, ByAccount: []valuedUsage{}, UnpricedModels: []string{}, FiveHourCurrent: []fiveHourWindowUsage{}, FiveHourExhausted: []fiveHourWindowUsage{}}
	for _, bucket := range buckets {
		price, priced := matchingPrice(prices, bucket.Model, bucket.BucketStart)
		cost := 0.0
		if priced {
			cost = usageCost(bucket.Counters, price)
		} else {
			unpriced[bucket.Model] = true
			response.Totals.Unpriced = true
		}
		response.Totals.Usage.Add(bucket.Counters)
		response.Totals.CostUSD += cost
		model := byModel[bucket.Model]
		if model == nil {
			model = &valuedUsage{Model: bucket.Model}
			byModel[bucket.Model] = model
		}
		model.Usage.Add(bucket.Counters)
		model.CostUSD += cost
		model.Unpriced = model.Unpriced || !priced
		account := byAccount[bucket.Account]
		if account == nil {
			account = &valuedUsage{Account: bucket.Account}
			byAccount[bucket.Account] = account
		}
		account.Usage.Add(bucket.Counters)
		account.CostUSD += cost
		account.Unpriced = account.Unpriced || !priced
	}
	for _, value := range byModel {
		response.ByModel = append(response.ByModel, *value)
	}
	for _, value := range byAccount {
		response.ByAccount = append(response.ByAccount, *value)
	}
	for model := range unpriced {
		response.UnpricedModels = append(response.UnpricedModels, model)
	}
	return response
}

func matchingPrice(prices []store.ModelPrice, model string, at int64) (store.ModelPrice, bool) {
	var best store.ModelPrice
	bestSpecificity := -1
	for _, price := range prices {
		if price.EffectiveFrom > at {
			continue
		}
		pattern := price.ModelPattern
		prefix := strings.TrimSuffix(pattern, "*")
		matches := pattern == model || (strings.HasSuffix(pattern, "*") && strings.HasPrefix(model, prefix))
		if !matches {
			continue
		}
		specificity := len(prefix)
		if pattern == model {
			specificity += 1000
		}
		if specificity > bestSpecificity || (specificity == bestSpecificity && price.EffectiveFrom > best.EffectiveFrom) {
			best, bestSpecificity = price, specificity
		}
	}
	return best, bestSpecificity >= 0
}

func usageCost(usage store.UsageCounters, price store.ModelPrice) float64 {
	return (float64(usage.InputTokens)*price.InputUSDPerMTok +
		float64(usage.OutputTokens)*price.OutputUSDPerMTok +
		float64(usage.CacheCreation5mTokens)*price.CacheCreation5mUSDPerMTok +
		float64(usage.CacheCreation1hTokens)*price.CacheCreation1hUSDPerMTok +
		float64(usage.CacheReadTokens)*price.CacheReadUSDPerMTok) / 1_000_000
}

func valueFiveHourWindows(windows []store.FiveHourWindow, prices []store.ModelPrice) []fiveHourWindowUsage {
	result := make([]fiveHourWindowUsage, 0, len(windows))
	for _, window := range windows {
		values := make([]valuedUsage, 0, len(window.ByModel))
		cost := 0.0
		unpriced := false
		for model, counters := range window.ByModel {
			value := valuedUsage{Model: model, Usage: counters}
			if price, ok := matchingPrice(prices, model, window.LastObservedAt/1000); ok {
				value.CostUSD = usageCost(counters, price)
				cost += value.CostUSD
			} else {
				value.Unpriced = true
				unpriced = true
			}
			values = append(values, value)
		}
		sort.Slice(values, func(i, j int) bool {
			if values[i].CostUSD == values[j].CostUSD {
				return values[i].Model < values[j].Model
			}
			return values[i].CostUSD > values[j].CostUSD
		})
		reset, _ := strconv.ParseInt(window.ResetsAt, 10, 64)
		result = append(result, fiveHourWindowUsage{
			Account: window.Account, FirstObservedAt: window.FirstObservedAt,
			LastObservedAt: window.LastObservedAt, ResetsAt: reset * 1000,
			FirstUsedPercent: window.FirstUsedPercent, LastUsedPercent: window.LastUsedPercent,
			MaxUsedPercent: window.MaxUsedPercent, ExhaustedAt: window.ExhaustedAt,
			ExhaustionReason: window.ExhaustionReason, ObservedCostUSD: cost,
			EventCount: window.EventCount, MissingUsageCount: window.MissingUsageCount,
			IncompleteCount: window.IncompleteCount, ByModel: values, Unpriced: unpriced,
		})
	}
	return result
}

func (s *Server) listModelPrices(w http.ResponseWriter, r *http.Request) {
	prices, err := s.store.ModelPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query model prices")
		return
	}
	views := make([]store.ModelPrice, len(prices))
	for index, price := range prices {
		views[index] = price
		views[index].EffectiveFrom *= 1000
		views[index].CreatedAt *= 1000
	}
	writeJSON(w, http.StatusOK, map[string]any{"prices": views})
}

func (s *Server) saveModelPrice(w http.ResponseWriter, r *http.Request) {
	var price store.ModelPrice
	if err := decodeAdminJSON(w, r, &price); err != nil {
		return
	}
	price.EffectiveFrom /= 1000
	saved, err := s.store.SaveModelPrice(r.Context(), price)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	saved.EffectiveFrom *= 1000
	saved.CreatedAt *= 1000
	writeJSON(w, http.StatusCreated, saved)
}

func (s *Server) clearUsageAccounting(w http.ResponseWriter, r *http.Request) {
	if err := s.accounting.Clear(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to clear usage accounting")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cleared": true})
}
