package server

import (
	"time"

	"github.com/useteploy/teploy-dash/internal/remote"
)

// X02 §2.4 (D13): the observation envelope is the standard shape for ANY
// observed resource. canonicalObservation projects one fleet envelope onto
// that cross-product form — what the contracts corpus pins and what X06's
// "missing telemetry means unknown, not healthy" surfaces consume. The
// /api/fleet response keeps its D01 shape (the UI reads it); this is the
// shared-contract projection of the same truth, so drift between the two is
// impossible by construction (one derives from the other).
type canonicalObservation struct {
	Resource      canonicalResource  `json:"resource"`
	LastSuccessAt *time.Time         `json:"last_success_at,omitempty"`
	CollectedAt   time.Time          `json:"collected_at"`
	Freshness     string             `json:"freshness"` // fresh|stale|unknown (§2.4 rule 2)
	Error         string             `json:"error,omitempty"`
	Source        string             `json:"source,omitempty"`
	LastKnown     canonicalLastKnown `json:"last_known"`
}

type canonicalResource struct {
	Type string `json:"type"` // "server" today; the shape is resource-generic
	ID   string `json:"id"`   // envelope id: CLI stable id, else name-hash
}

type canonicalLastKnown struct {
	Server string            `json:"server"`
	Host   string            `json:"host,omitempty"`
	Apps   []remote.AppState `json:"apps"`
}

// canonicalObservationFrom projects a fleet envelope.
func canonicalObservationFrom(env ServerObservation) canonicalObservation {
	c := canonicalObservation{
		Resource:    canonicalResource{Type: "server", ID: env.ID},
		CollectedAt: env.CollectedAt,
		Freshness:   env.Freshness,
		Error:       env.Error,
		Source:      env.Source,
		LastKnown:   canonicalLastKnown{Server: env.Server, Host: env.Host, Apps: env.Apps},
	}
	if !env.LastSuccessAt.IsZero() {
		c.LastSuccessAt = &env.LastSuccessAt
	}
	return c
}
