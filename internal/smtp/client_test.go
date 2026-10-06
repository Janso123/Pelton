package smtp

import (
	"bufio"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/peltonapp/Pelton/internal/certtrust"
	"github.com/peltonapp/Pelton/internal/outbox"
)

// serveData answers a whole submission up to the message body, then ends the
// body with finish: it either drops the connection or sends a reply.
func serveData(finish func(conn net.Conn)) func(net.Conn) {
	return func(conn net.Conn) {
		defer conn.Close()
		r := bufio.NewReader(conn)
		_, _ = conn.Write([]byte("220 test ready\r\n"))
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					finish(conn)
					return
				}
				continue
			}
			switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				_, _ = conn.Write([]byte("250 test\r\n"))
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				_, _ = conn.Write([]byte("250 ok\r\n"))
			case strings.HasPrefix(cmd, "DATA"):
				_, _ = conn.Write([]byte("354 go ahead\r\n"))
				inData = true
			default:
				_, _ = conn.Write([]byte("502 not here\r\n"))
			}
		}
	}
}

func sendVia(t *testing.T, finish func(conn net.Conn)) error {
	t.Helper()
	port, fp := selfSignedServer(t, serveData(finish))
	client, err := Dial(Config{Host: "127.0.0.1", Port: port, TLS: TLSImplicit, Trust: certtrust.Trust{Pins: []string{fp}}})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return client.Send(t.Context(), "a@example.com", []string{"b@example.com"}, []byte("Subject: hi\r\n\r\nbody\r\n"))
}

// A connection that drops after the body was handed over leaves the outcome
// unknown, so the error must say the message may have been sent.
func TestSendMarksADroppedConnectionAfterDataAsMaybeSent(t *testing.T) {
	err := sendVia(t, func(net.Conn) {})
	if !errors.Is(err, outbox.ErrMaybeSent) {
		t.Fatalf("Send() error = %v, want ErrMaybeSent", err)
	}
}

// A reply from the server is a clear answer, so a rejection is safe to retry.
func TestSendDoesNotMarkARejectionAsMaybeSent(t *testing.T) {
	err := sendVia(t, func(conn net.Conn) { _, _ = conn.Write([]byte("554 rejected\r\n")) })
	if err == nil {
		t.Fatal("Send() succeeded, want the rejection")
	}
	if errors.Is(err, outbox.ErrMaybeSent) {
		t.Fatalf("Send() error = %v, must not be ErrMaybeSent", err)
	}
}
