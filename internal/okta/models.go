package okta

import "time"

// Group represents a simplified Okta group payload.
type Group struct {
	ID          string
	Name        string
	Description string
}

// GroupMember represents a user assigned to an Okta group.
type GroupMember struct {
	ID     string
	Status string
	Login  string
	Email  string
}

// EventTarget references the target object of an Okta system log entry.
type EventTarget struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
}

// Event captures the subset of fields needed from the Okta system log.
type Event struct {
	EventType string        `json:"eventType"`
	Published time.Time     `json:"published"`
	Targets   []EventTarget `json:"targets"`
}
