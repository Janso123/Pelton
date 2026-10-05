package desktop

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gojmap "github.com/Janso123/go-jmap"
	"github.com/Janso123/go-jmap/core"
	"github.com/Janso123/go-jmap/core/push/websocket"
	"golang.org/x/oauth2"

	"github.com/peltonapp/Pelton/internal/certtrust"
	"github.com/peltonapp/Pelton/internal/credentials"
	pimap "github.com/peltonapp/Pelton/internal/imap"
	pjmap "github.com/peltonapp/Pelton/internal/jmap"
	"github.com/peltonapp/Pelton/internal/outbox"
	"github.com/peltonapp/Pelton/internal/proxy"
	"github.com/peltonapp/Pelton/internal/storage"
)

// hiddenJMAPHost and hiddenMailDomain do not resolve, so a connection that
// reaches them went through tunnelProxy, which is the only thing that knows
// where they are.
const (
	hiddenJMAPHost   = "jmap.pelton.invalid"
	hiddenMailDomain = "mail-domain.pelton.invalid"
)

// tunnelProxy is an HTTP CONNECT proxy that sends every tunnel to upstream,
// whatever host was asked for, and records the targets it was asked for.
type tunnelProxy struct {
	host string
	port int

	mu      sync.Mutex
	targets []string
}

func newTunnelProxy(t *testing.T, upstream string) *tunnelProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p := &tunnelProxy{host: host}
	p.port, _ = strconv.Atoi(port)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(conn, upstream)
		}
	}()
	return p
}

func (p *tunnelProxy) serve(conn net.Conn, upstream string) {
	defer conn.Close()
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil || req.Method != http.MethodConnect {
		return
	}
	p.mu.Lock()
	p.targets = append(p.targets, req.Host)
	p.mu.Unlock()
	up, err := net.Dial("tcp", upstream)
	if err != nil {
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer up.Close()
	_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go func() { _, _ = io.Copy(up, conn) }()
	_, _ = io.Copy(conn, up)
}

func (p *tunnelProxy) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.targets...)
}

func (p *tunnelProxy) route() storage.AccountProxy {
	return storage.AccountProxy{Mode: storage.AccountProxyManual, Scheme: proxy.SchemeHTTP, Host: p.host, Port: p.port}
}

// jmapTLSServer is a JMAP server on httptest's self-signed certificate. base
// is the origin it names in its session document, which is how it is reached.
// newJMAPPlainServer starts the same server without TLS.
type jmapTLSServer struct {
	srv *httptest.Server

	mu     sync.Mutex
	paths  []string
	opened int
	closed int
}

func newJMAPTLSServer(t *testing.T, base func() string) *jmapTLSServer {
	t.Helper()
	s := &jmapTLSServer{}
	s.srv = httptest.NewUnstartedServer(s.handler(base))
	s.srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch state {
		case http.StateNew:
			s.opened++
		case http.StateClosed:
			s.closed++
		}
	}
	s.srv.StartTLS()
	t.Cleanup(s.srv.Close)
	return s
}

// conns is how many connections, each with its own TLS handshake, the server
// accepted and how many have closed.
func (s *jmapTLSServer) conns() (opened, closed int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened, s.closed
}

func newJMAPPlainServer(t *testing.T, base func() string) *jmapTLSServer {
	t.Helper()
	s := &jmapTLSServer{}
	s.srv = httptest.NewServer(s.handler(base))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *jmapTLSServer) handler(base func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.paths = append(s.paths, r.URL.Path)
		s.mu.Unlock()
		auth := r.Header.Get("Authorization")
		if r.URL.Path != "/ws" && auth != "Basic "+base64.StdEncoding.EncodeToString([]byte("user@example.com:pw")) && auth != "Bearer oauth-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/.well-known/jmap":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, jmapRouteSession(base()))
		case r.URL.Path == "/api/":
			var body struct {
				MethodCalls [][]json.RawMessage `json:"methodCalls"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"methodResponses": body.MethodCalls, "sessionState": "s1"})
		case strings.HasPrefix(r.URL.Path, "/download/"):
			_, _ = io.WriteString(w, "blob")
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accountId":"A1","blobId":"B2","type":"text/plain","size":4}`)
		case r.URL.Path == "/ws":
			acceptWebSocket(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (s *jmapTLSServer) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *jmapTLSServer) addr() string { return s.srv.Listener.Addr().String() }

func (s *jmapTLSServer) caPEM() string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.srv.Certificate().Raw}))
}

// acceptWebSocket completes the RFC 6455 handshake with the jmap subprotocol
// and holds the connection open, which is all a dial needs to succeed.
func acceptWebSocket(w http.ResponseWriter, r *http.Request) {
	sum := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	conn, buf, err := w.(http.Hijacker).Hijack()
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(buf, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\nSec-WebSocket-Protocol: jmap\r\n\r\n",
		base64.StdEncoding.EncodeToString(sum[:]))
	_ = buf.Flush()
	go func() {
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}()
}

