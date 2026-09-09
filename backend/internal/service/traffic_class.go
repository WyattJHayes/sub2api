package service

import (
	"net/url"
	"strings"
	"unicode"
)

// TrafficClass identifies whether an observation represents billable production
// traffic or an operational/non-production request.
type TrafficClass string

const (
	TrafficClassProduction TrafficClass = "production"
	TrafficClassMetadata   TrafficClass = "metadata"
	TrafficClassSynthetic  TrafficClass = "synthetic"
	TrafficClassUnknown    TrafficClass = "unknown"
)

// TrafficClassificationInput contains only request metadata needed to classify
// a usage or error record. It intentionally excludes bodies and credentials.
type TrafficClassificationInput struct {
	RequestPath       string
	InboundEndpoint   string
	UpstreamEndpoint  string
	UserAgent         string
	IsCountTokens     bool
	ExplicitClass     string
	DefaultProduction bool
}

// NormalizeTrafficClass bounds persisted/API values to the supported set.
func NormalizeTrafficClass(value string) TrafficClass {
	switch TrafficClass(strings.ToLower(strings.TrimSpace(value))) {
	case TrafficClassProduction:
		return TrafficClassProduction
	case TrafficClassMetadata:
		return TrafficClassMetadata
	case TrafficClassSynthetic:
		return TrafficClassSynthetic
	case TrafficClassUnknown:
		return TrafficClassUnknown
	default:
		return TrafficClassUnknown
	}
}

// ClassifyTraffic applies explicit evidence in a stable order. Metadata and
// synthetic signals take precedence over the production fallback so probes can
// never inflate SLA denominators even when they use a normal API endpoint.
func ClassifyTraffic(input TrafficClassificationInput) TrafficClass {
	if explicitRaw := strings.TrimSpace(input.ExplicitClass); explicitRaw != "" {
		// An explicit value is authoritative, including `unknown`. Invalid
		// values are deliberately retained as unknown instead of silently
		// becoming billable production traffic.
		return NormalizeTrafficClass(explicitRaw)
	}

	if input.IsCountTokens || isMetadataEndpoint(input.RequestPath) || isMetadataEndpoint(input.InboundEndpoint) || isMetadataEndpoint(input.UpstreamEndpoint) {
		return TrafficClassMetadata
	}

	if isSyntheticUserAgent(input.UserAgent) {
		return TrafficClassSynthetic
	}

	if input.DefaultProduction || hasRequestEndpoint(input) {
		return TrafficClassProduction
	}

	return TrafficClassUnknown
}

func hasRequestEndpoint(input TrafficClassificationInput) bool {
	return strings.TrimSpace(input.RequestPath) != "" ||
		strings.TrimSpace(input.InboundEndpoint) != "" ||
		strings.TrimSpace(input.UpstreamEndpoint) != ""
}

func normalizeEndpointPath(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Path != "" {
		value = parsed.Path
	} else if idx := strings.IndexAny(value, "?#"); idx >= 0 {
		value = value[:idx]
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	value = strings.TrimRight(value, "/")
	if value == "" {
		return "/"
	}
	return strings.ToLower(value)
}

func isMetadataEndpoint(raw string) bool {
	switch normalizeEndpointPath(raw) {
	case "/models", "/v1/models", "/v1beta/models":
		return true
	default:
		return false
	}
}

// isSyntheticUserAgent uses bounded marker matching to avoid classifying normal
// browser/client user agents that merely contain a common word such as "probe".
func isSyntheticUserAgent(raw string) bool {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" {
		return false
	}
	markers := []string{
		"sub2api-acceptance-probe",
		"sub2api-synthetic",
		"sub2api-healthcheck",
		"sub2api-smoke-test",
		"sub2api-probe",
	}
	for _, marker := range markers {
		if containsBoundedMarker(value, marker) {
			return true
		}
	}
	return false
}

func containsBoundedMarker(value, marker string) bool {
	start := 0
	for {
		idx := strings.Index(value[start:], marker)
		if idx < 0 {
			return false
		}
		idx += start
		end := idx + len(marker)
		leftOK := idx == 0 || !isTokenRune(rune(value[idx-1]))
		rightOK := end == len(value) || !isTokenRune(rune(value[end]))
		if leftOK && rightOK {
			return true
		}
		if end >= len(value) {
			return false
		}
		start = idx + 1
	}
}

func isTokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-'
}
