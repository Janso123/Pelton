package desktop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/peltonapp/Pelton/internal/certtrust"
	"github.com/peltonapp/Pelton/internal/storage"
)

// probeJMAPCertificates looks for the JMAP server along the account's route,
// the way its first sync will, and returns the certificate that did not verify.
func (a *App) probeJMAPCertificates(account storage.Account) ([]UntrustedCertDTO, error) {
	_, probeErr, err := a.probeAccountJMAP(account)
	if err != nil {
		return nil, err
	}
	if u := untrustedJMAPCert(probeErr); u != nil {
		return []UntrustedCertDTO{*u}, nil
	}
	return nil, nil
}

// jmapPresentedCert checks the TLS handshake with the account's stored JMAP
// session server and returns the certificate that did not verify.
func (a *App) jmapPresentedCert(account storage.Account, trust certtrust.Trust, dial func(ctx context.Context, network, addr string) (net.Conn, error)) ([]UntrustedCertDTO, error) {
	host, port, err := jmapServer(account)
	if err != nil {
		return nil, err
	}
	if u := untrustedCert("jmap", host, port, a.checkJMAPTLS(host, port, trust, dial)); u != nil {
		return []UntrustedCertDTO{*u}, nil
	}
	return nil, nil
}

// checkJMAPTLS completes the handshake with a JMAP server along dial (nil for
// a direct connection) and hangs up.
func (a *App) checkJMAPTLS(host string, port int, trust certtrust.Trust, dial func(ctx context.Context, network, addr string) (net.Conn, error)) error {
	if a.checkTLS != nil {
		return a.checkTLS("jmap", host, port, trust)
	}
	if dial == nil {
		dial = (&net.Dialer{Timeout: mailDialTimeout}).DialContext
	}
	ctx, cancel := context.WithTimeout(a.ctx, mailDialTimeout)
	defer cancel()
	conn, err := dialMailTLS(ctx, dial, trust, net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return conn.Close()
}

// jmapServer is the host and port of a JMAP mailbox's session URL.
func jmapServer(account storage.Account) (string, int, error) {
	u, err := url.Parse(account.JMAPSessionURL)
	if err != nil || u.Hostname() == "" {
		return "", 0, fmt.Errorf("pelton: account %d has no usable JMAP session URL", account.ID)
	}
	return u.Hostname(), urlPort(u), nil
}

// untrustedJMAPCert describes the certificate behind a failed JMAP request,
// naming the server from the request's URL, or returns nil when err is some
// other failure.
func untrustedJMAPCert(err error) *UntrustedCertDTO {
	var uerr *url.Error
	if !errors.As(err, &uerr) {
		return nil
	}
	u, perr := url.Parse(uerr.URL)
	if perr != nil {
		return nil
	}
	return untrustedCert("jmap", u.Hostname(), urlPort(u), err)
}
