package desktop

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peltonapp/Pelton/internal/credentials"
	"github.com/peltonapp/Pelton/internal/storage"
)

// jmapHarnessMailbox configures one mailbox in the metrics harness server.
type jmapHarnessMailbox struct {
	RemoteID string
	Name     string
	Role     string // "inbox" or empty
	Messages int
}

type jmapSyncMetrics struct {
	PeakBodyAPIPosts int32
	WallTime         time.Duration
}

const jmapMetricsBodyAPIDelay = 40 * time.Millisecond

// runJMAPSyncHarness runs a full JMAP account sync against an httptest server
// and records peak concurrent /api POST handlers during the body-fetch phase.
func runJMAPSyncHarness(t *testing.T, mailboxes []jmapHarnessMailbox, nParallel int) jmapSyncMetrics {
	t.Helper()
	if len(mailboxes) == 0 {
		t.Fatal("runJMAPSyncHarness: need at least one mailbox")
	}

	ctx, stopBackground := testContext(t)
	t.Cleanup(stopBackground)
	db, err := storage.Open(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.RunMigrations(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := db.SetInt(ctx, settingSyncMaxParallel, nParallel); err != nil {
		t.Fatalf("set sync_max_parallel: %v", err)
	}
	limit := 30
	for _, mb := range mailboxes {
		if mb.Messages > limit {
			limit = mb.Messages
		}
	}
	if err := db.SetInt(ctx, settingSyncMessageLimit, limit); err != nil {
		t.Fatalf("set sync_message_limit: %v", err)
	}

	type msgRec struct {
		mailbox string
		idx     int
	}
	byID := make(map[string]msgRec)
	var mailboxList []map[string]any
	for _, mb := range mailboxes {
		role := mb.Role
		if role == "inbox" {
			role = "inbox"
		}
		entry := map[string]any{
			"id": mb.RemoteID, "name": mb.Name, "role": role,
			"sortOrder": len(mailboxList), "totalEmails": mb.Messages,
			"unreadEmails": 0, "totalThreads": mb.Messages, "unreadThreads": 0,
		}
		mailboxList = append(mailboxList, entry)
		for i := 1; i <= mb.Messages; i++ {
			id := fmt.Sprintf("%s-m%02d", mb.RemoteID, i)
			byID[id] = msgRec{mailbox: mb.RemoteID, idx: i}
		}
	}

	var inflightAPI atomic.Int32
	var peakBodyAPI atomic.Int32
	var bodyPhase atomic.Bool

	trackBodyPOST := func() {
		if !bodyPhase.Load() {
			return
		}
		cur := inflightAPI.Add(1)
		for {
			old := peakBodyAPI.Load()
			if cur <= old || peakBodyAPI.CompareAndSwap(old, cur) {
				break
			}
		}
		defer inflightAPI.Add(-1)
		time.Sleep(jmapMetricsBodyAPIDelay)
	}

	rawBlob := "Subject: t\r\n\r\nbody\r\n"

	apiHandler := func(w http.ResponseWriter, r *http.Request, calls []metricsJMAPCall) {
		switch calls[0].Name {
		case "Mailbox/get":
			writeMetricsMethod(w, "s1", calls[0].ID, "Mailbox/get", map[string]any{
				"accountId": "A1", "state": "mb1", "list": mailboxList, "notFound": []string{},
			})
		case "Email/query":
			mbox := metricsQueryMailbox(calls[0].Args)
			var ids []string
			for id, rec := range byID {
				if rec.mailbox == mbox {
					ids = append(ids, id)
				}
			}
			sortMetricsIDs(ids)
			writeMetricsMethod(w, "s1", calls[0].ID, "Email/query", map[string]any{
				"accountId": "A1", "queryState": "q1", "canCalculateChanges": true,
				"position": 0, "ids": ids, "total": len(ids),
			})
		case "Email/get":
			props := metricsEmailProps(calls[0].Args)
			bodyFetch := props["blobId"]
			if bodyFetch {
				bodyPhase.Store(true)
				trackBodyPOST()
			}
			reqIDs := metricsStringIDs(calls[0].Args["ids"])
			if len(reqIDs) == 0 {
				writeMetricsMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
					"accountId": "A1", "state": "em1", "list": []any{}, "notFound": []string{},
				})
				return
			}
			list := make([]map[string]any, 0, len(reqIDs))
			for _, id := range reqIDs {
				rec, ok := byID[id]
				if !ok {
					continue
				}
				em := map[string]any{
					"id":         id,
					"keywords":   map[string]bool{},
					"mailboxIds": map[string]bool{rec.mailbox: true},
					"receivedAt": fmt.Sprintf("2024-06-%02dT12:00:00Z", (rec.idx%28)+1),
				}
				if bodyFetch {
					em["blobId"] = "blob-" + id
				} else {
					em["subject"] = id
					em["preview"] = "preview-" + id
					em["from"] = []map[string]any{{"email": "a@ex.test"}}
					em["to"] = []map[string]any{{"email": "b@ex.test"}}
					em["size"] = 10
					em["hasAttachment"] = false
				}
				list = append(list, em)
			}
			writeMetricsMethod(w, "s1", calls[0].ID, "Email/get", map[string]any{
				"accountId": "A1", "state": "em1", "list": list, "notFound": []string{},
			})
		default:
			t.Fatalf("unexpected JMAP method %q", calls[0].Name)
		}
	}

	sessionJSON := metricsSessionDoc(256, 128)
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/.well-known/jmap"):
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, sessionJSON)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/api"):
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read api body: %v", err)
			}
			apiHandler(w, r, metricsParseCalls(t, body))
		case strings.Contains(r.URL.Path, "/download/"):
			w.Header().Set("Content-Type", "application/octet-stream")
			io.WriteString(w, rawBlob)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	accountID, err := db.CreateAccount(ctx, &storage.Account{
		Email:             "user@example.com",
		Protocol:          "jmap",
		JMAPSessionURL:    srv.URL + "/.well-known/jmap",
		JMAPMailAccountID: "A1",
	})
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := credentials.Store(accountID, credentials.Secret{
		Method: credentials.MethodPassword, Password: "secret",
	}); err != nil {
		t.Fatalf("store credentials: %v", err)
	}
	t.Cleanup(func() { _ = credentials.Delete(accountID) })

	account, err := db.GetAccount(ctx, accountID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}

	a := &App{ctx: ctx, store: db, log: slog.New(slog.DiscardHandler)}
	t.Cleanup(func() { a.stopAccountScheduler(account.ID) })

	start := time.Now()
	if err := a.syncAccountOnceJMAP(ctx, *account); err != nil {
		t.Fatalf("syncAccountOnceJMAP: %v", err)
	}
	return jmapSyncMetrics{
		PeakBodyAPIPosts: peakBodyAPI.Load(),
		WallTime:         time.Since(start),
	}
}

