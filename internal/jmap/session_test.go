package jmap

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/peltonapp/Pelton/internal/certtrust"
)

const sessionDocMailSubmission = `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32,
      "maxObjectsInGet": 256,
      "maxObjectsInSet": 128,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:submission": {},
    "urn:ietf:params:jmap:websocket": {
      "url": "wss://example.com/jmap/ws",
      "supportsPush": true
    }
  },
  "accounts": {
    "A1": {
      "name": "user@example.com",
      "isPersonal": true,
      "isReadOnly": false,
      "accountCapabilities": {
        "urn:ietf:params:jmap:mail": {
          "maxMailboxesPerEmail": null,
          "maxMailboxDepth": 10
        },
        "urn:ietf:params:jmap:submission": {
          "maxDelayedSend": 0,
          "submissionExtensions": {}
        }
      }
    }
  },
  "primaryAccounts": {
    "urn:ietf:params:jmap:mail": "A1",
    "urn:ietf:params:jmap:submission": "A1"
  },
  "username": "user@example.com",
  "apiUrl": "/api/",
  "downloadUrl": "/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "/upload/{accountId}/",
  "eventSourceUrl": "/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
  "state": "s1"
}`

const sessionDocMailOnly = `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32,
      "maxObjectsInGet": 256,
      "maxObjectsInSet": 128,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {}
  },
  "accounts": {
    "A1": {
      "name": "user@example.com",
      "isPersonal": true,
      "isReadOnly": false,
      "accountCapabilities": {
        "urn:ietf:params:jmap:mail": {
          "maxMailboxesPerEmail": null,
          "maxMailboxDepth": 10
        }
      }
    }
  },
  "primaryAccounts": {
    "urn:ietf:params:jmap:mail": "A1"
  },
  "username": "user@example.com",
  "apiUrl": "/api/",
  "downloadUrl": "/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "/upload/{accountId}/",
  "eventSourceUrl": "/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
  "state": "s1"
}`

const sessionDocSubmissionMissingOnAccount = `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32,
      "maxObjectsInGet": 256,
      "maxObjectsInSet": 128,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:submission": {}
  },
  "accounts": {
    "A1": {
      "name": "user@example.com",
      "isPersonal": true,
      "isReadOnly": false,
      "accountCapabilities": {
        "urn:ietf:params:jmap:mail": {
          "maxMailboxesPerEmail": null,
          "maxMailboxDepth": 10
        }
      }
    }
  },
  "primaryAccounts": {
    "urn:ietf:params:jmap:mail": "A1",
    "urn:ietf:params:jmap:submission": "A1"
  },
  "username": "user@example.com",
  "apiUrl": "/api/",
  "downloadUrl": "/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "/upload/{accountId}/",
  "eventSourceUrl": "/eventsource/?types={types}&closeafter={closeafter}&ping={ping}",
  "state": "s1"
}`

func TestProbe(t *testing.T) {
	t.Run("mail and submission available", func(t *testing.T) {
		srv := newSessionServer(t, http.StatusOK, sessionDocMailSubmission, wantBasic("user", "pw"))
		defer srv.Close()
		stubDiscover(t, srv.URL+"/.well-known/jmap")

		p, err := ProbeDomain(context.Background(), ProbeOptions{}, "user@example.com", "user", "pw", false)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !p.Available {
			t.Fatal("Available: want true")
		}
		if p.MailAccountID != "A1" {
			t.Fatalf("MailAccountID: got %q want A1", p.MailAccountID)
		}
		wantURL := srv.URL + "/.well-known/jmap"
		if p.SessionURL != wantURL {
			t.Fatalf("SessionURL: got %q want %q", p.SessionURL, wantURL)
		}
		if !p.WebSocket {
			t.Fatal("WebSocket: want true")
		}
	})

	t.Run("without submission", func(t *testing.T) {
		srv := newSessionServer(t, http.StatusOK, sessionDocMailOnly, wantBasic("user", "pw"))
		defer srv.Close()
		stubDiscover(t, srv.URL+"/.well-known/jmap")

		p, err := ProbeDomain(context.Background(), ProbeOptions{}, "user@example.com", "user", "pw", false)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if p.Available {
			t.Fatal("Available: want false")
		}
	})

	t.Run("http 404", func(t *testing.T) {
		srv := newSessionServer(t, http.StatusNotFound, "", "")
		defer srv.Close()
		stubDiscover(t, srv.URL+"/.well-known/jmap")

		p, err := ProbeDomain(context.Background(), ProbeOptions{}, "user@example.com", "user", "pw", false)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if p.Available {
			t.Fatal("Available: want false")
		}
		if p != (Probe{}) {
			t.Fatalf("want zero Probe, got %+v", p)
		}
	})

	// a refusal is the one failure that says something about the password, so
	// it comes back rather than reading as "no JMAP here": a mailbox looking up
	// its session has to be able to ask for a new one.
	t.Run("http 401", func(t *testing.T) {
		srv := newSessionServer(t, http.StatusUnauthorized, "", "")
		defer srv.Close()
		stubDiscover(t, srv.URL+"/.well-known/jmap")

		p, err := ProbeDomain(context.Background(), ProbeOptions{}, "user@example.com", "user", "pw", false)
		if !errors.Is(err, ErrAuthFailed) {
			t.Fatalf("err = %v, want ErrAuthFailed", err)
		}
		if p.Available {
			t.Fatal("Available: want false")
		}
		if p != (Probe{}) {
			t.Fatalf("want zero Probe, got %+v", p)
		}
	})

	t.Run("submission absent from account", func(t *testing.T) {
		srv := newSessionServer(t, http.StatusOK, sessionDocSubmissionMissingOnAccount, wantBasic("user", "pw"))
		defer srv.Close()
		stubDiscover(t, srv.URL+"/.well-known/jmap")

		p, err := ProbeDomain(context.Background(), ProbeOptions{}, "user@example.com", "user", "pw", false)
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if p.Available {
			t.Fatal("Available: want false")
		}
	})

	t.Run("falls back to imap host hint", func(t *testing.T) {
		srv := newSessionServer(t, http.StatusOK, sessionDocMailSubmission, wantBasic("user", "pw"))
		defer srv.Close()
		prev := discover
		discover = func(ctx context.Context, domain string, _ bool) (string, error) {
			switch domain {
			case "example.com":
				return "", context.DeadlineExceeded
			case "mail.example.com":
				return srv.URL + "/.well-known/jmap", nil
			default:
				t.Fatalf("unexpected discover domain %q", domain)
				return "", nil
			}
		}
		t.Cleanup(func() { discover = prev })

		p, err := ProbeDomain(context.Background(), ProbeOptions{}, "user@example.com", "user", "pw", false, "mail.example.com:993")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if !p.Available {
			t.Fatal("Available: want true via IMAP host hint")
		}
		wantURL := srv.URL + "/.well-known/jmap"
		if p.SessionURL != wantURL {
			t.Fatalf("SessionURL: got %q want %q", p.SessionURL, wantURL)
		}
	})
}

