//go:build integration && linux

package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/fcanela/kwak/internal/matrix"
	"github.com/fcanela/kwak/test/integration/ittest"
)

func TestSmoke(t *testing.T) {
	env := ittest.Start(t)
	if env == nil {
		return
	}
	for name, l := range map[string]matrix.Login{"admin": env.Admin, "alice": env.Alice, "bob": env.Bob} {
		if l.UserID != "@"+name+":kwak.test" || l.AccessToken == "" {
			t.Errorf("%s: %+v", name, l)
		}
	}
	login := env.RegisterHuman(t, "carol")
	for name, c := range map[string]*matrix.Client{"Client": env.Client(login.AccessToken), "AppserviceClient": env.AppserviceClient()} {
		if ok, err := c.ProfileExists(t.Context(), login.UserID); err != nil || !ok {
			t.Errorf("env.%s ProfileExists(%s) = %v, %v", name, login.UserID, ok, err)
		}
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		env.Homeserver+"/_matrix/client/v3/account/whoami", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+login.AccessToken)
	req.Header.Set("X-Forwarded-For", ittest.ForwardedFor)
	resp, err := env.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&who); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || who.UserID != "@carol:kwak.test" || who.UserID != login.UserID {
		t.Fatalf("whoami: status %d, user %q, login %+v", resp.StatusCode, who.UserID, login)
	}
}
