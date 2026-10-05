package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"
)

// TestMain swaps go-keyring for its in-memory mock before any test runs, so
// nothing in this package ever reads or writes the machine's real credential
// store.
func TestMain(m *testing.M) {
	keyring.MockInit()
	m.Run()
}

func TestChunkBytesSplitsAndReassembles(t *testing.T) {
	cases := []struct {
		name   string
		length int
		size   int
		want   int
	}{
		{"empty", 0, 10, 1},
		{"under the size", 4, 10, 1},
		{"exactly the size", 10, 10, 1},
		{"one byte over", 11, 10, 2},
		{"an exact multiple", 30, 10, 3},
		{"a ragged multiple", 31, 10, 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), c.length)
			chunks := chunkBytes(data, c.size)

			if len(chunks) != c.want {
				t.Errorf("chunkBytes produced %d chunks, want %d", len(chunks), c.want)
			}
			for i, chunk := range chunks {
				if len(chunk) > c.size {
					t.Errorf("chunk %d is %d bytes, over the %d-byte cap", i, len(chunk), c.size)
				}
			}
			if got := bytes.Join(chunks, nil); !bytes.Equal(got, data) {
				t.Errorf("rejoined %d bytes, want the original %d", len(got), len(data))
			}
		})
	}
}

// Every chunk has to fit Windows Credential Manager's per-entry cap, which is
// the only reason the splitting exists.
func TestChunkBytesHonoursTheEntryCap(t *testing.T) {
	data := bytes.Repeat([]byte("y"), maxEntrySize*3+17)
	for i, chunk := range chunkBytes(data, maxEntrySize) {
		if len(chunk) > maxEntrySize {
			t.Errorf("chunk %d is %d bytes, over the %d-byte entry cap", i, len(chunk), maxEntrySize)
		}
	}
}

