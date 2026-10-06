package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	gojmap "github.com/Janso123/go-jmap"
)

func TestSubmitNoSentMailbox(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionDocLimits(256, 128))
			return
		}
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	requests.Store(0)
	err := Submit(context.Background(), c, "A1", "", []byte("raw"), "user@example.com", "user@example.com", []string{"a@b.c"})
	if err == nil || err.Error() != "account has no Sent mailbox" {
		t.Fatalf("err: got %v", err)
	}
	if !errors.Is(err, ErrNoSentMailbox) {
		t.Fatalf("want ErrNoSentMailbox, got %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("requests: got %d want 0", requests.Load())
	}
}

func TestSubmitIdentityMatch(t *testing.T) {
	t.Run("no match", func(t *testing.T) {
		var uploads atomic.Int32
		srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
			if calls[0].Name != "Identity/get" {
				t.Fatalf("unexpected %s", calls[0].Name)
			}
			writeMethod(w, "s1", calls[0].ID, "Identity/get", map[string]any{
				"accountId": "A1", "state": "i1",
				"list": []map[string]any{
					{"id": "I1", "email": "other@example.com"},
				},
				"notFound": []string{},
			})
		})
		defer srv.Close()

		c := newTestClient(t, srv)
		err := Submit(context.Background(), c, "A1", "MbSent", []byte("raw"), "user@example.com", "user@example.com", []string{"a@b.c"})
		if !errors.Is(err, ErrNoMatchingIdentity) {
			t.Fatalf("want ErrNoMatchingIdentity, got %v", err)
		}
		if uploads.Load() != 0 {
			t.Fatalf("uploads: got %d want 0", uploads.Load())
		}
	})

	t.Run("ambiguous case-insensitive", func(t *testing.T) {
		var uploads atomic.Int32
		srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
			writeMethod(w, "s1", calls[0].ID, "Identity/get", map[string]any{
				"accountId": "A1", "state": "i1",
				"list": []map[string]any{
					{"id": "I1", "email": "User@Example.com"},
					{"id": "I2", "email": "user@example.com"},
				},
				"notFound": []string{},
			})
		})
		defer srv.Close()

		c := newTestClient(t, srv)
		err := Submit(context.Background(), c, "A1", "MbSent", []byte("raw"), "user@example.com", "user@example.com", []string{"a@b.c"})
		if !errors.Is(err, ErrAmbiguousIdentity) {
			t.Fatalf("want ErrAmbiguousIdentity, got %v", err)
		}
		if uploads.Load() != 0 {
			t.Fatalf("uploads: got %d want 0", uploads.Load())
		}
	})
}

func TestSubmitHappyPath(t *testing.T) {
	var uploads atomic.Int32
	var apiPosts atomic.Int32
	var sawImport, sawSubmission bool
	var emailID, identityID string

	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		apiPosts.Add(1)
		switch {
		case len(calls) == 1 && calls[0].Name == "Identity/get":
			writeMethod(w, "s1", calls[0].ID, "Identity/get", map[string]any{
				"accountId": "A1", "state": "i1",
				"list": []map[string]any{
					{"id": "ID9", "email": "User@Example.com"},
					{"id": "IDx", "email": "other@example.com"},
				},
				"notFound": []string{},
			})
		case len(calls) == 1 && calls[0].Name == "Email/query":
			writeQuery(w, "s1", calls[0].ID, "Email/query", []string{})
		case len(calls) == 2:
			for _, call := range calls {
				switch call.Name {
				case "Email/import":
					sawImport = true
					emails, _ := call.Args["emails"].(map[string]any)
					imp, _ := emails["email"].(map[string]any)
					if imp == nil {
						t.Fatalf("missing emails.email: %#v", call.Args)
					}
				case "EmailSubmission/set":
					sawSubmission = true
					update, _ := call.Args["onSuccessUpdateEmail"].(map[string]any)
					patch, _ := update["#sub"].(map[string]any)
					if len(update) != 1 || len(patch) != 1 || patch["keywords/$seen"] != true {
						t.Fatalf("onSuccessUpdateEmail: got %#v want {#sub: {keywords/$seen: true}}", call.Args["onSuccessUpdateEmail"])
					}
					create, _ := call.Args["create"].(map[string]any)
					if len(create) != 1 {
						t.Fatalf("create: %#v", create)
					}
					for _, v := range create {
						sub, _ := v.(map[string]any)
						emailID, _ = sub["emailId"].(string)
						identityID, _ = sub["identityId"].(string)
					}
				default:
					t.Fatalf("unexpected method %s", call.Name)
				}
			}
			writeMethods(w, "s1",
				[]any{"Email/import", map[string]any{
					"accountId": "A1", "oldState": "e0", "newState": "e1",
					"created": map[string]any{"email": map[string]any{"id": "E9"}},
				}, calls[0].ID},
				[]any{"EmailSubmission/set", map[string]any{
					"accountId": "A1", "newState": "s1",
					"created": map[string]any{"sub": map[string]any{"id": "S1", "emailId": "E9", "identityId": "ID9"}},
				}, calls[1].ID},
			)
		default:
			t.Fatalf("unexpected calls: %+v", calls)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "from@example.com", []string{"a@b.c", "d@e.f"})
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if apiPosts.Load() != 3 {
		t.Fatalf("api posts: got %d want 3 (identity + Sent lookup + import/set)", apiPosts.Load())
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads: got %d want 1", uploads.Load())
	}
	if !sawImport || !sawSubmission {
		t.Fatalf("sawImport=%v sawSubmission=%v", sawImport, sawSubmission)
	}
	if emailID != string(gojmap.CreationRef("email")) {
		t.Fatalf("emailId: got %q want %q", emailID, gojmap.CreationRef("email"))
	}
	if identityID != "ID9" {
		t.Fatalf("identityId: got %q want ID9", identityID)
	}
}