func jmapRouteSession(base string) string {
	ws := "wss" + strings.TrimPrefix(base, "https")
	return `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000, "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000, "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32, "maxObjectsInGet": 256, "maxObjectsInSet": 128,
      "collationAlgorithms": ["i;ascii-casemap"]
    },
    "urn:ietf:params:jmap:mail": {},
    "urn:ietf:params:jmap:submission": {},
    "urn:ietf:params:jmap:websocket": {"url": "` + ws + `/ws", "supportsPush": true}
  },
  "accounts": {
    "A1": {
      "name": "user@example.com", "isPersonal": true, "isReadOnly": false,
      "accountCapabilities": {"urn:ietf:params:jmap:mail": {}, "urn:ietf:params:jmap:submission": {}}
    }
  },
  "primaryAccounts": {"urn:ietf:params:jmap:mail": "A1", "urn:ietf:params:jmap:submission": "A1"},
  "username": "user@example.com",
  "apiUrl": "` + base + `/api/",
  "downloadUrl": "` + base + `/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "` + base + `/upload/{accountId}/",
  "eventSourceUrl": "` + base + `/eventsource/",
  "state": "s1"
}`
}

// jmapRouteAccount stores a JMAP mailbox reaching its session at base, with a
// password in the keyring.
func jmapRouteAccount(t *testing.T, a *App, base string, edit func(*storage.Account)) storage.Account {
	t.Helper()
	account := storage.Account{
		Email: "user@example.com", IMAPHost: "127.0.0.1", IMAPPort: 1, SMTPHost: "127.0.0.1", SMTPPort: 1,
		Protocol: "jmap", JMAPSessionURL: base + "/.well-known/jmap", JMAPMailAccountID: "A1",
	}
	if edit != nil {
		edit(&account)
	}
	id, err := a.store.CreateAccount(a.ctx, &account)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := credentials.Store(id, credentials.Secret{Method: credentials.MethodPassword, Password: "pw"}); err != nil {
		t.Fatalf("store credentials: %v", err)
	}
	t.Cleanup(func() { _ = credentials.Delete(id) })
	stored, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return *stored
}

func pinOf(s *jmapTLSServer) string { return certtrust.Fingerprint(s.srv.Certificate()) }

// Every request a JMAP mailbox makes (session, method calls, blob download
// and upload, the push socket) has to take its proxy, and trust the
// certificate it pinned. The server's name only resolves inside the proxy, so
// anything that went around it fails.
func TestJMAPClientTakesTheMailboxProxyAndTrust(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	base := "https://" + hiddenJMAPHost
	srv := newJMAPTLSServer(t, func() string { return base })
	tunnel := newTunnelProxy(t, srv.addr())
	account := jmapRouteAccount(t, a, base, func(acc *storage.Account) {
		acc.Proxy = tunnel.route()
		acc.TrustedCerts = []string{pinOf(srv)}
	})

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	client, err := a.jmapClient(ctx, &account)
	if err != nil {
		t.Fatalf("jmapClient: %v", err)
	}
	req := &gojmap.Request{}
	req.Invoke(core.Echo{"hello": "proxy"})
	if _, err := client.Do(ctx, req); err != nil {
		t.Fatalf("api call: %v", err)
	}
	body, err := client.Download(ctx, "A1", "B1", gojmap.DownloadOptions{})
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	_ = body.Close()
	if _, err := client.Upload(ctx, "A1", strings.NewReader("data"), "text/plain"); err != nil {
		t.Fatalf("upload: %v", err)
	}
	conn, err := websocket.Dial(ctx, client.Client)
	if err != nil {
		t.Fatalf("websocket: %v", err)
	}
	_ = conn.Close()

	paths := srv.seen()
	for _, want := range []string{"/.well-known/jmap", "/api/", "/download/A1/B1/filename", "/upload/A1/", "/ws"} {
		if !slices.Contains(paths, want) {
			t.Errorf("server never saw %s (saw %v)", want, paths)
		}
	}
	for _, target := range tunnel.seen() {
		if target != hiddenJMAPHost+":443" {
			t.Errorf("proxy tunnelled to %q, want only the JMAP host", target)
		}
	}
}

// A proxy that is not there fails the connection; it never falls back to
// going direct, which would reveal the address the proxy was there to hide.
func TestJMAPClientFailsClosedWhenTheProxyIsDown(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	var base string
	srv := newJMAPPlainServer(t, func() string { return base })
	base = "http://" + srv.addr()
	deadHost, deadPortN := deadPort(t)
	account := jmapRouteAccount(t, a, base, func(acc *storage.Account) {
		acc.Proxy = storage.AccountProxy{Mode: storage.AccountProxyManual, Scheme: proxy.SchemeSOCKS5, Host: deadHost, Port: deadPortN}
	})

	if _, err := a.jmapClient(a.ctx, &account); err == nil {
		t.Fatal("jmapClient connected with its proxy down")
	}
	if paths := srv.seen(); len(paths) != 0 {
		t.Errorf("server was reached directly: %v", paths)
	}
}

