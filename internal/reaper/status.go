package reaper

import "remote-agents/internal/jsonfile"

// Status is the launcher's latest view of its session, for listings.
type Status struct {
	Working   []string `json:"working,omitempty"`
	IdleSince int64    `json:"idle_since_ms,omitempty"`
	Updated   int64    `json:"updated_ms"`
}

func WriteStatus(path string, s Status) error { return jsonfile.Write(path, s) }

// ReadStatus reports false when the session has no status yet.
func ReadStatus(path string) (Status, bool, error) {
	var s Status
	found, err := jsonfile.Read(path, &s)
	return s, found, err
}