func TestSubmitCleansUpOnForbidden(t *testing.T) {
	var uploads atomic.Int32
	var phase atomic.Int32
	var destroyIDs []any

	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		switch phase.Add(1) {
		case 1:
			writeMethod(w, "s1", calls[0].ID, "Identity/get", map[string]any{
				"accountId": "A1", "state": "i1",
				"list":     []map[string]any{{"id": "ID9", "email": "user@example.com"}},
				"notFound": []string{},
			})
		case 2:
			if calls[0].Name != "Email/query" {
				t.Fatalf("Sent lookup method: got %s", calls[0].Name)
			}
			writeQuery(w, "s1", calls[0].ID, "Email/query", []string{})
		case 3:
			writeMethods(w, "s1",
				[]any{"Email/import", map[string]any{
					"accountId": "A1", "newState": "e1",
					"created": map[string]any{"email": map[string]any{"id": "E9"}},
				}, calls[0].ID},
				[]any{"error", map[string]any{"type": "forbidden"}, calls[1].ID},
			)
		case 4:
			if calls[0].Name != "Email/set" {
				t.Fatalf("cleanup method: got %s", calls[0].Name)
			}
			destroyIDs, _ = calls[0].Args["destroy"].([]any)
			writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
				"accountId": "A1", "newState": "e2",
				"destroyed": []string{"E9"},
			})
		default:
			t.Fatalf("extra request phase %d: %+v", phase.Load(), calls)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("want forbidden in error, got %v", err)
	}
	if len(destroyIDs) != 1 || destroyIDs[0] != "E9" {
		t.Fatalf("destroy: got %#v want [E9]", destroyIDs)
	}
}

func TestSubmitSessionStaleDoesNotReplay(t *testing.T) {
	var uploads atomic.Int32
	var apiPosts, sessionGETs atomic.Int32
	state := "A"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			sessionGETs.Add(1)
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionDocWithState(state, 256, 128))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/"):
			uploads.Add(1)
			writeUpload(w, "blob1", 3)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			apiPosts.Add(1)
			calls := parseCalls(t, mustRead(t, r.Body))
			switch {
			case len(calls) == 1 && calls[0].Name == "Identity/get":
				writeMethod(w, state, calls[0].ID, "Identity/get", map[string]any{
					"accountId": "A1", "state": "i1",
					"list":     []map[string]any{{"id": "ID9", "email": "user@example.com"}},
					"notFound": []string{},
				})
			case len(calls) == 1 && calls[0].Name == "Email/query":
				writeQuery(w, state, calls[0].ID, "Email/query", []string{})
			case len(calls) == 2:
				state = "B"
				writeMethods(w, "B",
					[]any{"Email/import", map[string]any{
						"accountId": "A1", "newState": "e1",
						"created": map[string]any{"email": map[string]any{"id": "E9"}},
					}, calls[0].ID},
					[]any{"EmailSubmission/set", map[string]any{
						"accountId": "A1", "newState": "s1",
						"created": map[string]any{"sub": map[string]any{"id": "S1"}},
					}, calls[1].ID},
				)
			default:
				t.Fatalf("unexpected calls: %+v", calls)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if apiPosts.Load() != 3 {
		t.Fatalf("api posts: got %d want 3 (identity + Sent lookup + import/set, no mutation replay)", apiPosts.Load())
	}

	before := sessionGETs.Load()
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("second Submit: %v", err)
	}
	if sessionGETs.Load() <= before {
		t.Fatal("expected RefreshSession before later call")
	}
}