func TestJMAPClientCertificateTrust(t *testing.T) {
	for _, tt := range []struct {
		name    string
		trust   func(*jmapTLSServer, *storage.Account)
		trusted bool
	}{
		{"untrusted", func(*jmapTLSServer, *storage.Account) {}, false},
		{"pinned", func(s *jmapTLSServer, acc *storage.Account) { acc.TrustedCerts = []string{pinOf(s)} }, true},
		{"custom CA", func(s *jmapTLSServer, acc *storage.Account) { acc.CAPEM = s.caPEM() }, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newAccountTestApp(t)
			a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
			var base string
			srv := newJMAPTLSServer(t, func() string { return base })
			base = "https://" + srv.addr()
			account := jmapRouteAccount(t, a, base, func(acc *storage.Account) { tt.trust(srv, acc) })

			_, err := a.jmapClient(a.ctx, &account)
			if tt.trusted {
				if err != nil {
					t.Fatalf("jmapClient: %v", err)
				}
				return
			}
			if syncFailureReason(err) != syncFailCertificate {
				t.Fatalf("jmapClient error %v classed %q, want a certificate failure", err, syncFailureReason(err))
			}
			if slices.Contains(srv.seen(), "/.well-known/jmap") {
				t.Error("the password went to a server whose certificate did not verify")
			}
		})
	}
}

// The wizard's JMAP probe runs after the IMAP login with the same password,
// so it has to take the route the user picked, not go out directly.
func TestTestConnectionProbesJMAPAlongTheChosenRoute(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	a.checkTLS = func(string, string, int, certtrust.Trust) error { return nil }
	a.newIMAPClient = func(pimap.Config) (mailClient, error) { return &fakeIMAP{}, nil }
	base := "https://" + hiddenJMAPHost
	srv := newJMAPTLSServer(t, func() string { return base })
	tunnel := newTunnelProxy(t, srv.addr())

	req := TestConnectionRequest{
		Email: "user@" + hiddenMailDomain, Username: "user@example.com", Password: "pw",
		IMAPHost: hiddenJMAPHost, IMAPPort: 993, SMTPHost: hiddenJMAPHost, SMTPPort: 465,
		Proxy: AccountProxyDTO{Mode: proxy.ModeManual, Scheme: proxy.SchemeHTTP, Host: tunnel.host, Port: tunnel.port},
	}

	// first without trusting the certificate: it comes back for review, with
	// the JMAP host, and the password is not sent.
	got, err := a.TestConnection(req)
	if err != nil {
		t.Fatalf("TestConnection: %v", err)
	}
	if got.JMAPAvailable || len(got.Untrusted) != 1 {
		t.Fatalf("result %+v, want the JMAP certificate to review", got)
	}
	u := got.Untrusted[0]
	if u.Server != "jmap" || u.Host != hiddenJMAPHost || u.Port != 443 || u.Fingerprint != pinOf(srv) {
		t.Errorf("Untrusted[0] = %+v, want the JMAP certificate at %s:443", u, hiddenJMAPHost)
	}
	if len(srv.seen()) != 0 {
		t.Errorf("the server got requests over an unverified connection: %v", srv.seen())
	}
	// both places the probe looks, the IMAP host and the address's domain,
	// were asked for through the proxy; neither was looked up or dialled
	// directly, and no SRV query went out for them.
	if got, want := tunnel.seen(), []string{hiddenJMAPHost + ":443", hiddenMailDomain + ":443"}; !slices.Equal(got, want) {
		t.Errorf("proxy tunnelled to %v, want %v", got, want)
	}

	req.TrustedCerts = []string{u.Fingerprint}
	got, err = a.TestConnection(req)
	if err != nil {
		t.Fatalf("TestConnection with the pin: %v", err)
	}
	if !got.JMAPAvailable || len(got.Untrusted) != 0 {
		t.Fatalf("result %+v, want JMAP found once the certificate is trusted", got)
	}
	if len(tunnel.seen()) == 0 {
		t.Error("the probe did not go through the chosen proxy")
	}
}

// For a JMAP mailbox the certificate that matters is the JMAP server's: the
// sync failure dialog and the editor check it, and trusting it pins it.
func TestJMAPAccountCertificatesAreTheJMAPServers(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	var base string
	srv := newJMAPTLSServer(t, func() string { return base })
	base = "https://" + srv.addr()
	account := jmapRouteAccount(t, a, base, nil)

	got, err := a.ProbeAccountCertificates(account.ID)
	if err != nil {
		t.Fatalf("ProbeAccountCertificates: %v", err)
	}
	host, port, _ := net.SplitHostPort(srv.addr())
	if len(got) != 1 || got[0].Server != "jmap" || got[0].Host != host || strconv.Itoa(got[0].Port) != port {
		t.Fatalf("probed %+v, want only the JMAP server at %s", got, srv.addr())
	}
	if err := a.TrustAccountCertificate(account.ID, got[0].Fingerprint); err != nil {
		t.Fatalf("TrustAccountCertificate: %v", err)
	}
	stored, _ := a.store.GetAccount(a.ctx, account.ID)
	if !slices.Contains(stored.TrustedCerts, pinOf(srv)) {
		t.Fatalf("trusted certs %v, want the JMAP server's pinned", stored.TrustedCerts)
	}
	if again, err := a.ProbeAccountCertificates(account.ID); err != nil || len(again) != 0 {
		t.Errorf("after trusting, probed %+v, %v; want nothing left", again, err)
	}
	if _, err := a.jmapClient(a.ctx, stored); err != nil {
		t.Errorf("jmapClient after trusting: %v", err)
	}
}

