package jmap

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	gojmap "github.com/Janso123/go-jmap"
	"github.com/Janso123/go-jmap/core"
	"github.com/Janso123/go-jmap/core/push/websocket"
	"github.com/Janso123/go-jmap/mail"
	"github.com/Janso123/go-jmap/mail/emailsubmission"

	"github.com/peltonapp/Pelton/internal/certtrust"
)

const probeTimeout = 10 * time.Second

// discover is the session-endpoint lookup for host: SRV first when srv is
// set, otherwise straight to the well-known URL. Tests replace it to avoid DNS.
var discover = func(ctx context.Context, host string, srv bool) (string, error) {
	if srv {
		return core.Discover(ctx, host)
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/.well-known/jmap"}).String(), nil
}

// ProbeOptions is how a probe reaches the server.
type ProbeOptions struct {
	// HTTPClient carries every request the probe makes, which is where a
	// mailbox's proxy and certificate trust come in. Nil is a direct client
	// trusting the system roots.
	HTTPClient *http.Client
	// NoSRV skips the _jmap._tcp SRV lookup and goes to the host's well-known
	// URL. A proxied probe sets it: the lookup is a plain DNS query that would
	// leave outside the proxy.
	NoSRV bool
}

// Probe is what a JMAP probe found: whether the server offers JMAP mail, whether
// it advertises WebSocket push, the session URL to use, and the mail account id.
// A zero Probe means JMAP is not available.
type Probe struct {
	Available     bool
	WebSocket     bool
	SessionURL    string
	MailAccountID string
}

// ProbeDomain looks up JMAP for an email address and authenticates.
// username is the Basic user when bearer is false; secret is the password or the access token.
// hostHints are tried before the email domain (typically the IMAP host),
// which is what private Stalwart and similar setups need when mail lives on mail.example.com
// while addresses use @example.com.
// A timeout or a non-JMAP response returns a zero Probe and a nil error.
// When no host offers JMAP and one presented a certificate that did not verify,
// that failure is returned (certtrust.Untrusted finds the certificate in it), so
// the caller can show it for review. Failing that, a host that refused the
// secret returns an error carrying ErrAuthFailed, so the caller can ask for a
// new one.
func ProbeDomain(ctx context.Context, opts ProbeOptions, email, username, secret string, bearer bool, hostHints ...string) (Probe, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	var untrusted, refused error
	for _, host := range probeHosts(email, hostHints...) {
		p, err := probeHost(ctx, opts, host, username, secret, bearer)
		if err != nil {
			if _, ok := certtrust.Untrusted(err); ok {
				if untrusted == nil {
					untrusted = err
				}
			} else if err := AuthError(err); errors.Is(err, ErrAuthFailed) && refused == nil {
				refused = err
			}
			continue
		}
		if p.Available {
			return p, nil
		}
	}
	if untrusted != nil {
		return Probe{}, untrusted
	}
	return Probe{}, refused
}

func probeHosts(email string, hints ...string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(h string) {
		h = normalizeProbeHost(h)
		if h == "" || seen[h] {
			return
		}
		seen[h] = true
		out = append(out, h)
	}
	// Prefer the IMAP host first: private Stalwart (and similar) puts JMAP on
	// mail.example.com while addresses use @example.com, and a bogus
	// well-known on the email domain can burn the shared probe timeout.
	for _, h := range hints {
		add(h)
	}
	add(domainOf(email))
	return out
}

// normalizeProbeHost turns an IMAP host (possibly with a port) into a bare hostname.
func normalizeProbeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if host == "" || strings.Contains(host, "://") || strings.ContainsAny(host, "/\\?#@") {
		return ""
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" || strings.ContainsAny(host, "/\\?#@") {
		return ""
	}
	// Hostnames must not keep a colon; IPv6 literals may.
	if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return ""
	}
	return host
}

func probeHost(ctx context.Context, probe ProbeOptions, host, username, secret string, bearer bool) (Probe, error) {
	if err := ctx.Err(); err != nil {
		return Probe{}, err
	}
	sessionURL, err := discover(ctx, host, !probe.NoSRV)
	if err != nil {
		return Probe{}, err
	}

	var opts []gojmap.Option
	if probe.HTTPClient != nil {
		opts = append(opts, gojmap.WithHTTPClient(probe.HTTPClient))
	}
	if bearer {
		opts = append(opts, gojmap.WithBearer(secret))
	} else {
		opts = append(opts, gojmap.WithBasic(username, secret))
	}
	// after the auth option, which works on a copy of the client, so the
	// caller's client keeps its own timeout.
	opts = append(opts, gojmap.WithTimeout(probeTimeout))

	client := gojmap.NewClient(sessionURL, opts...)
	if err := client.Authenticate(ctx); err != nil {
		return Probe{}, err
	}
	if client.Session == nil {
		return Probe{}, nil
	}

	p := Probe{SessionURL: sessionURL}
	if _, ok := client.Session.Capabilities[websocket.URI]; ok {
		p.WebSocket = true
	}

	mailID, err := client.PrimaryAccount(mail.URI)
	if err != nil {
		return p, nil
	}
	p.MailAccountID = string(mailID)

	acc, ok := client.Session.Accounts[mailID]
	if !ok {
		return p, nil
	}

	if hasURI(client.Session.Capabilities, mail.URI) &&
		hasURI(client.Session.Capabilities, emailsubmission.URI) &&
		hasURI(acc.Capabilities, mail.URI) &&
		hasURI(acc.Capabilities, emailsubmission.URI) {
		p.Available = true
	}
	return p, nil
}

func domainOf(email string) string {
	_, domain, _ := strings.CutLast(email, "@")
	return domain
}

func hasURI(caps map[gojmap.URI]gojmap.Capability, uri gojmap.URI) bool {
	_, ok := caps[uri]
	return ok
}