func newSubmitServer(t *testing.T, sessionJSON string, uploads *atomic.Int32, onAPI func(http.ResponseWriter, []methodCall)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionJSON)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/upload/"):
			if uploads != nil {
				uploads.Add(1)
			}
			body, _ := io.ReadAll(r.Body)
			writeUpload(w, "blob1", len(body))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			onAPI(w, parseCalls(t, mustRead(t, r.Body)))
		default:
			http.NotFound(w, r)
		}
	}))
}

func writeUpload(w http.ResponseWriter, blobID string, size int) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"accountId": "A1",
		"blobId":    blobID,
		"type":      "application/octet-stream",
		"size":      size,
	})
}

func writeMethods(w http.ResponseWriter, sessionState string, responses ...[]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"methodResponses": responses,
		"sessionState":    sessionState,
	})
}

// writeQuery answers an Email/query or EmailSubmission/query call with ids.
func writeQuery(w http.ResponseWriter, sessionState, callID, name string, ids []string) {
	writeMethod(w, sessionState, callID, name, map[string]any{
		"accountId": "A1", "queryState": "q1", "canCalculateChanges": false,
		"position": 0, "ids": ids,
	})
}

func writeIdentity(w http.ResponseWriter, callID string) {
	writeMethod(w, "s1", callID, "Identity/get", map[string]any{
		"accountId": "A1", "state": "i1",
		"list":     []map[string]any{{"id": "ID9", "email": "user@example.com"}},
		"notFound": []string{},
	})
}

func writeImportAndSubmission(w http.ResponseWriter, calls []methodCall) {
	writeMethods(w, "s1",
		[]any{"Email/import", map[string]any{
			"accountId": "A1", "newState": "e1",
			"created": map[string]any{"email": map[string]any{"id": "E10"}},
		}, calls[0].ID},
		[]any{"EmailSubmission/set", map[string]any{
			"accountId": "A1", "newState": "s1",
			"created": map[string]any{"sub": map[string]any{"id": "S2"}},
		}, calls[1].ID},
	)
}

const rawWithMessageID = "Message-ID: <abc@example.com>\r\nFrom: me\r\n\r\nbody"

func assertSentLookup(t *testing.T, call methodCall) {
	t.Helper()
	filter, _ := call.Args["filter"].(map[string]any)
	if filter["inMailbox"] != "MbSent" {
		t.Fatalf("inMailbox: got %#v want MbSent", filter["inMailbox"])
	}
	if got := stringIDs(filter["header"]); len(got) != 2 || got[0] != "Message-ID" || got[1] != "abc@example.com" {
		t.Fatalf("header: got %#v want [Message-ID abc@example.com]", got)
	}
}

// writeSentCopy answers the request that inspects the Sent copy E9: its
// keywords (with $seen when seen) and the submissions that point at it.
func writeSentCopy(t *testing.T, w http.ResponseWriter, calls []methodCall, seen bool, submissionIDs []string) {
	t.Helper()
	writeSentCopyOf(t, w, calls, "E9", seen, submissionIDs)
}