// The route test of a JMAP mailbox reaches its JMAP server, not the IMAP and
// SMTP ports it does not use.
func TestTestAccountRouteReachesTheJMAPServer(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	jmapHost, jmapPort := listen(t)
	deadHost, deadPortN := deadPort(t)
	account := jmapRouteAccount(t, a, "https://"+net.JoinHostPort(jmapHost, strconv.Itoa(jmapPort)), nil)

	err := a.TestAccountRoute(RouteTestRequest{
		AccountID: account.ID, Proxy: AccountProxyDTO{Mode: accountProxyGlobal},
		IMAPHost: deadHost, IMAPPort: deadPortN, SMTPHost: deadHost, SMTPPort: deadPortN,
	})
	if err != nil {
		t.Errorf("TestAccountRoute: %v", err)
	}

	gone := jmapRouteAccount(t, a, "https://"+net.JoinHostPort(deadHost, strconv.Itoa(deadPortN)), nil)
	err = a.TestAccountRoute(RouteTestRequest{AccountID: gone.ID, Proxy: AccountProxyDTO{Mode: accountProxyGlobal}})
	if err == nil || !strings.Contains(err.Error(), "JMAP") {
		t.Errorf("TestAccountRoute against a JMAP server that is down: %v, want a JMAP failure", err)
	}
}

// Contacts found for a JMAP mailbox are fetched along the mailbox's route
// with its certificate trust, like its mail.
func TestProvisionCardDAVTakesTheMailboxRoute(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	tunnel := newTunnelProxy(t, "127.0.0.1:1")
	account := jmapRouteAccount(t, a, "https://"+hiddenJMAPHost, func(acc *storage.Account) {
		acc.Proxy = tunnel.route()
		acc.TrustedCerts = []string{strings.Repeat("ab", 32)}
	})

	var client *http.Client
	prev := discoverCardDAV
	discoverCardDAV = func(ctx context.Context, hc *http.Client, email string) (string, error) {
		client = hc
		return "", fmt.Errorf("stop here")
	}
	t.Cleanup(func() { discoverCardDAV = prev })

	_ = a.provisionCardDAVBooks(a.ctx, account, "pw")
	if client == nil {
		t.Fatal("discovery was not called")
	}
	resp, err := client.Get("https://" + hiddenJMAPHost + "/.well-known/carddav")
	if err == nil {
		_ = resp.Body.Close()
	}
	if got := tunnel.seen(); len(got) != 1 || got[0] != hiddenJMAPHost+":443" {
		t.Errorf("proxy saw %v, want the CardDAV request", got)
	}
}

// A JMAP mailbox sends with EmailSubmission, which files the sent copy
// itself. Nothing may also append one over IMAP.
func TestTransmitJMAPDoesNotAppendToSentOverIMAP(t *testing.T) {
	imapClient := &fakeIMAP{}
	app, account := sendingAccount(t, imapClient)
	if err := app.store.SwitchAccountProtocol(app.ctx, account.ID, "jmap", "https://jmap.example/.well-known/jmap", "A1", nil); err != nil {
		t.Fatalf("switch to jmap: %v", err)
	}
	if _, err := app.store.CreateFolder(app.ctx, &storage.Folder{
		AccountID: account.ID, Name: "Sent", IMAPPath: "MbSent", RemoteID: "MbSent", Attributes: []string{`\Sent`},
	}); err != nil {
		t.Fatalf("create sent: %v", err)
	}
	app.jmapClientForTest = func(context.Context, storage.Account) (*pjmap.Client, error) {
		return nil, nil
	}
	submitted := 0
	prev := submit
	submit = func(context.Context, *pjmap.Client, string, string, []byte, string, string, []string) error {
		submitted++
		return nil
	}
	t.Cleanup(func() { submit = prev })
	app.newIMAPClient = func(pimap.Config) (mailClient, error) {
		t.Error("a JMAP send opened an IMAP connection")
		return imapClient, nil
	}

	err := (&accountTransmitter{app: app}).Transmit(context.Background(), outbox.Message{
		AccountID: account.ID, EnvelopeFrom: "me@example.com", Recipients: []string{"you@example.com"},
		Raw: []byte("Subject: hi\r\n\r\nbody\r\n"),
	})
	if err != nil {
		t.Fatalf("Transmit: %v", err)
	}
	if submitted != 1 {
		t.Errorf("submitted %d times, want 1", submitted)
	}
	if len(imapClient.appended) != 0 {
		t.Errorf("appended %d messages to Sent over IMAP, want none", len(imapClient.appended))
	}
}

// Adding a Gmail or Outlook mailbox probes JMAP with the fresh access token;
// with a proxy chosen, the token may only travel through it.
func TestBeginOAuthAccountProbesJMAPThroughTheChosenRoute(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	a.oauthAuthorize = func(context.Context, string, string, string, string, func(string)) (*oauth2.Token, error) {
		return &oauth2.Token{AccessToken: "oauth-access", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}, nil
	}
	base := "https://" + hiddenJMAPHost
	srv := newJMAPTLSServer(t, func() string { return base })
	tunnel := newTunnelProxy(t, srv.addr())

	pending, err := a.BeginOAuthAccount(AddAccountRequest{
		Email: "user@" + hiddenMailDomain, Provider: "google", ClientID: "cid",
		IMAPHost: hiddenJMAPHost, IMAPPort: 993,
		TrustedCerts: []string{pinOf(srv)},
		Proxy:        AccountProxyDTO{Mode: proxy.ModeManual, Scheme: proxy.SchemeHTTP, Host: tunnel.host, Port: tunnel.port},
	})
	if err != nil {
		t.Fatalf("BeginOAuthAccount: %v", err)
	}
	t.Cleanup(func() { _ = a.CancelAddAccount(pending.ID) })
	if !pending.JMAPAvailable {
		t.Fatalf("pending = %+v, want JMAP found through the proxy", pending)
	}
	if got := tunnel.seen(); len(got) == 0 || got[0] != hiddenJMAPHost+":443" {
		t.Errorf("proxy tunnelled to %v, want the JMAP host", got)
	}
}

