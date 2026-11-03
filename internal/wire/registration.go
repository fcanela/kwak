package wire

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Fixed values of the kwak appservice registration.
const (
	RegistrationID = "kwak"
	BotLocalpart   = "registrar"
)

// UserNamespace returns the anchored user namespace regex for serverName (I6).
func UserNamespace(serverName string) string {
	return `^@agent\..+:` + regexp.QuoteMeta(serverName) + `$`
}

// GenRegistration returns a fresh registration with two random 64-hex tokens read from rnd.
func GenRegistration(url, serverName string, rnd io.Reader) ([]byte, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, errors.New("wire: registration url must start with http:// or https://")
	}
	if !validServerName(serverName) {
		return nil, fmt.Errorf("wire: bad server name %q", serverName)
	}
	var b [64]byte
	if _, err := io.ReadFull(rnd, b[:]); err != nil {
		return nil, fmt.Errorf("wire: read random tokens: %w", err)
	}
	r := Registration{
		ID:              RegistrationID,
		URL:             url,
		ASToken:         hex.EncodeToString(b[:32]),
		HSToken:         hex.EncodeToString(b[32:]),
		SenderLocalpart: BotLocalpart,
		Namespaces: Namespaces{
			Users:   []Namespace{{Exclusive: true, Regex: UserNamespace(serverName)}},
			Aliases: []Namespace{},
			Rooms:   []Namespace{},
		},
	}
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// ParseRegistration reads a registration file strictly and checks the fields kwak relies on.
func ParseRegistration(data []byte) (Registration, error) {
	var r Registration
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Registration{}, fmt.Errorf("wire: registration: %w", err)
	}
	if !atEOF(dec) {
		return Registration{}, errors.New("wire: registration: trailing data")
	}
	switch {
	case r.ASToken == "" || r.HSToken == "":
		return Registration{}, errors.New("wire: registration: missing as_token or hs_token")
	case r.ASToken == r.HSToken:
		return Registration{}, errors.New("wire: registration: as_token equals hs_token")
	case r.SenderLocalpart == "":
		return Registration{}, errors.New("wire: registration: missing sender_localpart")
	case r.MSC4190:
		return Registration{}, errors.New("wire: registration: io.element.msc4190 must be false")
	}
	return r, nil
}

// validServerName accepts a DNS name or IP literal with an optional port. It refuses anything that
// could break out of the JSON value or the regex.
func validServerName(s string) bool {
	if s == "" || len(s) > 255 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' ||
			c == '.' || c == '-' || c == ':' || c == '[' || c == ']'
		if !ok {
			return false
		}
	}
	return true
}