// writeSentCopyOf is writeSentCopy for the Sent copy id.
func writeSentCopyOf(t *testing.T, w http.ResponseWriter, calls []methodCall, id string, seen bool, submissionIDs []string) {
	t.Helper()
	if len(calls) != 2 || calls[0].Name != "Email/get" || calls[1].Name != "EmailSubmission/query" {
		t.Fatalf("Sent copy check: got %+v", calls)
	}
	if got := stringIDs(calls[0].Args["ids"]); len(got) != 1 || got[0] != id {
		t.Fatalf("Email/get ids: got %#v want [%s]", got, id)
	}
	if got := stringIDs(calls[0].Args["properties"]); len(got) != 1 || got[0] != "keywords" {
		t.Fatalf("Email/get properties: got %#v want [keywords]", got)
	}
	filter, _ := calls[1].Args["filter"].(map[string]any)
	if got := stringIDs(filter["emailIds"]); len(got) != 1 || got[0] != id {
		t.Fatalf("emailIds: got %#v want [%s]", got, id)
	}
	keywords := map[string]bool{}
	if seen {
		keywords["$seen"] = true
	}
	writeMethods(w, "s1",
		[]any{"Email/get", map[string]any{
			"accountId": "A1", "state": "e1",
			"list":     []map[string]any{{"id": id, "keywords": keywords}},
			"notFound": []string{},
		}, calls[0].ID},
		[]any{"EmailSubmission/query", map[string]any{
			"accountId": "A1", "queryState": "q1", "canCalculateChanges": false,
			"position": 0, "ids": submissionIDs,
		}, calls[1].ID},
	)
}

func TestSubmitFindsExistingSubmission(t *testing.T) {
	var uploads atomic.Int32
	var sawCopyCheck bool
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/query":
			assertSentLookup(t, calls[0])
			writeQuery(w, "s1", calls[0].ID, "Email/query", []string{"E9"})
		case "Email/get":
			sawCopyCheck = true
			writeSentCopy(t, w, calls, false, []string{"S1"})
		default:
			t.Fatalf("unexpected method %s: the message was already sent", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !sawCopyCheck {
		t.Fatal("expected the Sent copy check (Email/get + EmailSubmission/query)")
	}
	if uploads.Load() != 0 {
		t.Fatalf("uploads: got %d want 0", uploads.Load())
	}
}

func TestSubmitRemovesAnUnsubmittedCopy(t *testing.T) {
	var uploads atomic.Int32
	var order []string
	var destroyIDs []string
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		order = append(order, calls[0].Name)
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/query":
			assertSentLookup(t, calls[0])
			writeQuery(w, "s1", calls[0].ID, "Email/query", []string{"E9"})
		case "Email/get":
			writeSentCopy(t, w, calls, false, []string{})
		case "Email/set":
			destroyIDs = stringIDs(calls[0].Args["destroy"])
			writeMethod(w, "s1", calls[0].ID, "Email/set", map[string]any{
				"accountId": "A1", "newState": "e2",
				"destroyed": []string{"E9"},
			})
		case "Email/import":
			writeImportAndSubmission(w, calls)
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if len(destroyIDs) != 1 || destroyIDs[0] != "E9" {
		t.Fatalf("destroy: got %#v want [E9]", destroyIDs)
	}
	want := []string{"Identity/get", "Email/query", "Email/get", "Email/set", "Email/import"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("order: got %v want %v", order, want)
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads: got %d want 1", uploads.Load())
	}
}

func TestSubmitWithNothingInSentSendsNormally(t *testing.T) {
	var uploads atomic.Int32
	var sawImport bool
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/query":
			assertSentLookup(t, calls[0])
			writeQuery(w, "s1", calls[0].ID, "Email/query", []string{})
		case "Email/import":
			sawImport = true
			writeImportAndSubmission(w, calls)
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !sawImport {
		t.Fatal("expected Email/import")
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads: got %d want 1", uploads.Load())
	}
}

func TestSubmitWithoutMessageIDSkipsTheSentLookup(t *testing.T) {
	var uploads atomic.Int32
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/import":
			writeImportAndSubmission(w, calls)
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte("From: me\r\n\r\nbody"), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads: got %d want 1", uploads.Load())
	}
}

