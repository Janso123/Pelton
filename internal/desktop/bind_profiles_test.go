package desktop

import (
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pimap "github.com/peltonapp/Pelton/internal/imap"
	"github.com/peltonapp/Pelton/internal/logging"
	"github.com/peltonapp/Pelton/internal/proxy"
	"github.com/peltonapp/Pelton/internal/storage"
)

func newProfileApp(t *testing.T) *App {
	t.Helper()
	ctx, stopBackground := testContext(t)
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	t.Cleanup(stopBackground)
	if err := store.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// switching re-applies the log settings of the profile it lands in, so the
	// app under test needs the real writer rather than a bare logger.
	w := logging.NewWriter()
	return &App{ctx: ctx, store: store, log: w.Logger(), logWriter: w}
}

// A fresh install is one profile holding what it always had.
func TestListProfilesStartsWithMain(t *testing.T) {
	a := newProfileApp(t)

	profiles, err := a.ListProfiles()
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("got %d profiles, want 1", len(profiles))
	}
	if !profiles[0].Main || !profiles[0].Active {
		t.Errorf("the only profile is %+v, want main and active", profiles[0])
	}
}

func TestCreateProfileNeedsAName(t *testing.T) {
	a := newProfileApp(t)

	if _, err := a.CreateProfile(ProfileRequest{Name: "   "}); !errors.Is(err, errProfileNameRequired) {
		t.Errorf("CreateProfile(blank) = %v, want errProfileNameRequired", err)
	}
}

// Copy and share look the same on the day you make them and differ from then
// on: a copy stops tracking, a share does not.
func TestCreateProfileCopiesAndShares(t *testing.T) {
	a := newProfileApp(t)
	if err := a.store.Set(a.ctx, "theme", "dark"); err != nil {
		t.Fatalf("set: %v", err)
	}

	copied, err := a.CreateProfile(ProfileRequest{
		Name:            "Work",
		StartSettings:   startCopy,
		StartSignatures: startFresh,
		StartViews:      startFresh,
	})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if copied.ShareSettings {
		t.Error("a copied area is marked as shared")
	}

	shared, err := a.CreateProfile(ProfileRequest{Name: "Family", StartSettings: startShare})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if !shared.ShareSettings {
		t.Error("a shared area is not marked as shared")
	}

	// the copy has main's value of the day, without the link.
	if err := a.SwitchProfile(copied.ID); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if got, err := a.store.Get(a.ctx, "theme"); err != nil || got != "dark" {
		t.Errorf("copied theme = %q (%v), want dark", got, err)
	}
}

func TestSwitchProfileChangesWhatIsVisible(t *testing.T) {
	a := newProfileApp(t)

	mainAccount := storage.Account{Email: "me@example.com"}
	if _, err := a.store.CreateAccount(a.ctx, &mainAccount); err != nil {
		t.Fatalf("create account: %v", err)
	}

	work, err := a.CreateProfile(ProfileRequest{Name: "Work", StartSettings: startFresh})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("SwitchProfile: %v", err)
	}

	active, err := a.ActiveProfile()
	if err != nil {
		t.Fatalf("ActiveProfile: %v", err)
	}
	if active.ID != work.ID {
		t.Errorf("active = %d, want work %d", active.ID, work.ID)
	}
	accounts, err := a.store.ListAccounts(a.ctx)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("work shows %d of main's accounts", len(accounts))
	}
}

