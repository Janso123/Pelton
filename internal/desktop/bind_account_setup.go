package desktop

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	"github.com/peltonapp/Pelton/internal/autoconfig"
	"github.com/peltonapp/Pelton/internal/certtrust"
	"github.com/peltonapp/Pelton/internal/credentials"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/oauth"
	"github.com/peltonapp/Pelton/internal/proxy"
	psmtp "github.com/peltonapp/Pelton/internal/smtp"
	"github.com/peltonapp/Pelton/internal/storage"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// oauthFlowTimeout bounds how long the interactive consent flow may take.
const oauthFlowTimeout = 5 * time.Minute

// switchProtocolStallTimeout is how long clearing the cached mail during a
// protocol switch may go without committing a delete batch before the attempt
// is cancelled. Each committed batch restarts it. Tests shorten it.
var switchProtocolStallTimeout = 60 * time.Second

// errProtocolSwitchTimedOut is what SwitchProtocol returns when clearing the
// cache stalled twice. The account stays on its old protocol.
var errProtocolSwitchTimedOut = errors.New("pelton: protocol switch timed out")

// probeDomain looks up JMAP for an email domain. Tests replace it.
var probeDomain = pjmap.ProbeDomain

// DiscoveredDTO is the autodiscovery result for the wizard.
type DiscoveredDTO struct {
	IMAPHost string `json:"imapHost"`
	IMAPPort int    `json:"imapPort"`
	SMTPHost string `json:"smtpHost"`
	SMTPPort int    `json:"smtpPort"`
	// IMAPTLS and SMTPTLS are the security the source stated ("ssl" or
	// "starttls"), empty when it said nothing usable.
	IMAPTLS string `json:"imapTls"`
	SMTPTLS string `json:"smtpTls"`
	OAuth   bool   `json:"oauth"`
	// OAuthProvider is the provider key to sign in with when the servers belong
	// to one Pelton supports ("google"), empty otherwise.
	OAuthProvider string `json:"oauthProvider"`
	Source        string `json:"source"`
}

// DiscoverConfig resolves likely imap/smtp settings for an email address using
// autoconfig (ISPDB, the domain's well-known/autoconfig, then a guess). The
// wizard pre-fills the form with this; the user can still edit before testing.
func (a *App) DiscoverConfig(email string) (DiscoveredDTO, error) {
	d, err := autoconfig.Discover(a.ctx, a.httpClient(10*time.Second), email)
	if err != nil {
		return DiscoveredDTO{}, err
	}
	return DiscoveredDTO{
		IMAPHost:      d.IMAPHost,
		IMAPPort:      d.IMAPPort,
		SMTPHost:      d.SMTPHost,
		SMTPPort:      d.SMTPPort,
		IMAPTLS:       d.IMAPTLS,
		SMTPTLS:       d.SMTPTLS,
		OAuth:         d.OAuth,
		OAuthProvider: d.OAuthProvider,
		Source:        d.Source,
	}, nil
}

// ListOAuthProviders returns the supported oauth provider keys and labels so the
// wizard knows which providers can use the sign-in flow.
func (a *App) ListOAuthProviders() (map[string]string, error) {
	return oauth.Providers(), nil
}

// TestConnectionRequest carries the settings to verify before saving (password
// auth). OAuth is verified by the sign-in flow itself, so it is not tested here.
type TestConnectionRequest struct {
	Email string `json:"email"`
	// Username is the login name when it differs from the email; empty logs in
	// with Email.
	Username string `json:"username"`
	IMAPHost string `json:"imapHost"`
	IMAPPort int    `json:"imapPort"`
	// IMAPTLS pins the connection security: "ssl", "starttls", or empty to
	// derive it from the port. Sent so the test uses the same transport the
	// account will, instead of testing a different one.
	IMAPTLS  string `json:"imapTls"`
	Password string `json:"password"`
	// SMTPHost, SMTPPort and SMTPTLS are only checked for the certificate the
	// server presents, so one that needs trusting is caught here too.
	SMTPHost string `json:"smtpHost"`
	SMTPPort int    `json:"smtpPort"`
	SMTPTLS  string `json:"smtpTls"`
	// TrustedCerts and CAPEM are what the new mailbox will trust beyond the
	// system roots: fingerprints accepted in an earlier test, and a CA file.
	TrustedCerts []string `json:"trustedCerts"`
	CAPEM        string   `json:"caPem"`
	// Proxy is the route the new mailbox will take, so a mailbox only
	// reachable through its own proxy can pass the test before it exists.
	Proxy AccountProxyDTO `json:"proxy"`
}