func TestNormalizeProbeHost(t *testing.T) {
	cases := map[string]string{
		"mail.example.com":     "mail.example.com",
		"Mail.Example.COM:993": "mail.example.com",
		"[::1]:993":            "::1",
		"https://evil":         "",
		"":                     "",
	}
	for in, want := range cases {
		if got := normalizeProbeHost(in); got != want {
			t.Errorf("normalizeProbeHost(%q)=%q, want %q", in, got, want)
		}
	}
}

func stubDiscover(t *testing.T, sessionURL string) {
	t.Helper()
	prev := discover
	discover = func(ctx context.Context, domain string, _ bool) (string, error) {
		return sessionURL, nil
	}
	t.Cleanup(func() { discover = prev })
}

func wantBasic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func newSessionServer(t *testing.T, status int, body string, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jmap" {
			http.NotFound(w, r)
			return
		}
		if wantAuth != "" {
			got := r.Header.Get("Authorization")
			if got != wantAuth {
				t.Errorf("Authorization: got %q want %q", got, wantAuth)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
}

// The probe goes out over the client it is given, which is how the account's
// proxy and certificate trust reach it. Without that client a self-signed
// server fails, and the failure comes back so it can be shown for review
// rather than reading as "no JMAP here".
func TestProbeDomainUsesTheGivenClientAndReportsAnUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != wantBasic("user", "pw") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sessionDocMailSubmission)
	}))
	defer srv.Close()
	stubDiscover(t, srv.URL+"/.well-known/jmap")

	p, err := ProbeDomain(context.Background(), ProbeOptions{HTTPClient: srv.Client()}, "user@example.com", "user", "pw", false)
	if err != nil || !p.Available {
		t.Fatalf("probe over the trusting client = %+v, %v; want available", p, err)
	}

	p, err = ProbeDomain(context.Background(), ProbeOptions{}, "user@example.com", "user", "pw", false)
	if p.Available {
		t.Fatal("probe trusted a self-signed server")
	}
	cert, ok := certtrust.Untrusted(err)
	if !ok {
		t.Fatalf("probe error = %v, want the untrusted certificate", err)
	}
	if !cert.Equal(srv.Certificate()) {
		t.Error("reported a certificate other than the server's")
	}
}

// A proxied probe must not look up SRV records: that is a plain DNS query
// outside the proxy, naming the mail domain to whoever watches the network.
func TestProbeDomainSkipsSRVWhenAsked(t *testing.T) {
	for _, noSRV := range []bool{false, true} {
		var gotSRV []bool
		prev := discover
		discover = func(ctx context.Context, domain string, srv bool) (string, error) {
			gotSRV = append(gotSRV, srv)
			return "", context.DeadlineExceeded
		}
		_, _ = ProbeDomain(context.Background(), ProbeOptions{NoSRV: noSRV}, "user@example.com", "user", "pw", false, "mail.example.com")
		discover = prev
		if len(gotSRV) != 2 || gotSRV[0] == noSRV || gotSRV[1] == noSRV {
			t.Errorf("NoSRV=%v: discover srv flags %v, want %v for both hosts", noSRV, gotSRV, !noSRV)
		}
	}
}

func TestDiscoverWithoutSRVIsTheWellKnownURL(t *testing.T) {
	got, err := discover(context.Background(), "mail.example.com", false)
	if err != nil || got != "https://mail.example.com/.well-known/jmap" {
		t.Errorf("discover without SRV = %q, %v", got, err)
	}
}

func TestDomainOf(t *testing.T) {
	tests := []struct{ email, want string }{
		{"me@example.com", "example.com"},
		{"first@second@example.com", "example.com"},
		{"@example.com", "example.com"},
		{"me@", ""},
		{"example.com", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := domainOf(tt.email); got != tt.want {
			t.Errorf("domainOf(%q) = %q, want %q", tt.email, got, tt.want)
		}
	}
}
