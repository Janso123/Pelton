package imap

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/imapclient"
)

// scriptedSelect starts a server that answers every SELECT with the given
// untagged lines and returns a client connected to it.
func scriptedSelect(t *testing.T, untagged ...string) *Client {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprint(conn, "* OK ready\r\n")
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
			if !strings.HasPrefix(strings.ToUpper(rest), "SELECT") {
				fmt.Fprintf(conn, "%s OK done\r\n", tag)
				continue
			}
			for _, l := range untagged {
				fmt.Fprint(conn, l+"\r\n")
			}
			fmt.Fprintf(conn, "%s OK [READ-WRITE] selected\r\n", tag)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	raw := imapclient.New(conn, nil)
	t.Cleanup(func() { _ = raw.Close() })
	return &Client{raw: raw}
}

// Gmail lists every keyword ever set on the account in each folder's FLAGS,
// so one keyword with a "]" in it, left by another client, used to fail the
// SELECT of every folder at once (#474).
func TestSelectAcceptsAKeywordWithABracket(t *testing.T) {
	c := scriptedSelect(t,
		`* FLAGS (\Answered \Flagged \Draft \Deleted \Seen $NotPhishing $Phishing [Imap]/Todo)`,
		`* OK [PERMANENTFLAGS (\Answered \Flagged \Draft \Deleted \Seen $NotPhishing $Phishing [Imap]/Todo \*)] Flags permitted.`,
		`* OK [UIDVALIDITY 1] UIDs valid.`,
		`* 3 EXISTS`,
		`* 0 RECENT`,
		`* OK [UIDNEXT 4] Predicted next UID.`,
	)

	mbox, err := c.Select("#all")
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if mbox.NumMessages != 3 {
		t.Errorf("NumMessages = %d, want 3", mbox.NumMessages)
	}
}