// TestConnectionResult is the optional JMAP probe outcome of a connection
// test. There is no contacts capability field.
type TestConnectionResult struct {
	JMAPAvailable     bool   `json:"jmapAvailable"`
	JMAPWebSocket     bool   `json:"jmapWebSocket"`
	JMAPSessionURL    string `json:"jmapSessionURL"`
	JMAPMailAccountID string `json:"jmapMailAccountID"`
}

// TestConnection verifies imap credentials by connecting and logging in, so the
// wizard can confirm before creating the account, then probes JMAP. A server
// certificate that does not verify is not an error: it comes back in the
// result, from both servers at once, for the user to review and trust (#446).
// IMAP failure returns the IMAP error and a zero result. A probe failure leaves
// JMAPAvailable false and returns a nil error; an untrusted JMAP certificate is
// listed in Untrusted next to that.
func (a *App) TestConnection(req TestConnectionRequest) (ConnectionTestDTO, error) {
	if req.CAPEM != "" {
		if _, err := certtrust.ParseCA(req.CAPEM); err != nil {
			return ConnectionTestDTO{}, err
		}
	}
	_, route, err := a.routeFromDTO(req.Proxy, 0)
	if err != nil {
		return ConnectionTestDTO{}, err
	}
	dial := route.DialContext()
	trust := certtrust.Trust{Pins: normalizePins(req.TrustedCerts), CAPEM: req.CAPEM}
	untrusted := a.probeCertificates(
		pimap.Config{Host: req.IMAPHost, Port: req.IMAPPort, TLS: imapTLSMode(req.IMAPTLS), Trust: trust, Dial: dial},
		psmtp.Config{Host: req.SMTPHost, Port: req.SMTPPort, TLS: smtpTLSMode(req.SMTPTLS), Trust: trust, Dial: dial},
	)
	if len(untrusted) > 0 {
		return ConnectionTestDTO{Untrusted: untrusted}, nil
	}

	username := req.Username
	if username == "" {
		username = req.Email
	}
	client, err := a.connectIMAP(pimap.Config{
		Host:     req.IMAPHost,
		Port:     req.IMAPPort,
		Username: username,
		Password: req.Password,
		TLS:      imapTLSMode(req.IMAPTLS),
		Trust:    trust,
		Dial:     dial,
	})
	if err != nil {
		return ConnectionTestDTO{}, err
	}
	defer client.Close()
	if err := client.Login(); err != nil {
		return ConnectionTestDTO{}, err
	}
	_ = client.Logout()

	// the probe sends the same password, so it takes the same route and
	// trust. A JMAP certificate that does not verify comes back for review;
	// the IMAP login has passed, so the mailbox can still be added without it.
	probe, err := a.probeJMAP(a.ctx, route, trust, req.Email, username, req.Password, false, req.IMAPHost)
	result := ConnectionTestDTO{TestConnectionResult: probe}
	if u := untrustedJMAPCert(err); u != nil {
		result.Untrusted = []UntrustedCertDTO{*u}
	}
	return result, nil
}

// AddAccountRequest is the metadata the wizard collected. For password auth
// Password is set; for oauth Provider and ClientID are set and the flow runs.
type AddAccountRequest struct {
	Email string `json:"email"`
	// DisplayName is the From name recipients see. LocalLabel is what this app
	// calls the mailbox instead when UseLocalLabel is set, and goes nowhere near
	// an outgoing message.
	DisplayName   string `json:"displayName"`
	LocalLabel    string `json:"localLabel"`
	UseLocalLabel bool   `json:"useLocalLabel"`
	// Username is the login name when it differs from the email; empty logs in
	// with Email.
	Username string `json:"username"`
	IMAPHost string `json:"imapHost"`
	IMAPPort int    `json:"imapPort"`
	SMTPHost string `json:"smtpHost"`
	SMTPPort int    `json:"smtpPort"`
	// IMAPTLS and SMTPTLS pin the connection security: "ssl", "starttls", or
	// empty to derive it from the port.
	IMAPTLS string `json:"imapTls"`
	SMTPTLS string `json:"smtpTls"`
	// auth
	Password string `json:"password"`
	Provider string `json:"provider"`
	ClientID string `json:"clientId"`
	// ClientSecret is required by Google Desktop app clients and optional for
	// Microsoft Entra apps registered as confidential clients. Empty keeps the
	// public-client PKCE flow.
	ClientSecret string `json:"clientSecret"`
	// Protocol is "imap" or "jmap"; empty means imap. Client-supplied session
	// URLs and account ids are ignored; the backend re-authenticates.
	Protocol string `json:"protocol"`
	// TrustedCerts and CAPEM are the certificates and CA the mailbox trusts
	// beyond the system roots, as accepted in the connection test.
	TrustedCerts []string `json:"trustedCerts"`
	CAPEM        string   `json:"caPem"`
	// Proxy is the route the mailbox's connections take (#457).
	Proxy AccountProxyDTO `json:"proxy"`
}

