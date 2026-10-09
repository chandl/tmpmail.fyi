package app

import "time"

// AnalyticsEvent contains metadata only. Never add MIME content or arbitrary headers.
// Kind is delivery, smtpRejected or http; polling is an explicit, unverified UI marker.
type AnalyticsEvent struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Timestamp    time.Time `json:"timestamp"`
	Recipient    string    `json:"recipient,omitempty"`
	Sender       string    `json:"sender,omitempty"`
	SenderDomain string    `json:"senderDomain,omitempty"`
	IP           string    `json:"ip"`
	Size         int64     `json:"size,omitempty"`
	UserAgent    string    `json:"userAgent,omitempty"`
	Route        string    `json:"route,omitempty"`
	Method       string    `json:"method,omitempty"`
	Status       int       `json:"status,omitempty"`
	DurationMS   float64   `json:"durationMs,omitempty"`
	MessageID    string    `json:"messageId,omitempty"`
	Polling      bool      `json:"polling"`
}
