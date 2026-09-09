package service

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type RadarAlertCause string

const (
	RadarAlertCauseUpstreamModel        RadarAlertCause = "upstream_model"
	RadarAlertCauseChannelOrPool        RadarAlertCause = "channel_or_pool"
	RadarAlertCauseGatewayProtocol      RadarAlertCause = "gateway_protocol"
	RadarAlertCauseServiceQuality       RadarAlertCause = "service_quality"
	RadarAlertCauseInsufficientEvidence RadarAlertCause = "insufficient_evidence"
)

type RadarAlertStatus string

const (
	RadarAlertStatusOpen         RadarAlertStatus = "open"
	RadarAlertStatusAcknowledged RadarAlertStatus = "acknowledged"
	RadarAlertStatusResolved     RadarAlertStatus = "resolved"
)

type RadarRouteSlice struct {
	Reference        string
	Stable           bool
	QualityRegressed bool
}

type RadarAlertSignal struct {
	ModelRoute               string
	CapabilityDomain         string
	PolicyVersion            int
	QualityRegressed         bool
	ReliabilitySLOBreached   bool
	RouteSlices              []RadarRouteSlice
	OfficialDirectConfigured bool
	OfficialDirectStable     bool
	Sub2APIStable            bool
}

type RadarAlertEvent struct {
	Kind      string
	ActorID   int64
	CreatedAt time.Time
}

type RadarAlert struct {
	ID               uuid.UUID
	ModelRoute       string
	CapabilityDomain string
	Cause            RadarAlertCause
	PolicyVersion    int
	Status           RadarAlertStatus
	Events           []RadarAlertEvent
	recoveryPassed   bool
}

func AttributeRadarCause(signal RadarAlertSignal) RadarAlertCause {
	if !signal.QualityRegressed && signal.ReliabilitySLOBreached {
		return RadarAlertCauseServiceQuality
	}
	if signal.OfficialDirectConfigured && signal.OfficialDirectStable && !signal.Sub2APIStable {
		return RadarAlertCauseGatewayProtocol
	}
	regressed := 0
	for _, slice := range signal.RouteSlices {
		if slice.Stable && slice.QualityRegressed {
			regressed++
		}
	}
	if regressed >= 2 {
		return RadarAlertCauseUpstreamModel
	}
	if regressed == 1 {
		return RadarAlertCauseChannelOrPool
	}
	return RadarAlertCauseInsufficientEvidence
}

type RadarAlertRegistry struct {
	mu     sync.Mutex
	alerts map[string]*RadarAlert
}

func NewRadarAlertRegistry() *RadarAlertRegistry {
	return &RadarAlertRegistry{alerts: map[string]*RadarAlert{}}
}

func radarAlertKey(signal RadarAlertSignal) string {
	return strings.Join([]string{signal.ModelRoute, signal.CapabilityDomain, string(AttributeRadarCause(signal)), strconv.Itoa(signal.PolicyVersion)}, "|")
}

func (r *RadarAlertRegistry) Observe(signal RadarAlertSignal) *RadarAlert {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := radarAlertKey(signal)
	if existing := r.alerts[key]; existing != nil && existing.Status != RadarAlertStatusResolved {
		return existing
	}
	alert := &RadarAlert{ID: uuid.New(), ModelRoute: signal.ModelRoute, CapabilityDomain: signal.CapabilityDomain, Cause: AttributeRadarCause(signal), PolicyVersion: signal.PolicyVersion, Status: RadarAlertStatusOpen, Events: []RadarAlertEvent{{Kind: "observed", CreatedAt: time.Now().UTC()}}}
	r.alerts[key] = alert
	return alert
}

func (r *RadarAlertRegistry) Get(id uuid.UUID) *RadarAlert {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, alert := range r.alerts {
		if alert.ID == id {
			copyAlert := *alert
			copyAlert.Events = append([]RadarAlertEvent(nil), alert.Events...)
			return &copyAlert
		}
	}
	return nil
}

func (r *RadarAlertRegistry) Open() []*RadarAlert {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*RadarAlert, 0)
	for _, alert := range r.alerts {
		if alert.Status != RadarAlertStatusResolved {
			copyAlert := *alert
			copyAlert.Events = append([]RadarAlertEvent(nil), alert.Events...)
			out = append(out, &copyAlert)
		}
	}
	return out
}

func (r *RadarAlertRegistry) Acknowledge(id uuid.UUID, actorID int64) error {
	return r.transition(id, RadarAlertStatusAcknowledged, actorID, "acknowledged")
}

func (r *RadarAlertRegistry) Resolve(id uuid.UUID, actorID int64, recoveryPassed bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	alert := r.find(id)
	if alert == nil {
		return errors.New("radar alert not found")
	}
	if !recoveryPassed || !alert.recoveryPassed {
		return errors.New("successful recovery test is required")
	}
	alert.Status = RadarAlertStatusResolved
	alert.Events = append(alert.Events, RadarAlertEvent{Kind: "resolved", ActorID: actorID, CreatedAt: time.Now().UTC()})
	return nil
}

func (r *RadarAlertRegistry) RecordRecoveryTest(id uuid.UUID, passed bool, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	alert := r.find(id)
	if alert == nil {
		return errors.New("radar alert not found")
	}
	alert.recoveryPassed = passed
	alert.Events = append(alert.Events, RadarAlertEvent{Kind: "recovery_test", CreatedAt: at.UTC()})
	return nil
}

func (r *RadarAlertRegistry) transition(id uuid.UUID, status RadarAlertStatus, actorID int64, kind string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	alert := r.find(id)
	if alert == nil {
		return errors.New("radar alert not found")
	}
	alert.Status = status
	alert.Events = append(alert.Events, RadarAlertEvent{Kind: kind, ActorID: actorID, CreatedAt: time.Now().UTC()})
	return nil
}

func (r *RadarAlertRegistry) find(id uuid.UUID) *RadarAlert {
	for _, alert := range r.alerts {
		if alert.ID == id {
			return alert
		}
	}
	return nil
}