// PendingAccount is an OAuth flow that has tokens but no account row yet.
type PendingAccount struct {
	ID string `json:"id"`
	TestConnectionResult
}

// pendingAccount holds the wizard state for BeginOAuthAccount until finish/cancel.
type pendingAccount struct {
	req    AddAccountRequest
	secret credentials.Secret
	probe  TestConnectionResult
}

// targetAuth is the trusted JMAP session derived from a fresh authentication.
type targetAuth struct {
	SessionURL    string
	MailAccountID string
}

// AddPasswordAccount creates a password-authenticated account: it stores the
// metadata, files the password in the keyring, discovers the folder tree and
// runs an initial sync.
func (a *App) AddPasswordAccount(req AddAccountRequest) (AccountDTO, error) {
	if err := a.ready(); err != nil {
		return AccountDTO{}, err
	}
	secret := credentials.Secret{Method: credentials.MethodPassword, Password: req.Password}
	return a.createAccount(req, secret)
}

// AddOAuthAccount creates an oauth account via the legacy one-shot path (IMAP).
// New wizard flows use BeginOAuthAccount / FinishAddAccount instead.
func (a *App) AddOAuthAccount(req AddAccountRequest) (AccountDTO, error) {
	if err := a.ready(); err != nil {
		return AccountDTO{}, err
	}
	client, err := a.newAccountOAuthClient(req.Proxy)
	if err != nil {
		return AccountDTO{}, err
	}
	secret, err := a.authorizeOAuth(client, req.Provider, req.ClientID, req.ClientSecret, req.Email)
	if err != nil {
		return AccountDTO{}, err
	}
	req.Protocol = "imap"
	return a.createAccount(req, secret)
}

// newAccountOAuthClient is the http client for the token exchange of a mailbox
// that does not exist yet: along the route the wizard chose, or the app-wide
// way when that route sends sign-in there.
func (a *App) newAccountOAuthClient(dto AccountProxyDTO) (*http.Client, error) {
	route, err := accountProxyFromDTO(dto)
	if err != nil {
		return nil, err
	}
	if route.OAuthUseGlobal {
		return a.httpClient(oauthTimeout), nil
	}
	_, cfg, err := a.routeFromDTO(dto, 0)
	if err != nil {
		return nil, err
	}
	return cfg.HTTPClient(oauthTimeout), nil
}

// authorizeOAuth runs the interactive consent flow in the system browser and
// returns the keyring secret for the tokens it yields. client carries the code
// exchange, which is the one part of the flow the app sends itself.
func (a *App) authorizeOAuth(client *http.Client, provider, clientID, clientSecret, email string) (credentials.Secret, error) {
	ctx, cancel := context.WithTimeout(oauthContext(a.ctx, client), oauthFlowTimeout)
	defer cancel()

	token, err := a.authorize(ctx, provider, clientID, clientSecret, email, func(url string) {
		wailsruntime.BrowserOpenURL(a.ctx, url)
	})
	if err != nil {
		return credentials.Secret{}, err
	}
	if token.RefreshToken == "" {
		return credentials.Secret{}, fmt.Errorf("pelton: provider returned no refresh token; re-consent may be required")
	}
	return credentials.Secret{
		Method:       credentials.MethodOAuth,
		Provider:     provider,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RefreshToken: token.RefreshToken,
		AccessToken:  token.AccessToken,
		Expiry:       token.Expiry,
	}, nil
}

