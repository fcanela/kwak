package wire

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestMachineConfigJSON(t *testing.T) {
	in := `{"version":1,"server":"https://kwak.test","server_name":"kwak.test","owner":"fcanela",` +
		`"machine":"laptop","pairing_id":"3f9a1c2e","room_id":"!a1B2c3D4",` +
		`"standing_user":"@agent.fcanela.listener:kwak.test","resume_max":2,` +
		`"resume_exe":{"claude":"/home/u/.local/bin/claude"}}`
	var c MachineConfig
	if err := json.Unmarshal([]byte(in), &c); err != nil {
		t.Fatal(err)
	}
	want := MachineConfig{Version: 1, Server: "https://kwak.test", ServerName: "kwak.test",
		Owner: "fcanela", Machine: "laptop", PairingID: "3f9a1c2e", RoomID: "!a1B2c3D4",
		StandingUser: "@agent.fcanela.listener:kwak.test", ResumeMax: 2,
		ResumeExe: map[Harness]string{HarnessClaude: "/home/u/.local/bin/claude"}}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("decoded %+v", c)
	}
	out, err := json.Marshal(c)
	if err != nil || string(out) != in {
		t.Fatalf("encoded %s, %v", out, err)
	}
}

func TestParseMachineConfig(t *testing.T) {
	c, err := ParseMachineConfig([]byte(`{"version":0,"server":"https://kwak.test","server_name":"kwak.test"}`), false)
	if err != nil || c.Version != ConfigVersion || c.ResumeMax != DefaultResumeMax || DefaultResumeMax != 10 {
		t.Fatalf("%+v %v", c, err)
	}
	if c.ThreadCeiling != 0 || DefaultThreadCeiling != 50 {
		t.Fatalf("a missing thread_ceiling was filled in (%d) or the default changed", c.ThreadCeiling)
	}
	if c.ResumePerHour != 0 || DefaultResumePerHour != 300 {
		t.Fatalf("a missing resume_per_hour was filled in (%d) or the default changed", c.ResumePerHour)
	}
	c, err = ParseMachineConfig([]byte(`{"version":1,"server":"https://kwak.test","thread_ceiling":7,"resume_per_hour":40}`), false)
	if err != nil || c.ThreadCeiling != 7 || c.ResumePerHour != 40 {
		t.Fatalf("thread_ceiling, resume_per_hour: %+v %v", c, err)
	}
	if _, err := ParseMachineConfig([]byte(`{"version":2,"server":"https://kwak.test"}`), false); !errors.Is(err, ErrConfigNewer) {
		t.Fatalf("newer: %v", err)
	}
	if _, err := ParseMachineConfig([]byte(`{"version":1,"server":"http://kwak.test"}`), false); err == nil {
		t.Fatal("http accepted")
	}
	if _, err := ParseMachineConfig([]byte(`{"version":1,"server":"http://kwak.test"}`), true); err != nil {
		t.Fatalf("http for tests: %v", err)
	}
	if _, err := ParseMachineConfig([]byte(`{`), false); err == nil {
		t.Fatal("bad JSON accepted")
	}
}

// keyring_user and test are additive: a config.json from an older kwak has neither, keeps its
// shape on a save, and reads its key from the old shared keyring name (decision 40).
func TestMachineConfigKeyringAndTest(t *testing.T) {
	old, err := ParseMachineConfig([]byte(`{"version":1,"server":"https://kwak.test","server_name":"kwak.test","pairing_id":"3f9a1c2e"}`), false)
	if err != nil || old.KeyringUser != "" || old.Test {
		t.Fatalf("old config: %+v %v", old, err)
	}
	if got := old.KeyName(); got != "kwak.test/owner-key" {
		t.Fatalf("old config KeyName = %q", got)
	}
	out, _ := json.Marshal(old)
	var m map[string]any
	json.Unmarshal(out, &m)
	if _, ok := m["keyring_user"]; ok {
		t.Fatalf("an old config gained keyring_user: %s", out)
	}
	if _, ok := m["test"]; ok {
		t.Fatalf("an old config gained test: %s", out)
	}
	c, err := ParseMachineConfig([]byte(`{"version":1,"server":"https://kwak.test","server_name":"kwak.test",`+
		`"pairing_id":"3f9a1c2e","keyring_user":"kwak.test/3f9a1c2e/owner-key","test":true}`), false)
	if err != nil || !c.Test || c.KeyName() != "kwak.test/3f9a1c2e/owner-key" {
		t.Fatalf("new config: %+v %q %v", c, c.KeyName(), err)
	}
}

// The responder keys and the run limits are additive (decision 46): a config.json without them
// keeps its shape on a save, and a missing, bad or not positive limit is 15 minutes.
func TestMachineConfigResponderAndLimits(t *testing.T) {
	old, err := ParseMachineConfig([]byte(`{"version":1,"server":"https://kwak.test","volunteers":["x"]}`), false)
	if err != nil || old.Responder || old.ResumeTimeout != "" {
		t.Fatalf("old config: %+v %v", old, err)
	}
	out, _ := json.Marshal(old)
	for _, k := range []string{"responder", "responder_dir", "responder_prompt", "responder_command", "responder_continue",
		"responder_own_agents", "resume_timeout", "responder_timeout"} {
		var m map[string]any
		json.Unmarshal(out, &m)
		if _, ok := m[k]; ok {
			t.Fatalf("an old config gained %s: %s", k, out)
		}
	}
	c, err := ParseMachineConfig([]byte(`{"version":1,"server":"https://kwak.test","responder":true,`+
		`"responder_command":["/bin/x","{session}"],"responder_own_agents":true,"resume_timeout":"20m","responder_timeout":"90s"}`), false)
	if err != nil || !c.Responder || !c.ResponderOwnAgents || len(c.ResponderCommand) != 2 {
		t.Fatalf("new config: %+v %v", c, err)
	}
	if RunTimeout(c.ResumeTimeout) != 20*time.Minute || RunTimeout(c.ResponderTimeout) != 90*time.Second {
		t.Fatal("limits not read")
	}
	for _, s := range []string{"", "soon", "-5m", "0s"} {
		if RunTimeout(s) != 15*time.Minute {
			t.Fatalf("RunTimeout(%q) = %v", s, RunTimeout(s))
		}
	}
}
