package desktop

import (
	"context"
	"errors"

	"github.com/peltonapp/Pelton/internal/certtrust"
	"github.com/peltonapp/Pelton/internal/credentials"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/proxy"
	"github.com/peltonapp/Pelton/internal/storage"
)

// prepareJMAPAccount signs a new JMAP mailbox in and fills in its session
// before the row is stored.
func (a *App) prepareJMAPAccount(account *storage.Account, req AddAccountRequest, secret credentials.Secret) error {
	// the route's password is not filed yet: take it from the request.
	_, routeCfg, err := a.routeFromDTO(req.Proxy, 0)
	if err != nil {
		return err
	}
	auth, err := a.runAuthenticateTarget(a.ctx, *account, routeCfg, "jmap", secret)
	if err != nil {
		return err
	}
	account.JMAPSessionURL = auth.SessionURL
	account.JMAPMailAccountID = auth.MailAccountID
	return nil
}

// afterJMAPAccountCreated adds CardDAV books to a password JMAP mailbox.
// Failures are logged inside provision and never fail the caller. OAuth never
// runs this.
func (a *App) afterJMAPAccountCreated(account storage.Account, secret credentials.Secret) {
	if secret.Method == credentials.MethodPassword {
		_ = a.runProvisionCardDAV(a.ctx, account, secret.Password)
	}
}

// restoreJMAPSession signs a mailbox restored from a backup in, to find its
// session. Without a credential or a route, or when the sign-in fails, the
// session is found on first connect.
func (a *App) restoreJMAPSession(account *storage.Account, secret credentials.Secret, hasSecret bool) {
	if !hasSecret {
		// switching protocol would drop the cache and download everything
		// again, so the account stays JMAP and finds its session on the
		// first sync after its password is entered.
		a.log.Warn("backup mailbox is JMAP but has no credential, its session will be found on first connect", "account", account.Email)
		return
	}
	// the route the account will sync over; never a direct fallback.
	route, err := a.accountRoute(*account)
	if err != nil {
		a.log.Warn("backup mailbox proxy route unavailable, its JMAP session will be found on first connect", "account", account.Email, "err", err)
		return
	}
	auth, err := a.runAuthenticateTarget(a.ctx, *account, route, "jmap", secret)
	if err != nil {
		a.log.Warn("backup mailbox JMAP sign-in failed, its session will be found on first connect", "account", account.Email, "err", err)
		return
	}
	account.JMAPSessionURL = auth.SessionURL
	account.JMAPMailAccountID = auth.MailAccountID
}

// checkJMAPPassword is CheckAccountPassword for a JMAP mailbox: it fetches the
// session with the typed password, which is the sign-in a JMAP sync does. A
// mailbox with no session yet (restored from a backup) looks it up instead, as
// its first sync would, and stores nothing it finds.
func (a *App) checkJMAPPassword(account storage.Account, password string) PasswordCheckDTO {
	secret := credentials.Secret{Method: credentials.MethodPassword, Password: password}
	var err error
	if account.JMAPSessionURL != "" {
		_, err = a.jmapSignIn(a.ctx, account, secret)
	} else {
		var route proxy.Config
		if route, err = a.accountRoute(account); err == nil {
			_, err = a.runAuthenticateTarget(a.ctx, account, route, "jmap", secret)
		}
	}
	switch {
	case err == nil:
		return PasswordCheckDTO{OK: true}
	case errors.Is(err, pjmap.ErrAuthFailed):
		return PasswordCheckDTO{Rejected: true, Error: err.Error()}
	default:
		return PasswordCheckDTO{Error: err.Error()}
	}
}

// probeDomain looks up JMAP for an email domain. Tests replace it.
var probeDomain = pjmap.ProbeDomain

// probeAccountJMAP looks for JMAP for a stored account along its route, with
// its trust and credentials. probeErr is the probe's own outcome (an untrusted
// certificate or a refused secret, see pjmap.ProbeDomain); err is a failure to
// get that far.
func (a *App) probeAccountJMAP(account storage.Account) (probe TestConnectionResult, probeErr, err error) {
	secret, err := credentials.Load(account.ID)
	if errors.Is(err, credentials.ErrNotFound) {
		return TestConnectionResult{}, nil, errNoCredentials
	}
	if err != nil {
		return TestConnectionResult{}, nil, err
	}
	route, err := a.accountRoute(account)
	if err != nil {
		return TestConnectionResult{}, nil, err
	}
	token, bearer := secret.Password, false
	if secret.Method == credentials.MethodOAuth {
		if token, err = a.freshAccessToken(account, secret); err != nil {
			return TestConnectionResult{}, nil, err
		}
		bearer = true
	}
	probe, probeErr = a.probeJMAP(a.ctx, route, accountTrust(account), account.Email, loginName(account), token, bearer, account.IMAPHost)
	return probe, probeErr, nil
}

// probeJMAP looks for JMAP along route, trusting what trust does. The error
// is a certificate that did not verify (see untrustedJMAPCert) or a refused
// secret (pjmap.ErrAuthFailed); any other failure reads as JMAP not being
// offered.
func (a *App) probeJMAP(ctx context.Context, route proxy.Config, trust certtrust.Trust, email, username, secret string, bearer bool, hostHints ...string) (TestConnectionResult, error) {
	// a one-off client: its connections are closed once the probe is done.
	httpClient := mailHTTPClient(route, trust, 0)
	defer httpClient.CloseIdleConnections()
	p, err := probeDomain(ctx, pjmap.ProbeOptions{
		HTTPClient: httpClient,
		NoSRV:      route.Enabled(),
	}, email, username, secret, bearer, hostHints...)
	if err != nil || !p.Available {
		return TestConnectionResult{}, err
	}
	return TestConnectionResult{
		JMAPAvailable:     p.Available,
		JMAPWebSocket:     p.WebSocket,
		JMAPSessionURL:    p.SessionURL,
		JMAPMailAccountID: p.MailAccountID,
	}, nil
}

func (a *App) discoverFoldersJMAP(ctx context.Context, account storage.Account) error {
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		return err
	}
	adapter := pjmap.NewAdapter(client, account.JMAPMailAccountID)
	engine := a.newSyncEngine(adapter, account.ID)
	return engine.SyncMailboxes(ctx, account.ID)
}