// BeginOAuthAccount runs PKCE, probes JMAP with the access token, and stores the
// secret in memory under a random id. It does not insert an account row.
func (a *App) BeginOAuthAccount(req AddAccountRequest) (PendingAccount, error) {
	if err := a.ready(); err != nil {
		return PendingAccount{}, err
	}

	client, err := a.newAccountOAuthClient(req.Proxy)
	if err != nil {
		return PendingAccount{}, err
	}
	secret, err := a.authorizeOAuth(client, req.Provider, req.ClientID, req.ClientSecret, req.Email)
	if err != nil {
		return PendingAccount{}, err
	}
	username := req.Username
	if username == "" {
		username = req.Email
	}
	// the token goes only where the mailbox's route goes. A route that cannot
	// be resolved skips the probe: the account is then offered as IMAP.
	var probe TestConnectionResult
	if _, route, err := a.routeFromDTO(req.Proxy, 0); err == nil {
		trust := certtrust.Trust{Pins: normalizePins(req.TrustedCerts), CAPEM: req.CAPEM}
		probe, _ = a.probeJMAP(a.ctx, route, trust, req.Email, username, secret.AccessToken, true, req.IMAPHost)
	}

	id, err := randomPendingID()
	if err != nil {
		return PendingAccount{}, err
	}
	a.pendingMu.Lock()
	if a.pending == nil {
		a.pending = make(map[string]pendingAccount)
	}
	a.pending[id] = pendingAccount{req: req, secret: secret, probe: probe}
	a.pendingMu.Unlock()

	return PendingAccount{ID: id, TestConnectionResult: probe}, nil
}

// FinishAddAccount creates the account for a pending OAuth entry with the chosen
// protocol, then removes the pending entry. The entry stays until create succeeds
// so a failed create does not drop the OAuth secret.
func (a *App) FinishAddAccount(pendingID, protocol string) (AccountDTO, error) {
	if err := a.ready(); err != nil {
		return AccountDTO{}, err
	}
	protocol = normalizeProtocol(protocol)
	if protocol != "imap" && protocol != "jmap" {
		return AccountDTO{}, fmt.Errorf("pelton: unsupported protocol %q", protocol)
	}

	a.pendingMu.Lock()
	entry, ok := a.pending[pendingID]
	a.pendingMu.Unlock()
	if !ok {
		return AccountDTO{}, fmt.Errorf("pelton: unknown pending account %q", pendingID)
	}

	entry.req.Protocol = protocol
	dto, err := a.createAccount(entry.req, entry.secret)
	if err != nil {
		return AccountDTO{}, err
	}

	a.pendingMu.Lock()
	delete(a.pending, pendingID)
	a.pendingMu.Unlock()
	return dto, nil
}

// CancelAddAccount drops a pending OAuth entry without touching the database.
func (a *App) CancelAddAccount(pendingID string) error {
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	if a.pending != nil {
		delete(a.pending, pendingID)
	}
	return nil
}

// ProbeAccount loads the account secret, refreshes OAuth if needed, and probes
// JMAP. It does not change the stored protocol.
func (a *App) ProbeAccount(accountID int64) (TestConnectionResult, error) {
	if err := a.ready(); err != nil {
		return TestConnectionResult{}, err
	}
	account, err := a.store.GetAccount(a.ctx, accountID)
	if err != nil {
		return TestConnectionResult{}, err
	}
	probe, _, err := a.probeAccountJMAP(*account)
	return probe, err
}

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