func TestSubmitTreatsASeenCopyAsSent(t *testing.T) {
	var uploads atomic.Int32
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/query":
			writeQuery(w, "s1", calls[0].ID, "Email/query", []string{"E9"})
		case "Email/get":
			writeSentCopy(t, w, calls, true, []string{})
		default:
			t.Fatalf("unexpected method %s: the message was already sent", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if uploads.Load() != 0 {
		t.Fatalf("uploads: got %d want 0", uploads.Load())
	}
}

// answerSentScan serves a server that rejects the header and size filters:
// both lookups fail with unsupportedFilter, the last resort scan of Sent
// returns E8 and E9, and E9 carries scannedMessageID. It reports whether it answered the
// call.
func answerSentScan(t *testing.T, w http.ResponseWriter, calls []methodCall, scannedMessageID string) bool {
	t.Helper()
	switch calls[0].Name {
	case "Email/query":
		filter, _ := calls[0].Args["filter"].(map[string]any)
		_, byHeader := filter["header"]
		_, bySize := filter["minSize"]
		if byHeader || bySize {
			writeError(w, "s1", calls[0].ID, "unsupportedFilter")
			return true
		}
		assertSentScan(t, calls[0])
		writeQuery(w, "s1", calls[0].ID, "Email/query", []string{"E8", "E9"})
		return true
	case "Email/get":
		if props := stringIDs(calls[0].Args["properties"]); len(props) != 1 || props[0] != "messageId" {
			return false
		}
		if got := stringIDs(calls[0].Args["ids"]); len(got) != 2 || got[0] != "E8" || got[1] != "E9" {
			t.Fatalf("scan Email/get ids: got %#v want [E8 E9]", got)
		}
		writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
			"accountId": "A1", "state": "e1",
			"list": []map[string]any{
				{"id": "E8", "messageId": []string{"other@example.com"}},
				{"id": "E9", "messageId": []string{scannedMessageID}},
			},
			"notFound": []string{},
		})
		return true
	}
	return false
}

func assertSentScan(t *testing.T, call methodCall) {
	t.Helper()
	filter, _ := call.Args["filter"].(map[string]any)
	if filter["inMailbox"] != "MbSent" || len(filter) != 1 {
		t.Fatalf("scan filter: got %#v want {inMailbox: MbSent}", filter)
	}
	sort, _ := call.Args["sort"].([]any)
	if len(sort) != 1 {
		t.Fatalf("scan sort: got %#v", call.Args["sort"])
	}
	by, _ := sort[0].(map[string]any)
	if by["property"] != "receivedAt" || by["isAscending"] != false {
		t.Fatalf("scan sort: got %#v want receivedAt descending", by)
	}
	if call.Args["limit"] != float64(reconcileScan) {
		t.Fatalf("scan limit: got %#v want %d", call.Args["limit"], reconcileScan)
	}
}

func TestSubmitScansSentWhenTheServerCannotFilterByHeaderOrSize(t *testing.T) {
	var uploads atomic.Int32
	var sawCopyCheck bool
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		if answerSentScan(t, w, calls, "abc@example.com") {
			return
		}
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/get":
			sawCopyCheck = true
			writeSentCopy(t, w, calls, true, []string{})
		default:
			t.Fatalf("unexpected method %s: the message was already sent", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !sawCopyCheck {
		t.Fatal("expected the Sent copy check on the scanned match")
	}
	if uploads.Load() != 0 {
		t.Fatalf("uploads: got %d want 0", uploads.Load())
	}
}

func TestSubmitSendsWhenTheSentScanFindsNoCopy(t *testing.T) {
	var uploads atomic.Int32
	var sawImport bool
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		if answerSentScan(t, w, calls, "unrelated@example.com") {
			return
		}
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/import":
			sawImport = true
			writeImportAndSubmission(w, calls)
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !sawImport {
		t.Fatal("expected Email/import")
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads: got %d want 1", uploads.Load())
	}
}

func TestSubmitFailsWhenAFallbackLookupFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		// failing is the filter key of the query that fails with serverFail;
		// queries before it fail with unsupportedFilter.
		failing string
	}{
		{"size lookup", "minSize"},
		{"newest scan", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads atomic.Int32
			srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
				switch calls[0].Name {
				case "Identity/get":
					writeIdentity(w, calls[0].ID)
				case "Email/query":
					filter, _ := calls[0].Args["filter"].(map[string]any)
					_, failing := filter[tc.failing]
					if tc.failing == "" {
						failing = len(filter) == 1
					}
					if failing {
						writeError(w, "s1", calls[0].ID, "serverFail")
						return
					}
					writeError(w, "s1", calls[0].ID, "unsupportedFilter")
				default:
					t.Fatalf("unexpected method %s after a failed Sent lookup", calls[0].Name)
				}
			})
			defer srv.Close()

			c := newTestClient(t, srv)
			err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"})
			if err == nil || !strings.Contains(err.Error(), "serverFail") {
				t.Fatalf("want serverFail error, got %v", err)
			}
			if uploads.Load() != 0 {
				t.Fatalf("uploads: got %d want 0", uploads.Load())
			}
		})
	}
}

