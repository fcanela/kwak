package registrar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/fcanela/kwak/internal/wire"
)

// Duration is a time.Duration written as a Go duration string in config.json.
type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"10m\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	if v <= 0 {
		return fmt.Errorf("duration %q must be positive", s)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// D returns d as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// Config is config.json (spec 3.1). HSToken and BotLocalpart come from the registration file.
type Config struct {
	ServerName       string   `json:"server_name"`
	Homeserver       string   `json:"homeserver"`
	Registration     string   `json:"registration"`
	State            string   `json:"state"`
	AppserviceAddr   string   `json:"appservice_addr"`
	APIAddr          string   `json:"api_addr"`
	Room             string   `json:"room"`
	ReportRoom       string   `json:"report_room,omitempty"`      // optional (decision 39)
	TestRoom         string   `json:"test_room,omitempty"`        // optional; test pairings need it (decision 40)
	TestReportRoom   string   `json:"test_report_room,omitempty"` // optional (decisions 39 and 40)
	Admins           []string `json:"admins"`
	IdleAfter        Duration `json:"idle_after"`
	CloseGrace       Duration `json:"close_grace"`
	KeyLifetime      Duration `json:"key_lifetime"`
	KeyWarnBefore    Duration `json:"key_warn_before"`
	PairingTTL       Duration `json:"pairing_ttl"`
	UnconfirmedTTL   Duration `json:"unconfirmed_ttl"`
	PruneClosedAfter Duration `json:"prune_closed_after"`
	JobsEvery        Duration `json:"jobs_every"`
	ClaimWait        Duration `json:"claim_wait,omitzero"` // long poll of /pair/claim

	HSToken      string `json:"-"`
	BotLocalpart string `json:"-"`
}

// Defaults fills every unset duration and address with the spec 3.1 value.
func (c *Config) Defaults() {
	set := func(d *Duration, v time.Duration) {
		if *d == 0 {
			*d = Duration(v)
		}
	}
	set(&c.IdleAfter, 336*time.Hour)
	set(&c.CloseGrace, 10*time.Minute)
	set(&c.KeyLifetime, 2160*time.Hour)
	set(&c.KeyWarnBefore, 336*time.Hour)
	set(&c.PairingTTL, 10*time.Minute)
	set(&c.UnconfirmedTTL, 10*time.Minute)
	set(&c.PruneClosedAfter, 2160*time.Hour)
	set(&c.JobsEvery, time.Hour)
	set(&c.ClaimWait, 25*time.Second)
	if c.AppserviceAddr == "" {
		c.AppserviceAddr = ":8009"
	}
	if c.APIAddr == "" {
		c.APIAddr = ":8010"
	}
	if c.BotLocalpart == "" {
		c.BotLocalpart = wire.BotLocalpart
	}
}

// Check validates the fields the registrar relies on.
func (c *Config) Check() error {
	var errs []error
	if c.ServerName == "" || strings.ContainsAny(c.ServerName, "/@!#$ ") {
		errs = append(errs, fmt.Errorf("server_name %q is not a server name", c.ServerName))
	}
	if !c.alias(c.Room) {
		errs = append(errs, fmt.Errorf("room %q must be an alias on %s", c.Room, c.ServerName))
	} else if testName(c.Room) {
		errs = append(errs, fmt.Errorf("room %q must not end in %q: that name is for test rooms", c.Room, testSuffix))
	}
	// Test rooms are told apart by name (decision 41): the two test rooms end in "-test", the two
	// others do not. So a mistyped alias cannot put a test pairing in a room for real work.
	seen := map[string]string{c.Room: "room"}
	for _, side := range []struct {
		key, alias string
		test       bool
	}{
		{"report_room", c.ReportRoom, false}, {"test_room", c.TestRoom, true}, {"test_report_room", c.TestReportRoom, true},
	} {
		switch {
		case side.alias == "":
		case !c.alias(side.alias):
			errs = append(errs, fmt.Errorf("%s %q must be an alias on %s", side.key, side.alias, c.ServerName))
		case side.test && !testName(side.alias):
			errs = append(errs, fmt.Errorf("%s %q must end in %q, for example #agents-test:%s", side.key, side.alias, testSuffix, c.ServerName))
		case !side.test && testName(side.alias):
			errs = append(errs, fmt.Errorf("%s %q must not end in %q: that name is for test rooms", side.key, side.alias, testSuffix))
		case seen[side.alias] != "":
			errs = append(errs, fmt.Errorf("%s %q is also %s: each room must be its own", side.key, side.alias, seen[side.alias]))
		default:
			seen[side.alias] = side.key
		}
	}
	for _, a := range c.Admins {
		if !c.local(a) {
			errs = append(errs, fmt.Errorf("admin %q is not a user on %s", a, c.ServerName))
		}
	}
	if c.HSToken == "" {
		errs = append(errs, errors.New("no hs_token"))
	}
	return errors.Join(errs...)
}

// testSuffix ends the name of every test room, and of no other kwak room (decision 41).
const testSuffix = "-test"

// testName reports whether the room alias a names a test room: its local part, before the colon,
// ends in testSuffix.
func testName(a string) bool {
	name, _, _ := strings.Cut(strings.TrimPrefix(a, "#"), ":")
	return strings.HasSuffix(name, testSuffix)
}

// alias reports whether a is a room alias on this server.
func (c *Config) alias(a string) bool {
	name, ok := strings.CutPrefix(a, "#")
	name, ok2 := strings.CutSuffix(name, ":"+c.ServerName)
	return ok && ok2 && name != "" && !strings.ContainsAny(name, ":# ")
}

// local reports whether mxid is a user id on this server.
func (c *Config) local(mxid string) bool {
	lp, ok := localpart(mxid, c.ServerName)
	return ok && lp != ""
}

// mxid returns the full user id of a localpart.
func (c *Config) mxid(lp string) string { return "@" + lp + ":" + c.ServerName }

// Bot returns the registrar bot's user id.
func (c *Config) Bot() string { return c.mxid(c.BotLocalpart) }

func localpart(mxid, server string) (string, bool) {
	rest, ok := strings.CutPrefix(mxid, "@")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(rest, ":"+server)
}

// LoadConfig reads config.json strictly and then the registration file it names.
// It returns the config and the as_token.
func LoadConfig(path string) (Config, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, "", err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, "", fmt.Errorf("%s: %w", path, err)
	}
	if !strings.HasPrefix(c.Homeserver, "http://") && !strings.HasPrefix(c.Homeserver, "https://") {
		return Config{}, "", fmt.Errorf("%s: homeserver %q must be an http:// or https:// URL", path, c.Homeserver)
	}
	if c.State == "" || c.Registration == "" {
		return Config{}, "", fmt.Errorf("%s: state and registration are required", path)
	}
	raw, err := os.ReadFile(c.Registration)
	if err != nil {
		return Config{}, "", err
	}
	reg, err := wire.ParseRegistration(raw)
	if err != nil {
		return Config{}, "", fmt.Errorf("%s: %w (serve reads the JSON registration only)", c.Registration, err)
	}
	c.HSToken, c.BotLocalpart = reg.HSToken, reg.SenderLocalpart
	c.Defaults()
	if err := c.Check(); err != nil {
		return Config{}, "", fmt.Errorf("%s: %w", path, err)
	}
	return c, reg.ASToken, nil
}