// SwitchProtocol changes an account between imap and jmap after authenticating
// the target. Address books stay. The cached mail is deleted in batches (see
// storage.SwitchAccountProtocol), so the account's sync is held for the whole
// switch: nothing can start a scheduler or a sync job for it until the new
// worker runs, or the old worker again if the switch fails. Clearing the cache
// is cancelled when it stops making progress (switchProtocolStallTimeout) and
// retried once; when that fails too the account keeps its old protocol and the
// error is errProtocolSwitchTimedOut.
func (a *App) SwitchProtocol(accountID int64, protocol string) error {
	if err := a.ready(); err != nil {
		return err
	}
	protocol = normalizeProtocol(protocol)
	if protocol != "imap" && protocol != "jmap" {
		return fmt.Errorf("pelton: unsupported protocol %q", protocol)
	}

	account, err := a.store.GetAccount(a.ctx, accountID)
	if err != nil {
		return err
	}
	if account.Local {
		return fmt.Errorf("pelton: local account has no protocol to switch")
	}
	secret, err := credentials.Load(accountID)
	if errors.Is(err, credentials.ErrNotFound) {
		return errNoCredentials
	}
	if err != nil {
		return err
	}

	route, err := a.accountRoute(*account)
	if err != nil {
		return err
	}
	auth, err := a.runAuthenticateTarget(a.ctx, *account, route, protocol, secret)
	if err != nil {
		return err
	}

	// Hold before stopping, so a Sync, message open, scroll, limit raise or
	// push arriving mid-switch cannot start a fresh scheduler whose jobs write
	// old-protocol rows between the delete batches. Every return below starts
	// a worker with startHeldAccountWorker first; its calls pass the hold
	// under its own context, so the deferred release can run after it.
	release := a.holdAccountSync(accountID)
	defer release()
	// Each step's time goes to the debug log: a slow switch has to show
	// whether it waited on the old worker, the account lock, the cache
	// delete or folder discovery.
	switchStarted := time.Now()
	step := switchStarted
	logStep := func(name string) {
		now := time.Now()
		a.log.Debug("protocol switch step", "account", accountID, "step", name, "ms", now.Sub(step).Milliseconds())
		step = now
	}
	defer func() {
		a.log.Debug("protocol switch done", "account", accountID, "to", protocol, "total_ms", time.Since(switchStarted).Milliseconds())
	}()
	// Stops the worker (IDLE or JMAP watch), cancels and joins the account's
	// scheduler jobs and closes its IMAP sessions. Other accounts keep theirs.
	a.stopAccountWorker(accountID)
	// Force-abort this account's sync still holding its lock (a stuck FETCH
	// ignores worker cancel until the IMAP connection is closed).
	a.abortAccountSync(accountID)
	logStep("stop worker")

	sessionURL, mailID := auth.SessionURL, auth.MailAccountID
	if protocol == "imap" {
		sessionURL, mailID = "", ""
	}
	// Hold this account's lock across the switch so a mailbox mutation on it
	// (archive, move, download) cannot recreate the live attachment dir while
	// it is renamed aside. Abort + wait; a second abort covers a session
	// opened after the first one.
	if err := a.lockAccount(accountID, accountLockWait); err != nil {
		a.abortAccountSync(accountID)
		if err = a.lockAccount(accountID, accountLockWait); err != nil {
			a.startHeldAccountWorker(accountID)
			return err
		}
	}
	logStep("account lock")
	err = a.clearCacheForSwitch(accountID, protocol, sessionURL, mailID)
	a.accountLock(accountID).Unlock()
	logStep("clear cache")
	// the old worker is stopped; whatever the new protocol is, it starts from
	// fresh connections.
	a.dropJMAPHTTPClient(accountID)
	if err != nil {
		a.startHeldAccountWorker(accountID)
		return err
	}

	account, err = a.store.GetAccount(a.ctx, accountID)
	if err != nil {
		a.startHeldAccountWorker(accountID)
		return err
	}

	discCtx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	if err := a.discoverFoldersCtx(discCtx, *account); err != nil {
		a.log.Error("discover folders after protocol switch", "account", account.Email, "err", err)
	}
	cancel()
	logStep("discover folders")
	a.startHeldAccountWorker(accountID)

	// CardDAV discovery + first contact sync can exceed the settings UI budget
	// (contactsTimeout is 30s). Never block the protocol switch on it.
	if protocol == "jmap" && secret.Method == credentials.MethodPassword {
		acc := *account
		password := secret.Password
		goSafe("carddav after jmap switch", func() {
			_ = a.runProvisionCardDAV(a.ctx, acc, password)
		})
	}
	return nil
}

// errClearCacheStalled is the cancel cause clearCacheForSwitch's watchdog
// gives, so a stall is told apart from shutdown.
var errClearCacheStalled = errors.New("pelton: clearing the cache made no progress")

