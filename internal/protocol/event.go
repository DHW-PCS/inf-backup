// Package protocol defines the console wire contract shared with maintenance tools.
package protocol

import (
	"encoding/json"
	"regexp"
)

const Version = 1
const WrapperVersion = "3.0.0"
const Prefix = "INF_BACKUP_EVENT "

var requestID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type Event struct {
	Protocol  int            `json:"protocol"`
	RequestID *string        `json:"request_id"`
	Operation string         `json:"operation"`
	Event     string         `json:"event"`
	Payload   map[string]any `json:"payload"`
}

func ValidID(id string) bool { return requestID.MatchString(id) }
func ValidOperation(op string) bool {
	return op == "backup" || op == "dry-run" || op == "snapshots" || op == "check"
}
func Terminal(event string) bool {
	return event == "succeeded" || event == "failed" || event == "interrupted"
}
func New(id, operation, event string, payload map[string]any) Event {
	var request *string
	if id != "" {
		request = &id
	}
	if payload == nil {
		payload = map[string]any{}
	}
	return Event{Version, request, operation, event, payload}
}
func (e Event) ID() string {
	if e.RequestID == nil {
		return ""
	}
	return *e.RequestID
}
func (e Event) Line() string {
	data, err := json.Marshal(e)
	if err != nil {
		panic(err) // Events only contain JSON-compatible values.
	}
	return Prefix + string(data)
}