// Every sync job builds a JMAP client. They share one transport per mailbox,
// so a job reuses the open connection instead of paying for a new TCP, proxy
// and TLS handshake.
func TestJMAPClientsOfAMailboxShareConnections(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	var base string
	srv := newJMAPTLSServer(t, func() string { return base })
	base = "https://" + srv.addr()
	account := jmapRouteAccount(t, a, base, func(acc *storage.Account) { acc.TrustedCerts = []string{pinOf(srv)} })

	for i := range 2 {
		client, err := a.jmapClient(a.ctx, &account)
		if err != nil {
			t.Fatalf("jmapClient %d: %v", i, err)
		}
		req := &gojmap.Request{}
		req.Invoke(core.Echo{"n": i})
		if _, err := client.Do(a.ctx, req); err != nil {
			t.Fatalf("api call %d: %v", i, err)
		}
	}
	if opened, _ := srv.conns(); opened != 1 {
		t.Errorf("server accepted %d connections, want 1 reused across both clients", opened)
	}
}

// A changed route or trust gets a new transport, and the old one's idle
// connections are closed rather than left open with the old settings.
func TestJMAPTransportIsReplacedWhenTrustChanges(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	var base string
	srv := newJMAPTLSServer(t, func() string { return base })
	base = "https://" + srv.addr()
	account := jmapRouteAccount(t, a, base, func(acc *storage.Account) { acc.TrustedCerts = []string{pinOf(srv)} })

	first, err := a.accountJMAPHTTPClient(account)
	if err != nil {
		t.Fatalf("first client: %v", err)
	}
	if _, err := a.jmapClient(a.ctx, &account); err != nil {
		t.Fatalf("jmapClient: %v", err)
	}
	if again, _ := a.accountJMAPHTTPClient(account); again != first {
		t.Fatal("unchanged settings built a new transport")
	}

	account.TrustedCerts = append(account.TrustedCerts, strings.Repeat("ab", 32))
	second, err := a.accountJMAPHTTPClient(account)
	if err != nil {
		t.Fatalf("second client: %v", err)
	}
	if second == first {
		t.Fatal("changed trust kept the old transport")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, closed := srv.conns(); closed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the old transport's idle connection was left open")
		}
		time.Sleep(10 * time.Millisecond)
	}

	account.Proxy = storage.AccountProxy{Mode: storage.AccountProxyManual, Scheme: proxy.SchemeSOCKS5, Host: "127.0.0.1", Port: 1080}
	if third, _ := a.accountJMAPHTTPClient(account); third == second {
		t.Error("changed route kept the old transport")
	}
}

func TestDeleteAccountDropsItsJMAPTransport(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	account := jmapRouteAccount(t, a, "https://"+hiddenJMAPHost, nil)
	if _, err := a.accountJMAPHTTPClient(account); err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := a.DeleteAccount(account.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	a.jmapHTTPMu.Lock()
	_, ok := a.jmapHTTP[account.ID]
	a.jmapHTTPMu.Unlock()
	if ok {
		t.Error("the removed mailbox's transport is still cached")
	}
}

// A proxy or server that accepts the connection and then says nothing must
// not hold a JMAP request, or the push watch, forever.
func TestMailHTTPClientGivesUpOnAStalledHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = conn.Close() })
		}
	}()
	prev := mailHandshakeTimeout
	mailHandshakeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { mailHandshakeTimeout = prev })

	client := mailHTTPClient(proxy.Config{Mode: proxy.ModeOff}, certtrust.Trust{}, 0)
	done := make(chan error, 1)
	go func() {
		resp, err := client.Get("https://" + ln.Addr().String() + "/")
		if err == nil {
			_ = resp.Body.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a server that never answered the handshake was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request is still waiting on a stalled handshake")
	}
}

// sessionlessJMAPAccount is a JMAP mailbox restored from a backup without its
// password: it has no session yet. withPassword stores one in the keyring.
func sessionlessJMAPAccount(t *testing.T, a *App, withPassword bool, edit func(*storage.Account)) storage.Account {
	t.Helper()
	account := storage.Account{
		Email: "user@example.com", IMAPHost: "127.0.0.1", IMAPPort: 1, SMTPHost: "127.0.0.1", SMTPPort: 1,
		Protocol: "jmap",
	}
	if edit != nil {
		edit(&account)
	}
	id, err := a.store.CreateAccount(a.ctx, &account)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if withPassword {
		if err := credentials.Store(id, credentials.Secret{Method: credentials.MethodPassword, Password: "pw"}); err != nil {
			t.Fatalf("store credentials: %v", err)
		}
		t.Cleanup(func() { _ = credentials.Delete(id) })
	}
	stored, err := a.store.GetAccount(a.ctx, id)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	return *stored
}

