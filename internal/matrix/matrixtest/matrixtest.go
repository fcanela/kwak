// Package matrixtest registers and logs in human accounts for tests. It needs a homeserver that
// allows registration with a token, such as the integration harness's tuwunel.
package matrixtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/fcanela/kwak/internal/matrix"
)

// DefaultForwardedFor is used when the client has no ForwardedFor, so every request carries it.
const DefaultForwardedFor = "127.0.0.1"

const (
	stageToken = "m.login.registration_token"
	stageDummy = "m.login.dummy"
)

type uia struct {
	Session   string   `json:"session"`
	Completed []string `json:"completed"`
	Flows     []struct {
		Stages []string `json:"stages"`
	} `json:"flows"`
}

func withXFF(c *matrix.Client) *matrix.Client {
	cp := *c
	if cp.ForwardedFor == "" {
		cp.ForwardedFor = DefaultForwardedFor
	}
	return &cp
}

// RegisterHuman registers localpart with a password through user-interactive auth, completing the
// registration token stage (and a dummy stage if the server asks for one).
func RegisterHuman(ctx context.Context, c *matrix.Client, localpart, password, regToken string) (matrix.Login, error) {
	c = withXFF(c)
	body := map[string]any{"username": localpart, "password": password}
	for range 4 {
		l, err := c.Register(ctx, body)
		if err == nil {
			return l, nil
		}
		var me *matrix.Error
		if !errors.As(err, &me) || me.Status != http.StatusUnauthorized || me.Code != "" || len(me.Body) == 0 {
			return matrix.Login{}, err
		}
		var u uia
		if json.Unmarshal(me.Body, &u) != nil || u.Session == "" {
			return matrix.Login{}, err
		}
		stage, ok := nextStage(u)
		if !ok {
			return matrix.Login{}, fmt.Errorf("matrixtest: no supported registration flow in %s", me.Body)
		}
		auth := map[string]any{"type": stage, "session": u.Session}
		if stage == stageToken {
			auth["token"] = regToken
		}
		body["auth"] = auth
	}
	return matrix.Login{}, errors.New("matrixtest: registration did not finish")
}

func nextStage(u uia) (string, bool) {
	for _, f := range u.Flows {
		if slices.ContainsFunc(f.Stages, func(s string) bool { return s != stageToken && s != stageDummy }) {
			continue
		}
		for _, s := range f.Stages {
			if !slices.Contains(u.Completed, s) {
				return s, true
			}
		}
	}
	return "", false
}

// LoginPassword logs userID in with a password.
func LoginPassword(ctx context.Context, c *matrix.Client, userID, password string) (matrix.Login, error) {
	return withXFF(c).LoginWith(ctx, map[string]any{
		"type":       "m.login.password",
		"identifier": map[string]string{"type": "m.id.user", "user": userID},
		"password":   password,
	})
}