// clearCacheForSwitch runs storage.SwitchAccountProtocol and cancels it once
// switchProtocolStallTimeout passes without a delete batch committing; a long
// delete that keeps moving is never cut off. A stalled attempt is retried
// once, after the account's sync is aborted again: the batched delete is
// idempotent and the stored protocol changes only in its last transaction, so
// the retry carries on from wherever the first attempt stopped. Once the first
// attempt has stalled, any failure of the retry wraps errProtocolSwitchTimedOut.
func (a *App) clearCacheForSwitch(accountID int64, protocol, sessionURL, mailID string) error {
	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithCancelCause(a.ctx)
		var lastProgress atomic.Int64
		lastProgress.Store(time.Now().UnixNano())
		watchdog := time.AfterFunc(switchProtocolStallTimeout, func() { cancel(errClearCacheStalled) })
		err := a.store.SwitchAccountProtocol(ctx, accountID, protocol, sessionURL, mailID, func() {
			lastProgress.Store(time.Now().UnixNano())
			watchdog.Reset(switchProtocolStallTimeout)
		})
		watchdog.Stop()
		// judged by the cancel cause, not the error: a driver may report an
		// interrupted statement without wrapping the context's error.
		stalled := err != nil && errors.Is(context.Cause(ctx), errClearCacheStalled)
		cancel(nil)
		if !stalled {
			if err != nil && attempt > 1 {
				return fmt.Errorf("%w: %w", errProtocolSwitchTimedOut, err)
			}
			return err
		}
		pool := a.store.PoolStats()
		a.log.Warn("protocol switch step stalled", "account", accountID, "step", "clear cache",
			"ms_since_progress", time.Since(time.Unix(0, lastProgress.Load())).Milliseconds(), "attempt", attempt,
			"db_in_use", pool.InUse, "db_idle", pool.Idle, "db_wait_count", pool.WaitCount,
			"db_wait_ms", pool.WaitDuration.Milliseconds(), "wal_bytes", a.store.WALSize(),
			"goroutines", a.dumpGoroutines("switch-stall"))
		if attempt == 2 {
			return fmt.Errorf("%w: %w", errProtocolSwitchTimedOut, err)
		}
		a.abortAccountSync(accountID)
	}
}

// createAccount is the shared path for both auth methods: persist metadata, store
// the secret, discover folders, sync, and start idling. On any failure after the
// row is created it rolls the account back so a half-created account is not left.
func (a *App) createAccount(req AddAccountRequest, secret credentials.Secret) (AccountDTO, error) {
	if !validTLSMode(req.IMAPTLS) || !validTLSMode(req.SMTPTLS) {
		return AccountDTO{}, errUnknownTLSMode
	}
	protocol := normalizeProtocol(req.Protocol)
	if protocol != "imap" && protocol != "jmap" {
		return AccountDTO{}, fmt.Errorf("pelton: unsupported protocol %q", protocol)
	}
	route, err := accountProxyFromDTO(req.Proxy)
	if err != nil {
		return AccountDTO{}, err
	}
	account := &storage.Account{
		Email:         req.Email,
		DisplayName:   req.DisplayName,
		LocalLabel:    req.LocalLabel,
		UseLocalLabel: req.UseLocalLabel,
		Username:      req.Username,
		IMAPHost:      req.IMAPHost,
		IMAPPort:      req.IMAPPort,
		SMTPHost:      req.SMTPHost,
		SMTPPort:      req.SMTPPort,
		IMAPTLS:       req.IMAPTLS,
		SMTPTLS:       req.SMTPTLS,
		Protocol:      protocol,
		TrustedCerts:  normalizePins(req.TrustedCerts),
		CAPEM:         req.CAPEM,
		Proxy:         route,
	}
	if req.CAPEM != "" {
		if _, err := certtrust.ParseCA(req.CAPEM); err != nil {
			return AccountDTO{}, err
		}
	}

	if protocol == "jmap" {
		// the route's password is not filed yet: take it from the request.
		_, routeCfg, err := a.routeFromDTO(req.Proxy, 0)
		if err != nil {
			return AccountDTO{}, err
		}
		auth, err := a.runAuthenticateTarget(a.ctx, *account, routeCfg, "jmap", secret)
		if err != nil {
			return AccountDTO{}, err
		}
		account.JMAPSessionURL = auth.SessionURL
		account.JMAPMailAccountID = auth.MailAccountID
	}

	id, err := a.store.CreateAccount(a.ctx, account)
	if err != nil {
		return AccountDTO{}, err
	}

	if err := credentials.Store(id, secret); err != nil {
		_ = a.store.DeleteAccount(a.ctx, id)
		return AccountDTO{}, err
	}
	if err := saveAccountProxyPassword(id, route, req.Proxy); err != nil {
		_ = credentials.Delete(id)
		_ = a.store.DeleteAccount(a.ctx, id)
		return AccountDTO{}, err
	}

	if err := a.discoverFolders(*account); err != nil {
		// keep the account; folders can be (re)discovered on next sync. surface
		// the error so the wizard can warn, but the account exists.
		a.log.Error("discover folders", "account", account.Email, "err", err)
	}

	// Password JMAP mailboxes get CardDAV books auto-added. Failures are logged
	// inside provision and never fail account create. OAuth never runs this.
	if secret.Method == credentials.MethodPassword && protocol == "jmap" {
		_ = a.runProvisionCardDAV(a.ctx, *account, secret.Password)
	}

	// no sync yet. The wizard shows the discovered folders next so a huge
	// archive can be unchecked before anything is fetched, and calls
	// StartAccountSync once that choice is made (#173). Starting here would
	// download the folders the user is about to say they do not want.
	return toAccountDTO(*account), nil
}