// sizeLookup serves a server that rejects the header filter but filters by
// size. page returns the ids of the n-th size query (from 0), newest the ids
// of the scan of the newest mail in Sent; the copy E7 carries the mail's
// Message-ID, every other candidate a different one.
type sizeLookup struct {
	page      func(n int) []string
	newest    []string
	scanned   bool
	positions []int
	gets      [][]string
}

func (s *sizeLookup) answer(t *testing.T, w http.ResponseWriter, calls []methodCall) bool {
	t.Helper()
	switch calls[0].Name {
	case "Email/query":
		filter, _ := calls[0].Args["filter"].(map[string]any)
		if _, ok := filter["header"]; ok {
			writeError(w, "s1", calls[0].ID, "unsupportedFilter")
			return true
		}
		if len(filter) == 1 {
			assertSentScan(t, calls[0])
			s.scanned = true
			writeQuery(w, "s1", calls[0].ID, "Email/query", s.newest)
			return true
		}
		n := float64(len(rawWithMessageID))
		if filter["inMailbox"] != "MbSent" || filter["minSize"] != n || filter["maxSize"] != n+1 || len(filter) != 3 {
			t.Fatalf("size filter: got %#v want {inMailbox: MbSent, minSize: %v, maxSize: %v}", filter, n, n+1)
		}
		if calls[0].Args["limit"] != float64(sizeLookupPage) {
			t.Fatalf("size lookup limit: got %#v want %d", calls[0].Args["limit"], sizeLookupPage)
		}
		pos, _ := calls[0].Args["position"].(float64)
		s.positions = append(s.positions, int(pos))
		writeQuery(w, "s1", calls[0].ID, "Email/query", s.page(len(s.positions)-1))
		return true
	case "Email/get":
		if props := stringIDs(calls[0].Args["properties"]); len(props) != 1 || props[0] != "messageId" {
			return false
		}
		ids := stringIDs(calls[0].Args["ids"])
		s.gets = append(s.gets, ids)
		list := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			mid := id + "@other.example.com"
			if id == "E7" {
				mid = "abc@example.com"
			}
			list = append(list, map[string]any{"id": id, "messageId": []string{mid}})
		}
		writeMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
			"accountId": "A1", "state": "e1", "list": list, "notFound": []string{},
		})
		return true
	}
	return false
}

// candidates returns n ids that are not E7.
func candidates(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return ids
}

func TestSubmitFindsAnOlderCopyBySize(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pages         [][]string
		wantPositions []int
	}{
		{"one page", [][]string{{"E7"}}, []int{0, 1}},
		{"second page", [][]string{candidates("P", sizeLookupPage), {"E7", "Q1"}}, []int{0, sizeLookupPage, sizeLookupPage + 2}},
		{"server caps the limit", [][]string{candidates("P", 100), candidates("Q", 100), {"E7"}}, []int{0, 100, 200, 201}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads atomic.Int32
			var sawCopyCheck bool
			lookup := &sizeLookup{page: func(n int) []string {
				if n < len(tc.pages) {
					return tc.pages[n]
				}
				return []string{}
			}}
			srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
				if lookup.answer(t, w, calls) {
					return
				}
				switch calls[0].Name {
				case "Identity/get":
					writeIdentity(w, calls[0].ID)
				case "Email/get":
					sawCopyCheck = true
					writeSentCopyOf(t, w, calls, "E7", true, []string{})
				default:
					t.Fatalf("unexpected method %s: the message was already sent", calls[0].Name)
				}
			})
			defer srv.Close()

			c := newTestClient(t, srv)
			if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if !sawCopyCheck {
				t.Fatal("expected the Sent copy check on the size match")
			}
			if lookup.scanned {
				t.Fatal("scanned the newest mail in Sent after a size match")
			}
			if uploads.Load() != 0 {
				t.Fatalf("uploads: got %d want 0", uploads.Load())
			}
			if fmt.Sprint(lookup.positions) != fmt.Sprint(tc.wantPositions) {
				t.Fatalf("positions: got %v want %v", lookup.positions, tc.wantPositions)
			}
			for _, ids := range lookup.gets {
				if len(ids) > 256 {
					t.Fatalf("Email/get of %d ids exceeds maxObjectsInGet 256", len(ids))
				}
			}
		})
	}
}

