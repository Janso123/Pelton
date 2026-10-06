package jmap

import (
	"errors"
	"fmt"
	"net/http"

	gojmap "github.com/Janso123/go-jmap"
)

// ErrAuthFailed reports that the server refused the credentials. AuthError
// wraps it into the error, so callers can tell a wrong password from a server
// that is merely unreachable and act on it: the app marks the mailbox and
// offers to re-enter the password instead of retrying forever.
var ErrAuthFailed = errors.New("jmap: authentication failed")

// AuthError adds ErrAuthFailed to an error the server answered with 401
// Unauthorized, the status RFC 8620 section 3.1 gives refused credentials,
// whether it came as problem+json or a bare status. Anything else is left
// alone: a server error or a dropped connection says nothing about the
// password.
func AuthError(err error) error {
	var problem *gojmap.RequestError
	var status *gojmap.HTTPError
	switch {
	case errors.As(err, &problem) && problem.Status == http.StatusUnauthorized,
		errors.As(err, &status) && status.Status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %w", ErrAuthFailed, err)
	default:
		return err
	}
}
