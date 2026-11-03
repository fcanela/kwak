package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ConfigVersion is the config.json version this kwak writes and the newest it reads.
const ConfigVersion = 1

// MachineConfig is config.json on a paired machine (spec 5.2): machine facts only, never a
// secret. kwak pair writes it and the listener reads it.
type MachineConfig struct {
	Version      int                `json:"version"`
	Server       string             `json:"server"`
	ServerName   string             `json:"server_name"`
	Owner        string             `json:"owner"`
	Machine      string             `json:"machine"`
	PairingID    string             `json:"pairing_id"`
	RoomID       string             `json:"room_id"`
	StandingUser string             `json:"standing_user"`
	ResumeMax    int                `json:"resume_max"`
	ResumeExe    map[Harness]string `json:"resume_exe,omitempty"`
}

// DefaultResumeMax bounds background resume runs per machine when config.json sets none (DV-11).
const DefaultResumeMax = 2

// ErrConfigNewer means config.json (or state.json) was written by a newer kwak (spec 7.4).
var ErrConfigNewer = errors.New("kwak is older than its config; go install a newer kwak")

// ParseMachineConfig reads config.json (spec 5.2, 7.4): a newer version gives ErrConfigNewer, an
// older one is migrated to ConfigVersion, the server must be https:// (also http:// when
// allowHTTP), and a missing resume_max is DefaultResumeMax.
func ParseMachineConfig(data []byte, allowHTTP bool) (MachineConfig, error) {
	var c MachineConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("not valid: %w", err)
	}
	if c.Version > ConfigVersion {
		return c, ErrConfigNewer
	}
	if !strings.HasPrefix(c.Server, "https://") && !(allowHTTP && strings.HasPrefix(c.Server, "http://")) {
		return c, fmt.Errorf("server %q must start with https://", c.Server)
	}
	c.Version = ConfigVersion
	if c.ResumeMax <= 0 {
		c.ResumeMax = DefaultResumeMax
	}
	return c, nil
}