// Switching has to stop the profile's background work, or the app keeps
// watching mailboxes the new profile cannot see.
func TestSwitchProfileEndsTheSession(t *testing.T) {
	a := newProfileApp(t)
	before := a.sessionCtx()

	work, err := a.CreateProfile(ProfileRequest{Name: "Work"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("SwitchProfile: %v", err)
	}

	if before.Err() == nil {
		t.Error("the previous profile's session is still running")
	}
	if a.sessionCtx().Err() != nil {
		t.Error("the new profile has no live session")
	}
}

func TestDeleteMainProfileIsRefused(t *testing.T) {
	a := newProfileApp(t)
	main, err := a.store.MainProfile(a.ctx)
	if err != nil {
		t.Fatalf("main profile: %v", err)
	}

	if err := a.DeleteProfile(main.ID); !errors.Is(err, errMainProfileDelete) {
		t.Errorf("DeleteProfile(main) = %v, want errMainProfileDelete", err)
	}
}

// Deleting the profile you are in has to leave the app somewhere.
func TestDeleteActiveProfileFallsBackToMain(t *testing.T) {
	a := newProfileApp(t)
	main, err := a.store.MainProfile(a.ctx)
	if err != nil {
		t.Fatalf("main profile: %v", err)
	}
	work, err := a.CreateProfile(ProfileRequest{Name: "Work"})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("SwitchProfile: %v", err)
	}

	if err := a.DeleteProfile(work.ID); err != nil {
		t.Fatalf("DeleteProfile: %v", err)
	}

	active, err := a.ActiveProfile()
	if err != nil {
		t.Fatalf("ActiveProfile: %v", err)
	}
	if active.ID != main.ID {
		t.Errorf("active = %d after deleting the profile we were in, want main %d", active.ID, main.ID)
	}
}

func TestUpdateProfileRenamesMainWithoutSharing(t *testing.T) {
	a := newProfileApp(t)
	main, err := a.store.MainProfile(a.ctx)
	if err != nil {
		t.Fatalf("main profile: %v", err)
	}

	got, err := a.UpdateProfile(ProfileRequest{
		ID:   main.ID,
		Name: "Personal",
		// main shares from nobody, so this must be ignored rather than making
		// it point at itself.
		StartSettings: startShare,
	})
	if err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if got.Name != "Personal" {
		t.Errorf("name = %q, want Personal", got.Name)
	}
	if got.ShareSettings {
		t.Error("main was marked as sharing with itself")
	}
}

