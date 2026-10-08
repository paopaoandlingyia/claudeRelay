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
	Account           string              `json:"account,omitempty"`
	Ingress           string              `json:"ingress,omitempty"`
	Model             string              `json:"model,omitempty"`
	Usage             store.UsageCounters `json:"usage"`
	CostUSD           float64             `json:"cost_usd"`
	Unpriced          bool                `json:"unpriced,omitempty"`
	APIValueByTypeUSD *usageAPIValue      `json:"api_value_by_type_usd,omitempty"`
}

// usageAPIValue is API-price-equivalent revenue before any downstream discount,
// not the subscription's actual cost or a prediction of its quota rules.
type usageAPIValue struct {
	Input           float64 `json:"input"`
	Output          float64 `json:"output"`
	CacheCreation5m float64 `json:"cache_creation_5m"`
	CacheCreation1h float64 `json:"cache_creation_1h"`
	CacheRead       float64 `json:"cache_read"`
}

func (v *usageAPIValue) Add(other usageAPIValue) {
	v.Input += other.Input
	v.Output += other.Output
	v.CacheCreation5m += other.CacheCreation5m
	v.CacheCreation1h += other.CacheCreation1h
	v.CacheRead += other.CacheRead
}

func (v usageAPIValue) Total() float64 {
	return v.Input + v.Output + v.CacheCreation5m + v.CacheCreation1h + v.CacheRead
}

type fiveHourDataQuality struct {
	MissingUsage    bool `json:"missing_usage"`
	IncompleteUsage bool `json:"incomplete_usage"`
	Unpriced        bool `json:"unpriced"`
	PartialStart    bool `json:"partial_start"`
	MissingQuota    bool `json:"missing_quota"`
	MixedModels     bool `json:"mixed_models"`
}

type fiveHourWindowUsage struct {
	Account                   string              `json:"account"`
	FirstObservedAt           int64               `json:"first_observed_at"`
	LastObservedAt            int64               `json:"last_observed_at"`
	ResetsAt                  int64               `json:"resets_at"`
	FirstUsedPercent          float64             `json:"first_used_percent"`
	LastUsedPercent           float64             `json:"last_used_percent"`
	MaxUsedPercent            float64             `json:"max_used_percent"`
	ExhaustedAt               int64               `json:"exhausted_at,omitempty"`
	ExhaustionReason          string              `json:"exhaustion_reason,omitempty"`
	ObservedCostUSD           float64             `json:"observed_cost_usd"`
	EventCount                int64               `json:"event_count"`
	MissingUsageCount         int64               `json:"missing_usage_count"`
	IncompleteCount           int64               `json:"incomplete_count"`
	ByModel                   []valuedUsage       `json:"by_model"`
	Unpriced                  bool                `json:"unpriced,omitempty"`
	APIValueByTypeUSD         usageAPIValue       `json:"api_value_by_type_usd"`
	ObservedUsedPercentDelta  *float64            `json:"observed_used_percent_delta,omitempty"`
	APIValueUSDPerUsedPercent *float64            `json:"api_value_usd_per_used_percent,omitempty"`
	DataQuality               fiveHourDataQuality `json:"data_quality"`
}

type usageDashboardResponse struct {
	From              int64                          `json:"from"`
	To                int64                          `json:"to"`
	Totals            valuedUsage                    `json:"totals"`
	ByModel           []valuedUsage                  `json:"by_model"`
	ByAccount         []valuedUsage                  `json:"by_account"`
	ByIngress         []valuedUsage                  `json:"by_ingress"`
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
	ingressBuckets, err := s.store.UsageIngressBuckets(r.Context(), from)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query ingress usage")
		return
	}
	prices, err := s.store.ModelPrices(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "failed to query model prices")
		return
	}
	response := buildUsageDashboard(buckets, ingressBuckets, prices, from, now.Unix())
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

