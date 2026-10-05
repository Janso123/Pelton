package jmap

import (
	"errors"
	"testing"

	gojmap "github.com/Janso123/go-jmap"
)

// Only a 401 says the credentials were refused. A server error or a dropped
// connection says nothing about the password, and treating it as a refusal
// would ask the user to retype one that works.
func TestAuthErrorMarksOnlyARefusal(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "problem+json 401", err: &gojmap.RequestError{Type: "about:blank", Status: 401}, want: true},
		{name: "plain 401", err: &gojmap.HTTPError{Status: 401, StatusText: "401 Unauthorized"}, want: true},
		{name: "server error", err: &gojmap.HTTPError{Status: 500, StatusText: "500 Internal Server Error"}},
		{name: "forbidden problem", err: &gojmap.RequestError{Type: "about:blank", Status: 403}},
		{name: "network", err: errors.New("dial tcp: connection refused")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := AuthError(tt.err)
			if errors.Is(got, ErrAuthFailed) != tt.want {
				t.Errorf("AuthError(%v) is ErrAuthFailed = %v, want %v", tt.err, !tt.want, tt.want)
			}
			if !errors.Is(got, tt.err) {
				t.Errorf("AuthError(%v) lost the original error", tt.err)
			}
		})
	}
	if AuthError(nil) != nil {
		t.Error("AuthError(nil) is not nil")
	}
}