func TestParseChunkMarker(t *testing.T) {
	cases := []struct {
		raw  string
		want chunkLayout
		ok   bool
	}{
		{chunkMarkerV1 + "3", chunkLayout{"", 3}, true},
		{chunkMarkerV2 + "a:2", chunkLayout{"a", 2}, true},
		{chunkMarkerV2 + "b:5", chunkLayout{"b", 5}, true},
		{chunkMarkerV2 + "c:2", chunkLayout{}, false},
		{chunkMarkerV2 + "a:x", chunkLayout{}, false},
		{chunkMarkerV1 + "notanumber", chunkLayout{}, false},
		{chunkMarkerV1, chunkLayout{}, false},
		{`{"method":"password"}`, chunkLayout{}, false},
		{"", chunkLayout{}, false},
	}
	for _, c := range cases {
		got, ok := parseChunkMarker(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("parseChunkMarker(%q) = (%+v, %t), want (%+v, %t)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// A chunk entry must never be named the same as the plain entry of another
// account, or one account's secret would overwrite part of another's.
func TestChunkKeyCannotCollideWithAnAccountKey(t *testing.T) {
	seen := map[string]string{}
	for _, id := range []int64{1, 2, 10, 11, 100} {
		plain := key(id)
		if other, ok := seen[plain]; ok {
			t.Errorf("key(%d) = %q, already used by %s", id, plain, other)
		}
		seen[plain] = "account " + strconv.FormatInt(id, 10)

		for _, slot := range []string{"", "a", "b"} {
			for i := range 3 {
				name := chunkLayout{slot: slot}.key(id, i)
				if other, ok := seen[name]; ok {
					t.Errorf("chunk key(%d, %q, %d) = %q, already used by %s", id, slot, i, name, other)
				}
				seen[name] = "chunk " + strconv.Itoa(i) + " (slot " + slot + ") of account " + strconv.FormatInt(id, 10)
			}
		}
	}
}

// An oauth secret with long tokens goes over the entry cap on its own, so the
// chunked path is the normal one for those accounts, not an edge case.
func TestStoreAndLoadRoundTripAChunkedSecret(t *testing.T) {
	const accountID = 7
	want := Secret{
		Method:       MethodOAuth,
		Provider:     "gmail",
		ClientID:     "client-id",
		RefreshToken: strings.Repeat("r", maxEntrySize*2+31),
		AccessToken:  strings.Repeat("a", maxEntrySize),
	}

	if err := Store(accountID, want); err != nil {
		t.Fatalf("store: %v", err)
	}
	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Error("the loaded secret does not match the stored one")
	}
}

// The bug this guards: a long secret is replaced by a short one, the short one
// is written to the main entry, and the chunk entries from the old secret are
// left behind. Loading then has to ignore them rather than appending stale
// bytes, and storing has to clean them up.
func TestStoreClearsChunksWhenASecretShrinks(t *testing.T) {
	const accountID = 8
	big := Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("r", maxEntrySize*2)}
	if err := Store(accountID, big); err != nil {
		t.Fatalf("store the long secret: %v", err)
	}

	small := Secret{Method: MethodPassword, Password: "short"}
	if err := Store(accountID, small); err != nil {
		t.Fatalf("store the short secret: %v", err)
	}

	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != small {
		t.Errorf("loaded %+v, want the short secret that replaced it", got)
	}
	for _, slot := range []string{"a", "b"} {
		if _, err := keyring.Get(service, chunkLayout{slot: slot}.key(accountID, 0)); err == nil {
			t.Errorf("a chunk entry from the replaced secret is still in slot %s", slot)
		}
	}
}

func TestLoadReportsNotFoundForAnUnknownAccount(t *testing.T) {
	if _, err := Load(404); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load of an account with no secret returned %v, want ErrNotFound", err)
	}
}

// Deleting an account must not fail because there was nothing to delete, or
// removing a half-set-up account would error every time.
func TestDeleteIsIdempotent(t *testing.T) {
	const accountID = 9
	if err := Store(accountID, Secret{Method: MethodPassword, Password: "p"}); err != nil {
		t.Fatalf("store: %v", err)
	}
	for range 2 {
		if err := Delete(accountID); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}
	if _, err := Load(accountID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load after delete returned %v, want ErrNotFound", err)
	}
}

// A mailbox's proxy password is its own entry: it must not overwrite the
// account's mail secret or the app-wide proxy password, and clearing it must
// leave both alone.
func TestAccountProxyPasswordIsSeparate(t *testing.T) {
	const accountID = 12
	if err := Store(accountID, Secret{Method: MethodPassword, Password: "mail"}); err != nil {
		t.Fatalf("store account secret: %v", err)
	}
	if err := StoreProxyPassword("global"); err != nil {
		t.Fatalf("store global proxy password: %v", err)
	}
	t.Cleanup(func() { _ = DeleteProxyPassword(); _ = Delete(accountID) })

	if err := StoreAccountProxyPassword(accountID, "routed"); err != nil {
		t.Fatalf("store account proxy password: %v", err)
	}
	if got, err := LoadAccountProxyPassword(accountID); err != nil || got != "routed" {
		t.Errorf("LoadAccountProxyPassword = %q, %v, want routed", got, err)
	}

	if err := StoreAccountProxyPassword(accountID, ""); err != nil {
		t.Fatalf("clear account proxy password: %v", err)
	}
	if got, err := LoadAccountProxyPassword(accountID); err != nil || got != "" {
		t.Errorf("after clearing, LoadAccountProxyPassword = %q, %v, want empty", got, err)
	}
	if s, err := Load(accountID); err != nil || s.Password != "mail" {
		t.Errorf("account secret = %+v, %v, want it untouched", s, err)
	}
	if got, err := LoadProxyPassword(); err != nil || got != "global" {
		t.Errorf("global proxy password = %q, %v, want it untouched", got, err)
	}
}

// A dev run and a nightly keep their own databases, so their account ids start
// at 1 just like the installed app's. Their secrets must not land on the
// installed app's entries, or testing a build overwrites the real password.
func TestUseServiceKeepsInstallsApart(t *testing.T) {
	const accountID = 1
	t.Cleanup(func() {
		for _, name := range []string{"Pelton", "Pelton-dev"} {
			UseService(name)
			_ = Delete(accountID)
		}
		UseService(defaultService)
	})

	UseService("Pelton")
	if err := Store(accountID, Secret{Method: MethodPassword, Password: "real"}); err != nil {
		t.Fatalf("store stable: %v", err)
	}
	UseService("Pelton-dev")
	if _, err := Load(accountID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dev Load of the stable account returned %v, want ErrNotFound", err)
	}
	if err := Store(accountID, Secret{Method: MethodPassword, Password: "alice-e2e"}); err != nil {
		t.Fatalf("store dev: %v", err)
	}

	UseService("Pelton")
	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load stable: %v", err)
	}
	if got.Password != "real" {
		t.Errorf("stable password = %q after a dev store, want %q", got.Password, "real")
	}
}