func buildUsageDashboard(buckets []store.UsageBucket, ingressBuckets []store.UsageIngressBucket, prices []store.ModelPrice, from, to int64) usageDashboardResponse {
	byModel := make(map[string]*valuedUsage)
	byAccount := make(map[string]*valuedUsage)
	byIngress := make(map[string]*valuedUsage)
	unpriced := make(map[string]bool)
	response := usageDashboardResponse{From: from * 1000, To: to * 1000, ByModel: []valuedUsage{}, ByAccount: []valuedUsage{}, ByIngress: []valuedUsage{}, UnpricedModels: []string{}, FiveHourCurrent: []fiveHourWindowUsage{}, FiveHourExhausted: []fiveHourWindowUsage{}}
	response.Totals.APIValueByTypeUSD = &usageAPIValue{}
	for _, bucket := range buckets {
		price, priced := matchingPrice(prices, bucket.Model, bucket.BucketStart)
		valueByType := usageAPIValue{}
		if priced {
			valueByType = apiUsageValue(bucket.Counters, price)
		} else {
			unpriced[bucket.Model] = true
			response.Totals.Unpriced = true
		}
		cost := valueByType.Total()
		response.Totals.Usage.Add(bucket.Counters)
		response.Totals.CostUSD += cost
		response.Totals.APIValueByTypeUSD.Add(valueByType)
		model := byModel[bucket.Model]
		if model == nil {
			model = &valuedUsage{Model: bucket.Model, APIValueByTypeUSD: &usageAPIValue{}}
			byModel[bucket.Model] = model
		}
		model.Usage.Add(bucket.Counters)
		model.CostUSD += cost
		model.APIValueByTypeUSD.Add(valueByType)
		model.Unpriced = model.Unpriced || !priced
		account := byAccount[bucket.Account]
		if account == nil {
			account = &valuedUsage{Account: bucket.Account, APIValueByTypeUSD: &usageAPIValue{}}
			byAccount[bucket.Account] = account
		}
		account.Usage.Add(bucket.Counters)
		account.CostUSD += cost
		account.APIValueByTypeUSD.Add(valueByType)
		account.Unpriced = account.Unpriced || !priced
	}
	for _, bucket := range ingressBuckets {
		ingress := byIngress[bucket.Ingress]
		if ingress == nil {
			ingress = &valuedUsage{Ingress: bucket.Ingress}
			byIngress[bucket.Ingress] = ingress
		}
		ingress.Usage.Add(bucket.Counters)
	}
	for _, value := range byModel {
		response.ByModel = append(response.ByModel, *value)
	}
	for _, value := range byAccount {
		response.ByAccount = append(response.ByAccount, *value)
	}
	for _, value := range byIngress {
		response.ByIngress = append(response.ByIngress, *value)
	}
	sort.Slice(response.ByIngress, func(i, j int) bool { return response.ByIngress[i].Ingress < response.ByIngress[j].Ingress })
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

func apiUsageValue(usage store.UsageCounters, price store.ModelPrice) usageAPIValue {
	return usageAPIValue{
		Input:           float64(usage.InputTokens) * price.InputUSDPerMTok / 1_000_000,
		Output:          float64(usage.OutputTokens) * price.OutputUSDPerMTok / 1_000_000,
		CacheCreation5m: float64(usage.CacheCreation5mTokens) * price.CacheCreation5mUSDPerMTok / 1_000_000,
		CacheCreation1h: float64(usage.CacheCreation1hTokens) * price.CacheCreation1hUSDPerMTok / 1_000_000,
		CacheRead:       float64(usage.CacheReadTokens) * price.CacheReadUSDPerMTok / 1_000_000,
	}
}

func valueFiveHourWindows(windows []store.FiveHourWindow, prices []store.ModelPrice) []fiveHourWindowUsage {
	result := make([]fiveHourWindowUsage, 0, len(windows))
	for _, window := range windows {
		byModel := make(map[string]*valuedUsage)
		totalValue := usageAPIValue{}
		unpriced := false
		for _, bucket := range window.PriceBuckets {
			value := byModel[bucket.Model]
			if value == nil {
				value = &valuedUsage{Model: bucket.Model, APIValueByTypeUSD: &usageAPIValue{}}
				byModel[bucket.Model] = value
			}
			value.Usage.Add(bucket.Counters)
			if price, ok := matchingPrice(prices, bucket.Model, bucket.ObservedAtSeconds); ok {
				valueByType := apiUsageValue(bucket.Counters, price)
				value.APIValueByTypeUSD.Add(valueByType)
				value.CostUSD += valueByType.Total()
				totalValue.Add(valueByType)
			} else {
				value.Unpriced = true
				unpriced = true
			}
		}
		values := make([]valuedUsage, 0, len(byModel))
		for _, value := range byModel {
			values = append(values, *value)
		}
		sort.Slice(values, func(i, j int) bool {
			if values[i].CostUSD == values[j].CostUSD {
				return values[i].Model < values[j].Model
			}
			return values[i].CostUSD > values[j].CostUSD
		})
		quality := fiveHourDataQuality{
			MissingUsage: window.MissingUsageCount > 0, IncompleteUsage: window.IncompleteCount > 0,
			Unpriced: unpriced, PartialStart: window.FirstUsedPercent > 0,
			MissingQuota: window.FirstUsedPercent < 0 || window.MaxUsedPercent < 0,
			MixedModels:  len(values) > 1,
		}
		var delta, valuePerPercent *float64
		if !quality.MissingQuota {
			observedDelta := window.MaxUsedPercent - window.FirstUsedPercent
			delta = &observedDelta
			// Only expose the ratio for complete usage observed from zero. Missing
			// or partial usage otherwise makes the numerator misleading, and a
			// mid-window start can include already-charged first-request tokens.
			if observedDelta > 0 && window.EventCount > 0 && !quality.PartialStart &&
				!quality.MissingUsage && !quality.IncompleteUsage && !quality.Unpriced {
				ratio := totalValue.Total() / observedDelta
				valuePerPercent = &ratio
			}
		}
		reset, _ := strconv.ParseInt(window.ResetsAt, 10, 64)
		result = append(result, fiveHourWindowUsage{
			Account: window.Account, FirstObservedAt: window.FirstObservedAt,
			LastObservedAt: window.LastObservedAt, ResetsAt: reset * 1000,
			FirstUsedPercent: window.FirstUsedPercent, LastUsedPercent: window.LastUsedPercent,
			MaxUsedPercent: window.MaxUsedPercent, ExhaustedAt: window.ExhaustedAt,
			ExhaustionReason: window.ExhaustionReason, ObservedCostUSD: totalValue.Total(),
			EventCount: window.EventCount, MissingUsageCount: window.MissingUsageCount,
			IncompleteCount: window.IncompleteCount, ByModel: values, Unpriced: unpriced,
			APIValueByTypeUSD: totalValue, ObservedUsedPercentDelta: delta,
			APIValueUSDPerUsedPercent: valuePerPercent, DataQuality: quality,
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
