package service

import "testing"

func TestClassifyTraffic(t *testing.T) {
	tests := []struct {
		name string
		in   TrafficClassificationInput
		want TrafficClass
	}{
		{
			name: "model list is metadata",
			in:   TrafficClassificationInput{InboundEndpoint: "/v1/models", DefaultProduction: true},
			want: TrafficClassMetadata,
		},
		{
			name: "count tokens is metadata",
			in:   TrafficClassificationInput{RequestPath: "/v1/responses", IsCountTokens: true},
			want: TrafficClassMetadata,
		},
		{
			name: "acceptance probe is synthetic",
			in:   TrafficClassificationInput{RequestPath: "/v1/responses", UserAgent: "sub2api-acceptance-probe/1"},
			want: TrafficClassSynthetic,
		},
		{
			name: "normal gateway request is production",
			in:   TrafficClassificationInput{InboundEndpoint: "/v1/responses"},
			want: TrafficClassProduction,
		},
		{
			name: "usage without endpoint defaults to production",
			in:   TrafficClassificationInput{DefaultProduction: true},
			want: TrafficClassProduction,
		},
		{
			name: "error without origin remains unknown",
			in:   TrafficClassificationInput{},
			want: TrafficClassUnknown,
		},
		{
			name: "explicit valid class wins",
			in:   TrafficClassificationInput{ExplicitClass: "synthetic", InboundEndpoint: "/v1/responses"},
			want: TrafficClassSynthetic,
		},
		{
			name: "invalid explicit class becomes unknown when no signal",
			in:   TrafficClassificationInput{ExplicitClass: "invalid"},
			want: TrafficClassUnknown,
		},
		{
			name: "invalid explicit class never falls back to production",
			in:   TrafficClassificationInput{ExplicitClass: "invalid", InboundEndpoint: "/v1/responses", DefaultProduction: true},
			want: TrafficClassUnknown,
		},
		{
			name: "explicit unknown remains unknown",
			in:   TrafficClassificationInput{ExplicitClass: "unknown", InboundEndpoint: "/v1/responses", DefaultProduction: true},
			want: TrafficClassUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyTraffic(tt.in); got != tt.want {
				t.Fatalf("ClassifyTraffic() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeTrafficClass(t *testing.T) {
	if got := NormalizeTrafficClass(" Production "); got != TrafficClassProduction {
		t.Fatalf("NormalizeTrafficClass() = %q, want %q", got, TrafficClassProduction)
	}
	if got := NormalizeTrafficClass("invalid"); got != TrafficClassUnknown {
		t.Fatalf("NormalizeTrafficClass() = %q, want %q", got, TrafficClassUnknown)
	}
}