// The bug: chunks were rewritten in place before the main entry moved, so a
// failed second write left new chunk 0 next to old chunk 1 and the secret
// unreadable.
func TestStoreFailureKeepsThePreviousChunkedSecret(t *testing.T) {
	const accountID = 9
	old := Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("o", maxEntrySize*2)}
	if err := Store(accountID, old); err != nil {
		t.Fatalf("store the first secret: %v", err)
	}

	writes := 0
	setEntry = func(svc, user, pass string) error {
		writes++
		if writes == 2 {
			return errors.New("keychain locked")
		}
		return keyring.Set(svc, user, pass)
	}
	t.Cleanup(func() { setEntry = keyring.Set })

	if err := Store(accountID, Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("n", maxEntrySize*2)}); err == nil {
		t.Fatal("Store reported success though a chunk write failed")
	}
	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load after the failed store: %v", err)
	}
	if got != old {
		t.Error("the previous secret did not survive a failed store")
	}
}

// Secrets written before the slot layout use the v1 marker and numbered keys;
// they have to keep loading.
func TestLoadReadsAVersionOneChunkedSecret(t *testing.T) {
	const accountID = 10
	want := Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("v", maxEntrySize+50)}
	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	parts := chunkBytes(encoded, maxEntrySize)
	for i, p := range parts {
		if err := keyring.Set(service, key(accountID)+"."+strconv.Itoa(i), string(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := keyring.Set(service, key(accountID), chunkMarkerV1+strconv.Itoa(len(parts))); err != nil {
		t.Fatal(err)
	}

	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Error("a v1 chunked secret did not load")
	}
}

// The bug: a keyring read that failed for any reason looked like "no split
// secret", so Store wrote into slot a, which may be the live one, and a later
// failed write cleaned those chunks up and destroyed the stored secret. A read
// error other than not-found has to stop the store before anything is written.
func TestStoreAbortsWhenTheCurrentSecretCannotBeRead(t *testing.T) {
	const accountID = 11
	old := Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("o", maxEntrySize*2)}
	if err := Store(accountID, old); err != nil {
		t.Fatalf("store the first secret: %v", err)
	}
	t.Cleanup(func() { _ = Delete(accountID) })

	getEntry = func(string, string) (string, error) { return "", errors.New("keychain busy") }
	writes := 0
	setEntry = func(svc, user, pass string) error {
		writes++
		if writes == 2 {
			return errors.New("keychain locked")
		}
		return keyring.Set(svc, user, pass)
	}
	t.Cleanup(func() { getEntry, setEntry = keyring.Get, keyring.Set })

	if err := Store(accountID, Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("n", maxEntrySize*2)}); err == nil {
		t.Fatal("Store reported success though the current secret could not be read")
	}
	if writes != 0 {
		t.Errorf("Store wrote %d entries after a failed read, want none", writes)
	}

	getEntry = keyring.Get
	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load after the read recovered: %v", err)
	}
	if got != old {
		t.Error("the previous secret did not survive a store that could not read it")
	}
}

