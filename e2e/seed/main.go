// Seed fills alice and bob inboxes over plain HTTP JMAP.
// Each inbox receives messages from the other account. The corpus is not committed.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Janso123/go-jmap"
	"github.com/Janso123/go-jmap/core"
	"github.com/Janso123/go-jmap/mail"
	"github.com/Janso123/go-jmap/mail/email"
	"github.com/Janso123/go-jmap/mail/mailbox"

	_ "github.com/Janso123/go-jmap/core"
)

const (
	// Discover and the JMAP client require the session document to share the
	// https://127.0.0.1 origin. Stalwart advertises that origin when no
	// STALWART_PUBLIC_URL is set.
	sessionURL = "https://127.0.0.1/.well-known/jmap"
	inlineMax  = 32 * 1024
	fiveMB     = 5 * 1024 * 1024
)

// perInbox is 6,400 for the Playwright suite. E2E_SEED_PER_INBOX lowers it
// for manual.sh, where a smaller inbox is quicker to seed.
var perInbox = seedCount()

var (
	start = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	end   = time.Date(2026, 9, 24, 23, 59, 59, 0, time.UTC)
)

type account struct {
	name   string
	email  string
	secret string
}

type manifest struct {
	AliceCount         int    `json:"aliceCount"`
	BobCount           int    `json:"bobCount"`
	AliceShortSubject  string `json:"aliceShortSubject"`
	AliceLargeSubject  string `json:"aliceLargeSubject"`
	AliceSearchSubject string `json:"aliceSearchSubject"`
	ShortBody          string `json:"shortBody"`
	LargePrefix        string `json:"largePrefix"`
}

func main() {
	ctx := context.Background()
	alice := account{name: "alice", email: "alice@example.org", secret: "alice-e2e"}
	bob := account{name: "bob", email: "bob@example.org", secret: "bob-e2e"}

	aliceC := login(ctx, alice)
	bobC := login(ctx, bob)
	aliceID := primary(aliceC)
	bobID := primary(bobC)
	aliceInbox := inboxID(ctx, aliceC, aliceID)
	bobInbox := inboxID(ctx, bobC, bobID)

	rng := rand.New(rand.NewSource(1))
	man := manifest{
		AliceCount:  perInbox,
		BobCount:    perInbox,
		ShortBody:   "x",
		LargePrefix: "LARGE-BODY-MARKER",
	}

	fmt.Println("seeding alice inbox from bob")
	man.AliceShortSubject, man.AliceLargeSubject, man.AliceSearchSubject = seedInbox(ctx, aliceC, aliceID, alice.email, bob.email, aliceInbox, rng)
	fmt.Println("seeding bob inbox from alice")
	seedInbox(ctx, bobC, bobID, bob.email, alice.email, bobInbox, rng)

	out := filepath.Join(moduleE2E(), "manifest.json")
	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		fatal(err)
	}
	fmt.Println("wrote", out)
}

func login(ctx context.Context, a account) *jmap.Client {
	c := jmap.NewClient(sessionURL, jmap.WithBasic(a.email, a.secret), jmap.WithTimeout(2*time.Minute))
	if err := c.Authenticate(ctx); err != nil {
		fatal(fmt.Errorf("%s session: %w", a.name, err))
	}
	return c
}

func primary(c *jmap.Client) jmap.ID {
	id, err := c.PrimaryAccount(mail.URI)
	if err != nil {
		fatal(err)
	}
	return id
}

func inboxID(ctx context.Context, c *jmap.Client, accountID jmap.ID) jmap.ID {
	resp, err := jmap.Call[*mailbox.GetResponse](ctx, c, &mailbox.Get{Account: accountID})
	if err != nil {
		fatal(err)
	}
	for _, mb := range resp.List {
		role, ok := mb.Role.Value()
		if ok && role == mailbox.RoleInbox {
			return mb.ID
		}
	}
	fatal(fmt.Errorf("inbox missing"))
	return ""
}

