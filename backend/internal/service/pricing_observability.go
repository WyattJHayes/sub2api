package service

import (
	"context"
	"log/slog"
	"strings"
	"time"
)

const (
	pricingSourceRemote   = "remote"
	pricingSourceLocal    = "local"
	pricingSourceEmbedded = "embedded"
	pricingSourceEmpty    = "empty"
	pricingSourceUnknown  = "unknown"
)

// PricingSourceSnapshot captures the bounded runtime state needed by release
// gates and ops dashboards without exposing tenant, URL, or model labels.
type PricingSourceSnapshot struct {
	Source           string
	ModelCount       int
	LocalHash        string
	RemoteHash       string
	LastUpdated      time.Time
	LastRefreshAt    time.Time
	LastRefreshOK    bool
	LastRefreshError string
	FallbackTotal    uint64
}

type PricingSourceMetric struct {
	Name   string
	Labels map[string]string
	Value  float64
}

func (s *PricingService) SnapshotPricingSource() PricingSourceSnapshot {
	if s == nil {
		return PricingSourceSnapshot{Source: pricingSourceEmpty}
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	modelCount := len(s.pricingData)
	source := normalizePricingSource(s.source, modelCount)
	return PricingSourceSnapshot{
		Source:           source,
		ModelCount:       modelCount,
		LocalHash:        s.localHash,
		RemoteHash:       s.remoteHash,
		LastUpdated:      s.lastUpdated,
		LastRefreshAt:    s.lastRefreshAt,
		LastRefreshOK:    s.lastRefreshOK,
		LastRefreshError: s.lastRefreshError,
		FallbackTotal:    s.fallbackTotal.Load(),
	}
}

func logPricingSourceSnapshot(log *slog.Logger, now time.Time, snapshot PricingSourceSnapshot) {
	if log == nil {
		log = slog.Default()
	}
	if now.IsZero() {
		now = time.Now()
	}

	source := normalizePricingSource(snapshot.Source, snapshot.ModelCount)
	level := slog.LevelInfo
	if source == pricingSourceEmpty {
		level = slog.LevelError
	} else if !snapshot.LastRefreshOK || snapshot.FallbackTotal > 0 {
		level = slog.LevelWarn
	}

	record := slog.NewRecord(now, level, "radar_pricing_source_health", 0)
	record.AddAttrs(
		slog.String("component", "service.pricing"),
		slog.String("pricing_source", source),
		slog.Int("model_count", snapshot.ModelCount),
		slog.Int64("source_age_seconds", pricingSourceAgeSeconds(now, snapshot.LastUpdated)),
		slog.Uint64("fallback_total", snapshot.FallbackTotal),
		slog.Bool("last_refresh_ok", snapshot.LastRefreshOK),
		slog.String("last_refresh_error", snapshot.LastRefreshError),
	)
	if snapshot.LocalHash != "" {
		record.AddAttrs(slog.String("local_hash", snapshot.LocalHash))
	}
	if snapshot.RemoteHash != "" {
		record.AddAttrs(slog.String("remote_hash", snapshot.RemoteHash))
	}
	if !snapshot.LastUpdated.IsZero() {
		record.AddAttrs(slog.Time("last_updated", snapshot.LastUpdated))
	}
	if !snapshot.LastRefreshAt.IsZero() {
		record.AddAttrs(slog.Time("last_refresh_at", snapshot.LastRefreshAt))
	}
	_ = log.Handler().Handle(context.Background(), record)
}

func PricingSourceMetrics(now time.Time, snapshot PricingSourceSnapshot) []PricingSourceMetric {
	if now.IsZero() {
		now = time.Now()
	}
	source := normalizePricingSource(snapshot.Source, snapshot.ModelCount)

	lastRefreshOK := 0.0
	if snapshot.LastRefreshOK {
		lastRefreshOK = 1
	}

	return []PricingSourceMetric{
		{Name: "radar_pricing_source_current", Labels: map[string]string{"source": source}, Value: 1},
		{Name: "radar_pricing_source_age_seconds", Labels: map[string]string{}, Value: float64(pricingSourceAgeSeconds(now, snapshot.LastUpdated))},
		{Name: "radar_pricing_last_refresh_ok", Labels: map[string]string{}, Value: lastRefreshOK},
		{Name: "radar_pricing_fallback_total", Labels: map[string]string{}, Value: float64(snapshot.FallbackTotal)},
	}
}

func normalizePricingSource(source string, modelCount int) string {
	source = strings.ToLower(strings.TrimSpace(source))
	switch source {
	case pricingSourceRemote, pricingSourceLocal, pricingSourceEmbedded, pricingSourceEmpty, pricingSourceUnknown:
		return source
	case "":
		if modelCount == 0 {
			return pricingSourceEmpty
		}
		return pricingSourceUnknown
	default:
		return pricingSourceUnknown
	}
}

func pricingSourceAgeSeconds(now time.Time, lastUpdated time.Time) int64 {
	if now.IsZero() || lastUpdated.IsZero() {
		return 0
	}
	seconds := int64(now.Sub(lastUpdated).Seconds())
	if seconds < 0 {
		return 0
	}
	return seconds
}