// StartAccountSync runs the first sync of an account and parks it on idle (or
// JMAP watch), replacing any existing worker for that account.
func (a *App) StartAccountSync(accountID int64) error {
	if err := a.ready(); err != nil {
		return err
	}
	if _, err := a.store.GetAccount(a.ctx, accountID); err != nil {
		return err
	}
	a.startAccountWorker(accountID)
	return nil
}

func (a *App) runProvisionCardDAV(ctx context.Context, account storage.Account, password string) error {
	if a.provisionCardDAV != nil {
		return a.provisionCardDAV(ctx, account, password)
	}
	return a.provisionCardDAVBooks(ctx, account, password)
}

// runAuthenticateTarget signs in to account with protocol. route is the one
// its JMAP probe takes: a mailbox being created has no stored route password
// yet, so the caller resolves it.
func (a *App) runAuthenticateTarget(ctx context.Context, account storage.Account, route proxy.Config, protocol string, secret credentials.Secret) (targetAuth, error) {
	fn := a.authenticateTarget
	if fn == nil {
		fn = a.defaultAuthenticateTarget
	}
	return fn(ctx, account, route, protocol, secret)
}

func (a *App) defaultAuthenticateTarget(ctx context.Context, account storage.Account, route proxy.Config, protocol string, secret credentials.Secret) (targetAuth, error) {
	protocol = normalizeProtocol(protocol)
	switch protocol {
	case "imap":
		cfg, err := a.imapConfigFromSecret(account, secret)
		if err != nil {
			return targetAuth{}, err
		}
		client, err := a.connectIMAP(cfg)
		if err != nil {
			return targetAuth{}, err
		}
		defer client.Close()
		if err := client.Login(); err != nil {
			return targetAuth{}, err
		}
		_ = client.Logout()
		return targetAuth{}, nil
	case "jmap":
		username := loginName(account)
		var token string
		bearer := false
		switch secret.Method {
		case credentials.MethodOAuth:
			bearer = true
			if account.ID == 0 {
				if secret.AccessToken == "" {
					return targetAuth{}, fmt.Errorf("pelton: oauth account has no access token")
				}
				token = secret.AccessToken
			} else {
				var err error
				token, err = a.freshAccessToken(account, secret)
				if err != nil {
					return targetAuth{}, err
				}
			}
		default:
			token = secret.Password
		}
		probe, err := a.probeJMAP(ctx, route, accountTrust(account), account.Email, username, token, bearer, account.IMAPHost)
		if err != nil {
			// an untrusted certificate, named as one so it can be reviewed, or a
			// refused secret, so the password prompt can be raised.
			return targetAuth{}, err
		}
		if !probe.JMAPAvailable {
			return targetAuth{}, fmt.Errorf("pelton: JMAP is not available for this account")
		}
		return targetAuth{
			SessionURL:    probe.JMAPSessionURL,
			MailAccountID: probe.JMAPMailAccountID,
		}, nil
	default:
		return targetAuth{}, fmt.Errorf("pelton: unsupported protocol %q", protocol)
	}
}