// A JMAP mailbox with no session finds it on first connect, along its own
// proxy and with the certificates it trusts, stores it, and hands the mail
// account id to the caller's copy.
func TestJMAPClientFindsAMissingSession(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	base := "https://" + hiddenJMAPHost
	srv := newJMAPTLSServer(t, func() string { return base })
	tunnel := newTunnelProxy(t, srv.addr())
	account := sessionlessJMAPAccount(t, a, true, func(acc *storage.Account) {
		acc.Proxy = tunnel.route()
		acc.TrustedCerts = []string{pinOf(srv)}
	})
	var calls int
	var gotRoute proxy.Config
	var gotAccount storage.Account
	var gotSecret credentials.Secret
	a.authenticateTarget = func(_ context.Context, acc storage.Account, route proxy.Config, protocol string, secret credentials.Secret) (targetAuth, error) {
		calls++
		gotRoute, gotAccount, gotSecret = route, acc, secret
		if protocol != "jmap" {
			t.Errorf("protocol = %q, want jmap", protocol)
		}
		return targetAuth{SessionURL: base + "/.well-known/jmap", MailAccountID: "A1"}, nil
	}

	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	if _, err := a.jmapClient(ctx, &account); err != nil {
		t.Fatalf("jmapClient: %v", err)
	}
	if calls != 1 {
		t.Fatalf("session looked up %d times, want 1", calls)
	}
	if gotRoute != manualProxy(tunnel.route(), "") {
		t.Errorf("route = %+v, want the mailbox proxy", gotRoute)
	}
	if gotAccount.ID != account.ID || !slices.Equal(gotAccount.TrustedCerts, []string{pinOf(srv)}) {
		t.Errorf("looked up as %+v, want the mailbox with its trusted certificates", gotAccount)
	}
	if gotSecret.Password != "pw" {
		t.Errorf("secret = %+v, want the stored password", gotSecret)
	}
	if account.JMAPSessionURL != base+"/.well-known/jmap" || account.JMAPMailAccountID != "A1" {
		t.Errorf("caller copy = %q %q, want the found session", account.JMAPSessionURL, account.JMAPMailAccountID)
	}
	stored, err := a.store.GetAccount(a.ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Protocol != "jmap" || stored.JMAPSessionURL != base+"/.well-known/jmap" || stored.JMAPMailAccountID != "A1" {
		t.Errorf("stored = %+v, want the found session", stored)
	}
	if !slices.Contains(srv.seen(), "/.well-known/jmap") {
		t.Errorf("client never fetched the found session: %v", srv.seen())
	}
}

// Without a password there is nothing to look the session up with: the
// mailbox waits for one, the same as any other mailbox missing its password.
func TestJMAPClientWithoutPasswordWaitsForOne(t *testing.T) {
	a := newAccountTestApp(t)
	account := sessionlessJMAPAccount(t, a, false, nil)
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		t.Fatal("looked the session up without a password")
		return targetAuth{}, nil
	}
	if _, err := a.jmapClient(a.ctx, &account); !errors.Is(err, errNoCredentials) {
		t.Fatalf("err = %v, want errNoCredentials", err)
	}
}

// A server that cannot be reached fails the sync and leaves the mailbox JMAP
// with nothing stored, so the next sync looks again.
func TestJMAPClientSessionLookupFailureKeepsJMAP(t *testing.T) {
	a := newAccountTestApp(t)
	account := sessionlessJMAPAccount(t, a, true, nil)
	var calls int
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		calls++
		return targetAuth{}, errors.New("server down")
	}
	for range 2 {
		if _, err := a.jmapClient(a.ctx, &account); err == nil || !strings.Contains(err.Error(), "server down") {
			t.Fatalf("err = %v, want the lookup failure", err)
		}
	}
	if calls != 2 {
		t.Errorf("session looked up %d times, want once per connect", calls)
	}
	stored, err := a.store.GetAccount(a.ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Protocol != "jmap" || stored.JMAPSessionURL != "" || stored.JMAPMailAccountID != "" {
		t.Errorf("stored = %+v, want jmap with no session", stored)
	}
}

// A mailbox proxy that cannot be resolved stops the lookup: it never goes out
// along some other route.
func TestJMAPClientSessionLookupFailsClosedOnARouteError(t *testing.T) {
	a := newAccountTestApp(t)
	account := sessionlessJMAPAccount(t, a, true, func(acc *storage.Account) {
		acc.Proxy = storage.AccountProxy{Mode: "bogus"}
	})
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		t.Fatal("looked the session up without the mailbox route")
		return targetAuth{}, nil
	}
	if _, err := a.jmapClient(a.ctx, &account); !errors.Is(err, errUnknownProxyMode) {
		t.Fatalf("err = %v, want errUnknownProxyMode", err)
	}
}

