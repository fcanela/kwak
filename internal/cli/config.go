package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/fcanela/kwak/internal/atomicfile"
	"github.com/fcanela/kwak/internal/wire"
)

// ConfigVersion is the config.json version this kwak writes.
const ConfigVersion = wire.ConfigVersion

// ConfigFile is the name of the machine config in paths.ConfigDir() (spec 5.2).
const ConfigFile = "config.json"

// DefaultResumeMax bounds background resume runs per machine (DV-11).
const DefaultResumeMax = wire.DefaultResumeMax

// ErrNotPaired means there is no config.json.
var ErrNotPaired = errors.New("this machine is not paired; run kwak setup or kwak pair")

// Config is config.json: machine facts only, never a secret (spec 5.2).
type Config struct {
	wire.MachineConfig

	extra map[string]json.RawMessage // fields this kwak does not know, kept on save
}

// loadConfig reads dir/config.json. A missing file gives ErrNotPaired; a newer version, a bad
// file or a server that is not https:// (http:// with allowHTTP) gives an error.
func loadConfig(dir string, allowHTTP bool) (*Config, error) {
	return loadConfigFile(filepath.Join(dir, ConfigFile), allowHTTP)
}

func loadConfigFile(path string, allowHTTP bool) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotPaired
	}
	if err != nil {
		return nil, err
	}
	mc, err := wire.ParseMachineConfig(data, allowHTTP)
	if errors.Is(err, wire.ErrConfigNewer) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigFile, err)
	}
	c := Config{MachineConfig: mc}
	if err := json.Unmarshal(data, &c.extra); err != nil {
		return nil, err
	}
	for _, k := range configKeys {
		delete(c.extra, k)
	}
	return &c, nil
}

var configKeys = []string{"version", "server", "server_name", "owner", "machine", "pairing_id",
	"room_id", "standing_user", "resume_max", "resume_exe", "resume", "thread_ceiling", "resume_per_hour",
	"check_outgoing", "check_incoming", "keyring_user", "test", "responder", "responder_dir", "responder_prompt",
	"responder_command", "responder_continue", "responder_own_agents", "resume_timeout", "responder_timeout",
	"listener_autostart"}

// Save writes dir/config.json with mode 0600 through a temp file and a rename.
func (c *Config) Save(dir string) error {
	known, err := json.Marshal(c)
	if err != nil {
		return err
	}
	all := map[string]json.RawMessage{}
	for k, v := range c.extra {
		all[k] = v
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(known, &m); err != nil {
		return err
	}
	for k, v := range m {
		all[k] = v
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(dir, ConfigFile, append(data, '\n'))
}

func writeFile(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(dir, name), data, 0o600)
}