func (a *App) imapConfigFromSecret(account storage.Account, secret credentials.Secret) (pimap.Config, error) {
	dial, err := a.accountDial(account)
	if err != nil {
		return pimap.Config{}, err
	}
	cfg := pimap.Config{
		Host:     account.IMAPHost,
		Port:     account.IMAPPort,
		Username: loginName(account),
		TLS:      imapTLSMode(account.IMAPTLS),
		Trust:    accountTrust(account),
		Dial:     dial,
	}
	switch secret.Method {
	case credentials.MethodOAuth:
		token, err := a.freshAccessToken(account, secret)
		if err != nil {
			if secret.AccessToken == "" {
				return pimap.Config{}, err
			}
			cfg.OAuth2Token = secret.AccessToken
		} else {
			cfg.OAuth2Token = token
		}
	default:
		cfg.Password = secret.Password
	}
	return cfg, nil
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

func normalizeProtocol(p string) string {
	p = strings.TrimSpace(strings.ToLower(p))
	if p == "" {
		return "imap"
	}
	return p
}

func randomPendingID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// discoverFolders lists the server's mailboxes and creates the storage folder
// rows, preserving the hierarchy via the per-server delimiter so the sidebar
// tree matches the server.
func (a *App) discoverFolders(account storage.Account) error {
	return a.discoverFoldersCtx(a.ctx, account)
}

func (a *App) discoverFoldersCtx(ctx context.Context, account storage.Account) error {
	if account.Protocol == "jmap" {
		return a.discoverFoldersJMAP(ctx, account)
	}
	cfg, err := a.resolveIMAP(account)
	if err != nil {
		return err
	}
	client, err := a.connectIMAP(cfg)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.Login(); err != nil {
		return err
	}
	defer client.Logout()

	folders, err := client.ListFolders()
	if err != nil {
		return err
	}
	return a.createFolderTree(account.ID, folders)
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

// ensureFolders discovers the folder tree over an already-connected client when
// the account has no folder rows yet: accounts restored from a backup import,
// or whose discovery failed during setup. With folders present it is a no-op.
func (a *App) ensureFolders(client mailClient, accountID int64) error {
	folders, err := a.store.ListFolders(a.ctx, accountID)
	if err != nil {
		return err
	}
	if len(folders) > 0 {
		return nil
	}
	serverFolders, err := client.ListFolders()
	if err != nil {
		return err
	}
	return a.createFolderTree(accountID, serverFolders)
}

// createFolderTree inserts folders parent-first so each child can resolve its
// parent id from the path above it, splitting on the server's delimiter.
func (a *App) createFolderTree(accountID int64, folders []pimap.Folder) error {
	// shallowest first so parents exist before their children.
	sort.SliceStable(folders, func(i, j int) bool {
		return depth(folders[i]) < depth(folders[j])
	})

	byPath := make(map[string]int64)
	for _, f := range folders {
		if f.HasAttr(goimap.MailboxAttrNonExistent) {
			continue
		}
		delim := delimString(f.Delimiter)
		name := f.Name
		var parentID *int64
		if delim != "" {
			if parent, last, ok := strings.CutLast(f.Name, delim); ok {
				name = last
				if pid, ok := byPath[parent]; ok {
					parentID = &pid
				}
			}
		}

		row := &storage.Folder{
			AccountID:  accountID,
			Name:       name,
			IMAPPath:   f.Name,
			RemoteID:   f.Name,
			Delimiter:  delim,
			ParentID:   parentID,
			Attributes: attrsToStrings(f.Attrs),
		}
		id, err := a.store.CreateFolder(a.ctx, row)
		if err != nil {
			return err
		}
		byPath[f.Name] = id
	}
	return nil
}

// depth counts how many delimiter segments a folder path has, for sort order.
func depth(f pimap.Folder) int {
	d := delimString(f.Delimiter)
	if d == "" {
		return 0
	}
	return strings.Count(f.Name, d)
}

// delimString renders the rune delimiter as a string, empty for a flat server.
func delimString(r rune) string {
	if r == 0 {
		return ""
	}
	return string(r)
}

// attrsToStrings converts imap mailbox attributes to plain strings for storage.
func attrsToStrings(attrs []goimap.MailboxAttr) []string {
	out := make([]string, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, string(a))
	}
	return out
}
