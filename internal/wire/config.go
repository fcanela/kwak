package wire

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/secrets"
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
	// Resume is this machine's background resume triggers (decision 44, spec 5.11): ResumeReplies,
	// ResumeMentions or both. Missing or empty is off. Only kwak config resume writes it; kwak pair
	// does not, and a re-pair keeps it.
	Resume []string `json:"resume,omitempty"`
	// ThreadCeiling is the loop ceiling: automatic turns per thread on this machine. kwak pair does
	// not write it; a missing or zero value is DefaultThreadCeiling.
	ThreadCeiling int `json:"thread_ceiling,omitempty"`
	// ResumePerHour caps background resume runs started on this machine in any 60 minutes
	// (decision 38). kwak pair does not write it; a missing or zero value is DefaultResumePerHour.
	// Only kwak config resume-per-hour writes it.
	ResumePerHour int `json:"resume_per_hour,omitempty"`
	// CheckOutgoing and CheckIncoming are the check commands of spec 5.16: absolute paths set with
	// kwak config, off when empty. kwak pair keeps them.
	CheckOutgoing string `json:"check_outgoing,omitempty"`
	CheckIncoming string `json:"check_incoming,omitempty"`
	// KeyringUser is the keyring item that holds this pairing's owner key,
	// secrets.PairingKeyName(server_name, pairing_id). kwak pair writes it since 2026-11-12; a
	// config.json without it was written by an older kwak, whose key is under the old shared
	// name (KeyName). Added 2026-11-12 (decision 40).
	KeyringUser string `json:"keyring_user,omitempty"`
	// Test is set for a test pairing (decision 40): its room is the test room and its standing
	// account the test one. kwak pair copies it from the claim. Added 2026-11-12.
	Test bool `json:"test,omitempty"`
	// Responder turns this machine's responder on (decision 46, spec 5.18). Only kwak config
	// responder writes it; kwak pair keeps it. The other responder keys are written by hand.
	Responder bool `json:"responder,omitempty"`
	// ResponderDir is the folder responder runs start in. Empty is "responder" in the config folder.
	ResponderDir string `json:"responder_dir,omitempty"`
	// ResponderPrompt replaces kwak's responder prompt when set.
	ResponderPrompt string `json:"responder_prompt,omitempty"`
	// ResponderCommand is the argv of a run for a new question, and ResponderContinue the argv of
	// a run for a follow-up in its thread. Both empty is kwak's command for Claude Code; only one
	// set starts no run. Placeholders: {claude} (resume_exe's claude, read at start), {prompt},
	// {session} (required) and {dir}.
	ResponderCommand  []string `json:"responder_command,omitempty"`
	ResponderContinue []string `json:"responder_continue,omitempty"`
	// ResponderOwnAgents lets the responder answer open questions of the owner's own agents, from
	// any machine. Off by default.
	ResponderOwnAgents bool `json:"responder_own_agents,omitempty"`
	// ResumeTimeout and ResponderTimeout limit one background run and one responder run: Go
	// durations such as "15m". Missing, bad or not positive is DefaultRunTimeout.
	ResumeTimeout    string `json:"resume_timeout,omitempty"`
	ResponderTimeout string `json:"responder_timeout,omitempty"`
	// ListenerAutostart false stops kwak mcp from starting the listener's user service when a
	// session sends a question while the listener is down (decision 46). Missing is on. Only kwak
	// config autostart writes it.
	ListenerAutostart *bool `json:"listener_autostart,omitempty"`
}

// Autostart reports whether kwak mcp may start the listener's user service (listener_autostart).
func (c MachineConfig) Autostart() bool { return c.ListenerAutostart == nil || *c.ListenerAutostart }

// DefaultRunTimeout limits one background or responder run (decision 46: "Lets set both to 15").
const DefaultRunTimeout = 15 * time.Minute

// RunTimeout reads a run limit of config.json: a positive Go duration, else DefaultRunTimeout.
func RunTimeout(s string) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return DefaultRunTimeout
}

// Background resume triggers in config.json (decision 44, spec 5.11).
const (
	ResumeReplies  = "replies"  // a reply in a thread the session started
	ResumeMentions = "mentions" // the session's agent is mentioned outside its own thread
)

// ResumeTriggers is the triggers of c.Resume that this kwak knows, replies first; nil means off. A
// word it does not know (a hand edit, or one a newer kwak wrote) is left out.
func (c MachineConfig) ResumeTriggers() []string {
	var on []string
	for _, t := range []string{ResumeReplies, ResumeMentions} {
		if slices.Contains(c.Resume, t) {
			on = append(on, t)
		}
	}
	return on
}

// KeyName is the keyring item of this pairing's owner key (secrets.KeyName).
func (c MachineConfig) KeyName() string {
	return secrets.KeyName(c.ServerName, c.PairingID, c.KeyringUser)
}

// DefaultResumeMax bounds background resume runs per machine when config.json sets none (DV-11,
// decision 35).
const DefaultResumeMax = 10

// DefaultResumePerHour caps background resume runs started per machine in any 60 minutes when
// config.json sets none (decision 38: "Go 300 per hour").
const DefaultResumePerHour = 300

// DefaultThreadCeiling is the loop ceiling when config.json sets none (build brief item 6).
const DefaultThreadCeiling = 50

// ErrConfigNewer means config.json (or state.json) was written by a newer kwak (spec 7.4).
var ErrConfigNewer = errors.New("kwak is older than its config; go install a newer kwak")

// ParseMachineConfig reads config.json (spec 5.2, 7.4): a newer version gives ErrConfigNewer, an
// older one is migrated to ConfigVersion, the server must be https:// (also http:// when
// allowHTTP), and a missing resume_max is DefaultResumeMax. thread_ceiling and resume_per_hour stay
// as written, so a save never adds them.
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
