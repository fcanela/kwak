package wire

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
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
	if err != nil || c.Version != ConfigVersion || c.ResumeMax != DefaultResumeMax {
		t.Fatalf("%+v %v", c, err)
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