func TestTrimIconKeepsAGlyphOrTwo(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"💼", "💼"},
		{" 🏠 ", "🏠"},
		{"", ""},
		{"abcdefghijkl", "abcdefgh"},
	} {
		if got := trimIcon(tt.in); got != tt.want {
			t.Errorf("trimIcon(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// The proxy and the MCP server are read once and cached, so a switch has to
// reload them: a stale proxy sends the new profile's mail around its own proxy
// settings, and a stale MCP server keeps the old profile's token and
// permissions reachable.
func TestSwitchProfileAppliesProxyAndMCPOfTheNewProfile(t *testing.T) {
	a := newProfileApp(t)
	t.Cleanup(a.stopMCP)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	work, err := a.CreateProfile(ProfileRequest{Name: "Work", StartSettings: startFresh})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	main, err := a.store.MainProfile(a.ctx)
	if err != nil {
		t.Fatalf("MainProfile: %v", err)
	}

	// work has a manual proxy; main has none and runs the MCP server.
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("switch to work: %v", err)
	}
	if err := a.store.Set(a.ctx, storage.SettingProxy, `{"mode":"manual","scheme":"socks5","host":"127.0.0.1","port":1080}`); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := a.SwitchProfile(main.ID); err != nil {
		t.Fatalf("switch to main: %v", err)
	}
	if got := a.currentProxy().Mode; got == proxy.ModeManual {
		t.Fatalf("main proxy mode = %q, want work's proxy gone", got)
	}
	for k, v := range map[string]string{settingMCPEnabled: "true", settingMCPPort: strconv.Itoa(port), settingMCPToken: "main-token"} {
		if err := a.store.Set(a.ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	if err := a.applyMCPState(); err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	if a.mcp == nil {
		t.Fatal("mcp not running in main")
	}

	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("switch to work: %v", err)
	}
	if got := a.currentProxy().Mode; got != proxy.ModeManual {
		t.Errorf("work proxy mode = %q, want %q", got, proxy.ModeManual)
	}
	a.mcpMu.Lock()
	running := a.mcp != nil
	a.mcpMu.Unlock()
	if running {
		t.Error("main's MCP server still runs after switching to a profile with it off")
	}
}

// Two profiles sharing settings ask for the same MCP server, and restarting it
// on every switch only dropped the agents connected to it.
func TestSwitchProfileKeepsAnUnchangedMCPServer(t *testing.T) {
	a := newProfileApp(t)
	t.Cleanup(a.stopMCP)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	for k, v := range map[string]string{settingMCPEnabled: "true", settingMCPPort: strconv.Itoa(port), settingMCPToken: "shared-token"} {
		if err := a.store.Set(a.ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	if err := a.applyMCPState(); err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	a.mcpMu.Lock()
	before := a.mcp
	a.mcpMu.Unlock()
	if before == nil {
		t.Fatal("mcp not running")
	}

	work, err := a.CreateProfile(ProfileRequest{Name: "Work", StartSettings: startShare})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("switch to work: %v", err)
	}

	a.mcpMu.Lock()
	after := a.mcp
	a.mcpMu.Unlock()
	if after != before || !after.Running() {
		t.Fatalf("mcp server = %p (running %v), want the same running server %p", after, after != nil && after.Running(), before)
	}
}

// Turning settings sharing off for the profile you are in changes which
// settings the backend reads, so the cached proxy and the MCP server have to
// follow; a non-active profile's edit changes neither.
func TestUpdateProfileReappliesSettingsOnlyForTheActiveProfile(t *testing.T) {
	a := newProfileApp(t)
	t.Cleanup(a.stopMCP)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	work, err := a.CreateProfile(ProfileRequest{Name: "Work", StartSettings: startFresh})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	main, err := a.store.MainProfile(a.ctx)
	if err != nil {
		t.Fatalf("MainProfile: %v", err)
	}
	// work's own settings: a manual proxy and no MCP.
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("switch to work: %v", err)
	}
	if err := a.store.Set(a.ctx, storage.SettingProxy, `{"mode":"manual","scheme":"socks5","host":"127.0.0.1","port":1080}`); err != nil {
		t.Fatalf("set proxy: %v", err)
	}
	if err := a.SwitchProfile(main.ID); err != nil {
		t.Fatalf("switch to main: %v", err)
	}
	for k, v := range map[string]string{settingMCPEnabled: "true", settingMCPPort: strconv.Itoa(port), settingMCPToken: "main-token"} {
		if err := a.store.Set(a.ctx, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	if err := a.applyMCPState(); err != nil {
		t.Fatalf("start mcp: %v", err)
	}
	mcpRunning := func() bool {
		a.mcpMu.Lock()
		defer a.mcpMu.Unlock()
		return a.mcp != nil
	}

	// editing work while in main touches neither.
	if _, err := a.UpdateProfile(ProfileRequest{ID: work.ID, Name: "Work", StartSettings: startShare}); err != nil {
		t.Fatalf("share while in main: %v", err)
	}
	if _, err := a.UpdateProfile(ProfileRequest{ID: work.ID, Name: "Work"}); err != nil {
		t.Fatalf("unshare while in main: %v", err)
	}
	if got := a.currentProxy().Mode; got == proxy.ModeManual {
		t.Errorf("proxy mode = %q after editing another profile, want main's", got)
	}
	if !mcpRunning() {
		t.Error("mcp stopped after editing another profile")
	}

	// in work, sharing with main, then off again.
	if _, err := a.UpdateProfile(ProfileRequest{ID: work.ID, Name: "Work", StartSettings: startShare}); err != nil {
		t.Fatalf("share: %v", err)
	}
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("switch to work: %v", err)
	}
	if got := a.currentProxy().Mode; got == proxy.ModeManual {
		t.Fatalf("shared proxy mode = %q, want main's", got)
	}
	if !mcpRunning() {
		t.Fatal("shared mcp not running in work")
	}
	if _, err := a.UpdateProfile(ProfileRequest{ID: work.ID, Name: "Work"}); err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if got := a.currentProxy().Mode; got != proxy.ModeManual {
		t.Errorf("proxy mode = %q, want %q after unsharing", got, proxy.ModeManual)
	}
	if mcpRunning() {
		t.Error("mcp still running after unsharing into a profile with it off")
	}
}

// Accounts added to or removed from the profile you are in change what its
// workers should be syncing, so they start and stop at once; editing another
// profile leaves the running workers alone.
func TestUpdateProfileStartsAndStopsWorkersForTheActiveProfile(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	a.logWriter = logging.NewWriter()
	a.newIMAPClient = func(pimap.Config) (mailClient, error) { return &fakeIMAP{}, nil }

	idA, _ := seedSwitchAccount(t, a, "a@example.test", "imap")
	idB, _ := seedSwitchAccount(t, a, "b@example.test", "imap")
	idC, _ := seedSwitchAccount(t, a, "c@example.test", "imap")
	work, err := a.CreateProfile(ProfileRequest{Name: "Work", AccountIDs: []int64{idA, idB}})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	other, err := a.CreateProfile(ProfileRequest{Name: "Other", AccountIDs: []int64{idA}})
	if err != nil {
		t.Fatalf("CreateProfile other: %v", err)
	}
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("SwitchProfile: %v", err)
	}
	// the switch starts the profile's workers in the background.
	deadline := time.Now().Add(5 * time.Second)
	for !(a.hasAccountWorker(idA) && a.hasAccountWorker(idB)) {
		if time.Now().After(deadline) {
			t.Fatal("the switch did not start the profile's workers")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// another profile's edit touches no worker.
	if _, err := a.UpdateProfile(ProfileRequest{ID: other.ID, Name: "Other", AccountIDs: []int64{idC}}); err != nil {
		t.Fatalf("UpdateProfile other: %v", err)
	}
	if !a.hasAccountWorker(idA) || !a.hasAccountWorker(idB) || a.hasAccountWorker(idC) {
		t.Fatalf("workers after editing another profile: a=%v b=%v c=%v, want true true false",
			a.hasAccountWorker(idA), a.hasAccountWorker(idB), a.hasAccountWorker(idC))
	}

	if _, err := a.UpdateProfile(ProfileRequest{ID: work.ID, Name: "Work", AccountIDs: []int64{idB, idC}}); err != nil {
		t.Fatalf("UpdateProfile work: %v", err)
	}
	if a.hasAccountWorker(idA) {
		t.Error("worker for the removed account is still registered")
	}
	if !a.hasAccountWorker(idB) {
		t.Error("worker for the kept account was stopped")
	}
	if !a.hasAccountWorker(idC) {
		t.Error("no worker for the added account")
	}
}

// An id repeated in the request is one account: the worker started for it is
// not stopped and started again by its second mention.
func TestUpdateProfileStartsOneWorkerForDuplicateAccountIDs(t *testing.T) {
	a := newJMAPSwitchTestApp(t)
	a.logWriter = logging.NewWriter()
	a.newIMAPClient = func(pimap.Config) (mailClient, error) { return &fakeIMAP{}, nil }
	logs := &lockedBuffer{}
	a.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	idA, _ := seedSwitchAccount(t, a, "a@example.test", "imap")
	idB, _ := seedSwitchAccount(t, a, "b@example.test", "imap")
	work, err := a.CreateProfile(ProfileRequest{Name: "Work", AccountIDs: []int64{idA}})
	if err != nil {
		t.Fatalf("CreateProfile: %v", err)
	}
	if err := a.SwitchProfile(work.ID); err != nil {
		t.Fatalf("SwitchProfile: %v", err)
	}

	if _, err := a.UpdateProfile(ProfileRequest{ID: work.ID, Name: "Work", AccountIDs: []int64{idA, idB, idB}}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	if !a.hasAccountWorker(idB) {
		t.Fatal("no worker for the added account")
	}
	// stopping a registered worker is the only thing that logs this, so a
	// second launch for the same account shows up here.
	got := logs.String()
	if strings.Contains(got, `"account worker stopped" account=`+strconv.FormatInt(idB, 10)+" ") {
		t.Errorf("a worker was restarted for a repeated id:\n%s", got)
	}
}