func TestJMAPBodyPhaseUsesMoreThanOneConcurrentSlotWhenNIs3(t *testing.T) {
	mailboxes := []jmapHarnessMailbox{
		{RemoteID: "MbInbox", Name: "INBOX", Role: "inbox", Messages: 30},
		{RemoteID: "MbA", Name: "FolderA", Messages: 30},
		{RemoteID: "MbB", Name: "FolderB", Messages: 30},
	}
	m := runJMAPSyncHarness(t, mailboxes, 3)
	t.Logf("body-phase peak concurrent /api POSTs=%d wall=%v", m.PeakBodyAPIPosts, m.WallTime)
	// Body chunks of different folders must overlap when the pool allows it.
	if m.PeakBodyAPIPosts < 2 {
		t.Fatalf("peak body-phase /api POST concurrency=%d, want >= 2 when sync_max_parallel=3 and >=3 body chunks", m.PeakBodyAPIPosts)
	}
}

// --- httptest helpers (local to metrics harness) ---

type metricsJMAPCall struct {
	Name string
	Args map[string]any
	ID   string
}

func metricsParseCalls(t *testing.T, body []byte) []metricsJMAPCall {
	t.Helper()
	var req struct {
		MethodCalls [][]json.RawMessage `json:"methodCalls"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal JMAP request: %v\n%s", err, body)
	}
	out := make([]metricsJMAPCall, 0, len(req.MethodCalls))
	for _, call := range req.MethodCalls {
		if len(call) != 3 {
			t.Fatalf("bad method call: %s", call)
		}
		var name, id string
		var args map[string]any
		_ = json.Unmarshal(call[0], &name)
		_ = json.Unmarshal(call[1], &args)
		_ = json.Unmarshal(call[2], &id)
		out = append(out, metricsJMAPCall{Name: name, Args: args, ID: id})
	}
	return out
}

func writeMetricsMethod(w http.ResponseWriter, sessionState, callID, name string, args map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"methodResponses": []any{[]any{name, args, callID}},
		"sessionState":    sessionState,
	})
}

func metricsStringIDs(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, x := range arr {
		out = append(out, fmt.Sprint(x))
	}
	return out
}

func metricsEmailProps(args map[string]any) map[string]bool {
	set := map[string]bool{}
	props, ok := args["properties"].([]any)
	if !ok {
		return set
	}
	for _, p := range props {
		set[fmt.Sprint(p)] = true
	}
	return set
}

func metricsQueryMailbox(args map[string]any) string {
	filter, ok := args["filter"].(map[string]any)
	if !ok {
		return ""
	}
	if inMbox, ok := filter["inMailbox"].(string); ok {
		return inMbox
	}
	if inMbox, ok := filter["inMailboxId"].(string); ok {
		return inMbox
	}
	return ""
}

func sortMetricsIDs(ids []string) {
	// Newest-first order matches harness id suffix m01..m30 (higher idx = newer).
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[i] < ids[j] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}
}

func metricsSessionDoc(maxGet, maxSet int) string {
	return fmt.Sprintf(`{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 8,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequest": 8,
      "maxCallsInRequest": 32,
      "maxObjectsInGet": %d,
      "maxObjectsInSet": %d,
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
        "urn:ietf:params:jmap:mail": {},
        "urn:ietf:params:jmap:submission": {}
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
}`, maxGet, maxSet)
}