func TestSubmitScansSentWhenTheSizeLookupFindsNoCopy(t *testing.T) {
	var uploads atomic.Int32
	var sawCopyCheck bool
	lookup := &sizeLookup{
		page: func(n int) []string {
			if n == 0 {
				return []string{"E5"}
			}
			return []string{}
		},
		newest: []string{"E8", "E7"},
	}
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		if lookup.answer(t, w, calls) {
			return
		}
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/get":
			sawCopyCheck = true
			writeSentCopyOf(t, w, calls, "E7", true, []string{})
		default:
			t.Fatalf("unexpected method %s: the message was already sent", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !lookup.scanned || !sawCopyCheck {
		t.Fatalf("scanned=%v sawCopyCheck=%v: want the newest scan to find E7", lookup.scanned, sawCopyCheck)
	}
	if uploads.Load() != 0 {
		t.Fatalf("uploads: got %d want 0", uploads.Load())
	}
}

func TestSubmitSendsWhenNeitherSizeNorScanFindsACopy(t *testing.T) {
	var uploads atomic.Int32
	var sawImport bool
	lookup := &sizeLookup{
		page: func(n int) []string {
			if n == 0 {
				return []string{"E5", "E6"}
			}
			return []string{}
		},
		newest: []string{"E8"},
	}
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		if lookup.answer(t, w, calls) {
			return
		}
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/import":
			sawImport = true
			writeImportAndSubmission(w, calls)
		default:
			t.Fatalf("unexpected method %s", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !sawImport {
		t.Fatal("expected Email/import")
	}
	if !lookup.scanned {
		t.Fatal("expected the newest scan after an empty size lookup")
	}
	if uploads.Load() != 1 {
		t.Fatalf("uploads: got %d want 1", uploads.Load())
	}
}

func TestSubmitFailsWhenTheSizeLookupHasTooManyCandidates(t *testing.T) {
	var uploads atomic.Int32
	lookup := &sizeLookup{page: func(n int) []string {
		return candidates(fmt.Sprintf("P%d-", n), sizeLookupPage)
	}}
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		if lookup.answer(t, w, calls) {
			return
		}
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		default:
			t.Fatalf("unexpected method %s after too many size candidates", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	if err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"}); err == nil {
		t.Fatal("expected an error past the candidate cap")
	}
	if uploads.Load() != 0 {
		t.Fatalf("uploads: got %d want 0", uploads.Load())
	}
	if want := sizeLookupCap/sizeLookupPage + 1; len(lookup.positions) != want {
		t.Fatalf("size queries: got %d want %d", len(lookup.positions), want)
	}
}

func TestSubmitFailsWhenTheSentLookupFails(t *testing.T) {
	var uploads atomic.Int32
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/query":
			writeError(w, "s1", calls[0].ID, "serverFail")
		default:
			t.Fatalf("unexpected method %s after a failed Sent lookup", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"})
	if err == nil || !strings.Contains(err.Error(), "serverFail") {
		t.Fatalf("want serverFail error, got %v", err)
	}
	if uploads.Load() != 0 {
		t.Fatalf("uploads: got %d want 0", uploads.Load())
	}
}

func TestSubmitFailsWhenTheUnsentCopyCannotBeRemoved(t *testing.T) {
	var uploads atomic.Int32
	srv := newSubmitServer(t, sessionDocLimits(256, 128), &uploads, func(w http.ResponseWriter, calls []methodCall) {
		switch calls[0].Name {
		case "Identity/get":
			writeIdentity(w, calls[0].ID)
		case "Email/query":
			writeQuery(w, "s1", calls[0].ID, "Email/query", []string{"E9"})
		case "Email/get":
			writeSentCopy(t, w, calls, false, []string{})
		case "Email/set":
			writeError(w, "s1", calls[0].ID, "serverFail")
		default:
			t.Fatalf("unexpected method %s after a failed destroy", calls[0].Name)
		}
	})
	defer srv.Close()

	c := newTestClient(t, srv)
	err := Submit(context.Background(), c, "A1", "MbSent", []byte(rawWithMessageID), "user@example.com", "user@example.com", []string{"a@b.c"})
	if err == nil || !strings.Contains(err.Error(), "serverFail") {
		t.Fatalf("want serverFail error, got %v", err)
	}
	if uploads.Load() != 0 {
		t.Fatalf("uploads: got %d want 0", uploads.Load())
	}
}