// Delete still has to remove the main entry when it cannot read it, but it must
// not guess at chunk names it could not look up.
func TestDeleteRemovesTheMainEntryWhenItCannotBeRead(t *testing.T) {
	const accountID = 13
	if err := Store(accountID, Secret{Method: MethodPassword, Password: "p"}); err != nil {
		t.Fatalf("store: %v", err)
	}
	getEntry = func(string, string) (string, error) { return "", errors.New("keychain busy") }
	t.Cleanup(func() { getEntry = keyring.Get })

	if err := Delete(accountID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	getEntry = keyring.Get
	if _, err := Load(accountID); !errors.Is(err, ErrNotFound) {
		t.Errorf("Load after delete returned %v, want ErrNotFound", err)
	}
}

// Storing over a v1 chunked secret moves it to the slot layout: the new secret
// loads and the numbered v1 chunk entries are removed.
func TestStoreMigratesAVersionOneChunkedSecret(t *testing.T) {
	const accountID = 14
	encoded, err := json.Marshal(Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("v", maxEntrySize+50)})
	if err != nil {
		t.Fatal(err)
	}
	parts := chunkBytes(encoded, maxEntrySize)
	for i, p := range parts {
		if err := keyring.Set(service, key(accountID)+"."+strconv.Itoa(i), string(p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := keyring.Set(service, key(accountID), chunkMarkerV1+strconv.Itoa(len(parts))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = Delete(accountID) })

	want := Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("w", maxEntrySize*2)}
	if err := Store(accountID, want); err != nil {
		t.Fatalf("store over the v1 secret: %v", err)
	}
	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != want {
		t.Error("the secret stored over a v1 one did not load")
	}
	for i := range parts {
		if _, err := keyring.Get(service, key(accountID)+"."+strconv.Itoa(i)); !errors.Is(err, keyring.ErrNotFound) {
			t.Errorf("v1 chunk %d is still there (err %v)", i, err)
		}
	}
}

// Store reads the current slot and then writes the other one, so two stores at
// once picked the same slot and wrote their chunks into each other. Store A is
// held on its first chunk write until store B makes a write of its own; with
// the lock B never gets that far, the wait times out, and A completes first.
func TestConcurrentStoresDoNotInterleaveChunks(t *testing.T) {
	const accountID = 12
	t.Cleanup(func() { _ = Delete(accountID) })
	secretA := Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("Q", maxEntrySize*3)}
	secretB := Secret{Method: MethodOAuth, RefreshToken: strings.Repeat("W", maxEntrySize*3)}

	var (
		mu         sync.Mutex
		order      []byte
		held       = make(chan struct{})
		bWrote     = make(chan struct{})
		bWroteOnce sync.Once
	)
	setEntry = func(svc, user, pass string) error {
		var tag byte
		switch {
		case strings.Contains(pass, "QQQ"):
			tag = 'A'
		case strings.Contains(pass, "WWW"):
			tag = 'B'
		}
		mu.Lock()
		first := tag == 'A' && len(order) == 0
		order = append(order, tag)
		mu.Unlock()
		if tag == 'B' {
			bWroteOnce.Do(func() { close(bWrote) })
		}
		if first {
			close(held)
			select {
			case <-bWrote:
			case <-time.After(300 * time.Millisecond):
			}
		}
		return keyring.Set(svc, user, pass)
	}
	t.Cleanup(func() { setEntry = keyring.Set })

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := Store(accountID, secretA); err != nil {
			t.Errorf("store A: %v", err)
		}
	}()
	<-held
	go func() {
		defer wg.Done()
		if err := Store(accountID, secretB); err != nil {
			t.Errorf("store B: %v", err)
		}
	}()
	wg.Wait()

	mu.Lock()
	chunks := strings.ReplaceAll(string(order), "\x00", "")
	mu.Unlock()
	switches := 0
	for i := 1; i < len(chunks); i++ {
		if chunks[i] != chunks[i-1] {
			switches++
		}
	}
	if switches != 1 {
		t.Errorf("chunk writes ran as %q, want one store's chunks before the other's", chunks)
	}
	got, err := Load(accountID)
	if err != nil {
		t.Fatalf("load after both stores: %v", err)
	}
	if got != secretA && got != secretB {
		t.Error("the loaded secret is neither of the two stored ones")
	}
}