// seedInbox creates perInbox messages in the recipient inbox, sent as fromEmail.
// The client must be the recipient: Email/set writes into that account.
func seedInbox(ctx context.Context, owner *jmap.Client, ownerID jmap.ID, ownerEmail, fromEmail string, box jmap.ID, rng *rand.Rand) (shortSub, largeSub, searchSub string) {
	batchMax := max(min(maxInSet(owner), 24), 1)

	var pending []*email.Email
	var pendingSizes []int
	flush := func() {
		if len(pending) == 0 {
			return
		}
		createBatch(ctx, owner, ownerID, pending)
		pending = pending[:0]
		pendingSizes = pendingSizes[:0]
	}

	for i := range perInbox {
		size := bodySize(i)
		subject := randomSubject(rng)
		switch i {
		case 0:
			shortSub = subject
		case 1:
			largeSub = subject
		case 100:
			searchSub = subject
		}
		when := randomWhen(rng)
		msg := &email.Email{
			MailboxIDs: map[jmap.ID]bool{box: true},
			From:       jmap.Some([]*mail.Address{{Email: fromEmail}}),
			To:         jmap.Some([]*mail.Address{{Email: ownerEmail}}),
			Subject:    jmap.Some(subject),
			ReceivedAt: jmap.UTCDatePtr(when),
			SentAt:     jmap.Some(jmap.Date(when)),
		}
		body := bodyText(i, size)
		if size <= inlineMax {
			msg.BodyValues = map[string]*email.BodyValue{"1": {Value: body}}
			msg.TextBody = []*email.BodyPart{{
				PartID: jmap.Some("1"),
				Type:   "text/plain",
			}}
			pending = append(pending, msg)
			pendingSizes = append(pendingSizes, size)
			if len(pending) >= batchMax || requestBytes(pendingSizes) > 4*1024*1024 {
				flush()
			}
		} else {
			flush()
			blobID := upload(ctx, owner, ownerID, body)
			msg.TextBody = []*email.BodyPart{{
				BlobID: jmap.Some(blobID),
				Type:   "text/plain",
			}}
			createBatch(ctx, owner, ownerID, []*email.Email{msg})
		}
		if (i+1)%200 == 0 {
			fmt.Printf("  %d/%d\n", i+1, perInbox)
		}
	}
	flush()
	return shortSub, largeSub, searchSub
}

func createBatch(ctx context.Context, c *jmap.Client, accountID jmap.ID, msgs []*email.Email) {
	create := make(map[jmap.ID]*email.Email, len(msgs))
	for i, m := range msgs {
		create[jmap.ID(fmt.Sprintf("m%d", i))] = m
	}
	resp, err := jmap.Call[*email.SetResponse](ctx, c, &email.Set{
		Account: accountID,
		Create:  jmap.Some(create),
	})
	if err != nil {
		fatal(err)
	}
	if nc, ok := resp.NotCreated.Value(); ok && len(nc) > 0 {
		for id, e := range nc {
			fatal(fmt.Errorf("notCreated %s: %+v", id, e))
		}
	}
	if len(resp.Created) != len(msgs) {
		fatal(fmt.Errorf("created %d of %d", len(resp.Created), len(msgs)))
	}
}

func upload(ctx context.Context, c *jmap.Client, accountID jmap.ID, body string) jmap.ID {
	up, err := c.Upload(ctx, accountID, bytes.NewReader([]byte(body)), "text/plain")
	if err != nil {
		fatal(err)
	}
	if up == nil || up.ID == "" {
		fatal(fmt.Errorf("empty blob upload"))
	}
	return up.ID
}

func bodySize(i int) int {
	rng := rand.New(rand.NewSource(int64(i) + 17))
	switch {
	case i == 0:
		return 1
	case i == 1:
		return fiveMB
	case i < 42:
		return logUniform(rng, 1<<20, 4<<20)
	case i%25 == 0:
		return logUniform(rng, 100*1024, 800*1024)
	default:
		return logUniform(rng, 1024, 40*1024)
	}
}

func bodyText(i, size int) string {
	if size == 1 {
		return "x"
	}
	prefix := ""
	if i == 1 {
		prefix = "LARGE-BODY-MARKER\n"
	}
	if len(prefix) > size {
		return prefix[:size]
	}
	unit := []byte("abcdefghijklmnopqrstuvwxyz\n")
	buf := make([]byte, size)
	copy(buf, prefix)
	for n := len(prefix); n < size; {
		c := copy(buf[n:], unit)
		n += c
	}
	return string(buf)
}

func logUniform(rng *rand.Rand, min, max int) int {
	ln := math.Log(float64(min)) + rng.Float64()*(math.Log(float64(max))-math.Log(float64(min)))
	n := int(math.Exp(ln))
	if n < min {
		n = min
	}
	if n > max {
		n = max
	}
	return n
}

func randomSubject(rng *rand.Rand) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 "
	n := 3 + rng.Intn(33)
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

func randomWhen(rng *rand.Rand) time.Time {
	span := end.Sub(start)
	return start.Add(time.Duration(rng.Int63n(int64(span))))
}

func requestBytes(sizes []int) int {
	n := 0
	for _, s := range sizes {
		n += s
	}
	return n
}

func maxInSet(c *jmap.Client) int {
	cap, ok := c.Session.Capabilities[jmap.CoreURI].(*core.Core)
	if !ok || cap.MaxObjectsInSet == 0 {
		return 16
	}
	return int(cap.MaxObjectsInSet)
}

func moduleE2E() string {
	wd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	if filepath.Base(wd) == "seed" {
		return filepath.Dir(wd)
	}
	if filepath.Base(wd) == "e2e" {
		return wd
	}
	return filepath.Join(wd, "e2e")
}

func seedCount() int {
	raw := os.Getenv("E2E_SEED_PER_INBOX")
	if raw == "" {
		return 6400
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		fatal(fmt.Errorf("E2E_SEED_PER_INBOX must be a non-negative number, got %q", raw))
	}
	return n
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