// Sync jobs hold their own copy of the account. One read before another job
// stored the session takes it from the store instead of looking it up again.
func TestJMAPClientUsesASessionAnotherConnectionFound(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	var base string
	srv := newJMAPTLSServer(t, func() string { return base })
	base = "https://" + srv.addr()
	account := sessionlessJMAPAccount(t, a, true, func(acc *storage.Account) {
		acc.TrustedCerts = []string{pinOf(srv)}
	})
	if err := a.store.SetAccountJMAPSession(a.ctx, account.ID, base+"/.well-known/jmap", "A1"); err != nil {
		t.Fatal(err)
	}
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		t.Fatal("looked up a session that is already stored")
		return targetAuth{}, nil
	}
	if _, err := a.jmapClient(a.ctx, &account); err != nil {
		t.Fatalf("jmapClient: %v", err)
	}
	if account.JMAPMailAccountID != "A1" {
		t.Errorf("mail account id = %q, want A1", account.JMAPMailAccountID)
	}
}

// A JMAP mailbox restored without its session has no JMAP server on record
// yet. Its certificate check looks for one along the mailbox route, the way the
// first sync will, so a lookup that failed on an untrusted certificate can be
// fixed from the sync failure dialog. Once trusted, the first connect finds
// the session through the proxy.
func TestSessionlessJMAPAccountCertificatesAndFirstConnect(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	base := "https://" + hiddenJMAPHost
	srv := newJMAPTLSServer(t, func() string { return base })
	tunnel := newTunnelProxy(t, srv.addr())
	account := sessionlessJMAPAccount(t, a, true, func(acc *storage.Account) {
		acc.Email = "user@" + hiddenMailDomain
		acc.Username = "user@example.com"
		acc.IMAPHost = hiddenJMAPHost
		acc.Proxy = tunnel.route()
	})

	got, err := a.ProbeAccountCertificates(account.ID)
	if err != nil {
		t.Fatalf("ProbeAccountCertificates: %v", err)
	}
	if len(got) != 1 || got[0].Server != "jmap" || got[0].Host != hiddenJMAPHost || got[0].Fingerprint != pinOf(srv) {
		t.Fatalf("probed %+v, want the JMAP certificate at %s", got, hiddenJMAPHost)
	}
	if len(srv.seen()) != 0 {
		t.Errorf("the server got requests over an unverified connection: %v", srv.seen())
	}
	if err := a.TrustAccountCertificate(account.ID, got[0].Fingerprint); err != nil {
		t.Fatalf("TrustAccountCertificate: %v", err)
	}

	stored, err := a.store.GetAccount(a.ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
	defer cancel()
	if _, err := a.jmapClient(ctx, stored); err != nil {
		t.Fatalf("jmapClient: %v", err)
	}
	if stored.JMAPSessionURL == "" || stored.JMAPMailAccountID != "A1" {
		t.Errorf("session = %q %q, want it found", stored.JMAPSessionURL, stored.JMAPMailAccountID)
	}
	for _, target := range tunnel.seen() {
		if target != hiddenJMAPHost+":443" && target != hiddenMailDomain+":443" {
			t.Errorf("proxy tunnelled to %q", target)
		}
	}
}

// A lookup that names no session or no mail account stores nothing: either
// one missing leaves the mailbox unable to sync.
func TestJMAPClientRejectsAnIncompleteSession(t *testing.T) {
	for _, tt := range []struct {
		name string
		auth targetAuth
	}{
		{"no session URL", targetAuth{MailAccountID: "A1"}},
		{"no mail account", targetAuth{SessionURL: "https://jmap.example/.well-known/jmap"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := newAccountTestApp(t)
			account := sessionlessJMAPAccount(t, a, true, nil)
			a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
				return tt.auth, nil
			}
			if _, err := a.jmapClient(a.ctx, &account); err == nil {
				t.Fatal("jmapClient accepted an incomplete session")
			}
			stored, err := a.store.GetAccount(a.ctx, account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.JMAPSessionURL != "" || stored.JMAPMailAccountID != "" || account.JMAPSessionURL != "" {
				t.Errorf("stored %+v / caller %+v, want nothing", stored, account)
			}
		})
	}
}

// A mailbox switched to IMAP while its session was being looked up keeps IMAP,
// and the lookup says why it stopped.
func TestJMAPClientSessionLookupAfterASwitchToIMAP(t *testing.T) {
	a := newAccountTestApp(t)
	account := sessionlessJMAPAccount(t, a, true, nil)
	a.authenticateTarget = func(context.Context, storage.Account, proxy.Config, string, credentials.Secret) (targetAuth, error) {
		if err := a.store.UpdateAccountProtocol(a.ctx, account.ID, "imap", "", ""); err != nil {
			t.Fatal(err)
		}
		return targetAuth{SessionURL: "https://jmap.example/.well-known/jmap", MailAccountID: "A1"}, nil
	}
	_, err := a.jmapClient(a.ctx, &account)
	if err == nil || !strings.Contains(err.Error(), "protocol changed during JMAP session lookup") {
		t.Fatalf("err = %v, want the protocol change named", err)
	}
	stored, err := a.store.GetAccount(a.ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Protocol != "imap" || stored.JMAPSessionURL != "" {
		t.Errorf("stored = %+v, want imap with no session", stored)
	}
}

// A JMAP server refusing the stored password has to mark the mailbox the same
// way an IMAP refusal does, or the password prompt never comes and every sync
// fails quietly. A later sign-in that works clears the mark.
func TestJMAPClientNotesARefusedPassword(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	var base string
	srv := newJMAPPlainServer(t, func() string { return base })
	base = "http://" + srv.addr()
	account := jmapRouteAccount(t, a, base, nil)
	if err := credentials.Store(account.ID, credentials.Secret{Method: credentials.MethodPassword, Password: "stale"}); err != nil {
		t.Fatalf("store credentials: %v", err)
	}

	_, err := a.jmapClient(a.ctx, &account)
	if err == nil {
		t.Fatal("jmapClient signed in with a refused password")
	}
	if !a.loginRejected(account.ID) {
		t.Error("a refused JMAP sign-in did not mark the account")
	}
	if got := syncFailureReason(err); got != syncFailAuth {
		t.Errorf("syncFailureReason = %q, want %q", got, syncFailAuth)
	}

	if err := credentials.Store(account.ID, credentials.Secret{Method: credentials.MethodPassword, Password: "pw"}); err != nil {
		t.Fatalf("store credentials: %v", err)
	}
	if _, err := a.jmapClient(a.ctx, &account); err != nil {
		t.Fatalf("jmapClient with the right password: %v", err)
	}
	if a.loginRejected(account.ID) {
		t.Error("a working JMAP sign-in left the account marked")
	}
}

// The password prompt checks what the user typed before storing it. A JMAP
// mailbox signs in over JMAP, so that is where the check has to go: its IMAP
// host may not answer at all, which would leave every password unverified.
func TestCheckAccountPasswordSignsInOverJMAP(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	var base string
	srv := newJMAPPlainServer(t, func() string { return base })
	base = "http://" + srv.addr()
	account := jmapRouteAccount(t, a, base, nil)

	got, err := a.CheckAccountPassword(account.ID, "wrong")
	if err != nil {
		t.Fatalf("check wrong password: %v", err)
	}
	if !got.Rejected || got.OK {
		t.Errorf("wrong password: got %+v, want rejected", got)
	}
	got, err = a.CheckAccountPassword(account.ID, "pw")
	if err != nil {
		t.Fatalf("check right password: %v", err)
	}
	if !got.OK {
		t.Errorf("right password: got %+v, want ok", got)
	}
}

// stubJMAPProbe answers the session lookup a mailbox restored from a backup
// does: available with the password "pw", refused with anything else.
func stubJMAPProbe(t *testing.T, sessionURL string) {
	t.Helper()
	orig := probeDomain
	t.Cleanup(func() { probeDomain = orig })
	probeDomain = func(_ context.Context, _ pjmap.ProbeOptions, _, _, secret string, _ bool, _ ...string) (pjmap.Probe, error) {
		if secret != "pw" {
			return pjmap.Probe{}, pjmap.AuthError(&gojmap.HTTPError{Status: http.StatusUnauthorized, StatusText: "401 Unauthorized"})
		}
		return pjmap.Probe{Available: true, SessionURL: sessionURL, MailAccountID: "A1"}, nil
	}
}

// A mailbox restored from a backup has no session yet and finds it by signing
// in. A refusal there is the one that matters most: the backup could not carry
// the password, so the prompt is the only way the mailbox ever syncs.
func TestJMAPClientNotesARefusedPasswordOnARestoredMailbox(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	stubJMAPProbe(t, "http://127.0.0.1:1/.well-known/jmap")
	account := jmapRouteAccount(t, a, "", func(acc *storage.Account) { acc.JMAPSessionURL, acc.JMAPMailAccountID = "", "" })
	if err := credentials.Store(account.ID, credentials.Secret{Method: credentials.MethodPassword, Password: "stale"}); err != nil {
		t.Fatalf("store credentials: %v", err)
	}

	if _, err := a.jmapClient(a.ctx, &account); err == nil {
		t.Fatal("jmapClient found a session with a refused password")
	}
	if !a.loginRejected(account.ID) {
		t.Error("a refused session lookup did not mark the account")
	}
}

// The prompt's check on a restored mailbox looks the session up with what was
// typed, and stores none of it: the password is not saved yet, and neither is
// a session found with it.
func TestCheckAccountPasswordOnARestoredJMAPMailbox(t *testing.T) {
	a := newAccountTestApp(t)
	a.proxyCfg = proxy.Config{Mode: proxy.ModeOff}
	stubJMAPProbe(t, "http://127.0.0.1:1/.well-known/jmap")
	account := jmapRouteAccount(t, a, "", func(acc *storage.Account) { acc.JMAPSessionURL, acc.JMAPMailAccountID = "", "" })

	got, err := a.CheckAccountPassword(account.ID, "wrong")
	if err != nil {
		t.Fatalf("check wrong password: %v", err)
	}
	if !got.Rejected || got.OK {
		t.Errorf("wrong password: got %+v, want rejected", got)
	}
	got, err = a.CheckAccountPassword(account.ID, "pw")
	if err != nil {
		t.Fatalf("check right password: %v", err)
	}
	if !got.OK {
		t.Errorf("right password: got %+v, want ok", got)
	}
	stored, err := a.store.GetAccount(a.ctx, account.ID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if stored.JMAPSessionURL != "" {
		t.Errorf("the check stored a session: %q", stored.JMAPSessionURL)
	}
	if a.loginRejected(account.ID) {
		t.Error("the check marked the account")
	}
}
