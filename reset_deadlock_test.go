package smtp_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

// resetAccessorSession mimics a backend that reaches back into the connection
// from Reset — e.g. to refresh an idle deadline with Conn().SetDeadline. Those
// accessors take Conn's internal lock, so Conn.reset must not hold it across
// the callback.
type resetAccessorSession struct {
	c *smtp.Conn
}

func (s *resetAccessorSession) Reset() {
	s.c.Conn().SetDeadline(time.Now().Add(time.Minute))
	s.c.TLSConnectionState()
	s.c.Session()
}

func (s *resetAccessorSession) Logout() error { return nil }

func (s *resetAccessorSession) Mail(ctx context.Context, from string, opts *smtp.MailOptions) error {
	return nil
}

func (s *resetAccessorSession) Rcpt(ctx context.Context, to string, opts *smtp.RcptOptions) error {
	return nil
}

func (s *resetAccessorSession) Data(ctx context.Context, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

type resetAccessorBackend struct{}

func (be *resetAccessorBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &resetAccessorSession{c: c}, nil
}

// testResetAccessorServer starts a server whose sessions call locked Conn
// accessors from Reset. The client connection carries a deadline so a
// regression fails the test instead of hanging the test binary.
func testResetAccessorServer(t *testing.T) (*smtp.Server, net.Conn, *bufio.Scanner) {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	s := smtp.NewServer(&resetAccessorBackend{})
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	go s.Serve(l)

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		s.Close()
	})

	scanner := bufio.NewScanner(c)
	scanRespect(t, scanner, "220 localhost ESMTP Service Ready")

	io.WriteString(c, "EHLO localhost\r\n")
	for scanner.Scan() {
		if line := scanner.Text(); len(line) >= 4 && line[3] == ' ' {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal("EHLO:", err)
	}

	return s, c, scanner
}

func scanRespect(t *testing.T, scanner *bufio.Scanner, want string) {
	t.Helper()
	if !scanner.Scan() {
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		t.Fatalf("expected %q, got no response: %v", want, err)
	}
	if got := scanner.Text(); got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}

// A backend that calls locked Conn accessors from Reset must not deadlock the
// connection goroutine when the client issues RSET.
func TestServerResetCallsLockedConnAccessors(t *testing.T) {
	_, c, scanner := testResetAccessorServer(t)

	io.WriteString(c, "RSET\r\n")
	scanRespect(t, scanner, "250 2.0.0 Session reset")

	io.WriteString(c, "NOOP\r\n")
	scanRespect(t, scanner, "250 2.0.0 I have successfully done nothing")
}

// handleData resets the connection on return, so the same deadlock would strand
// the session immediately after a message is accepted: the client sees its 250
// and then never gets another reply.
func TestServerResetAfterDataCallsLockedConnAccessors(t *testing.T) {
	_, c, scanner := testResetAccessorServer(t)

	io.WriteString(c, "MAIL FROM:<root@nsa.gov>\r\n")
	scanRespect(t, scanner, "250 2.0.0 Roger, accepting mail from <root@nsa.gov>")

	io.WriteString(c, "RCPT TO:<root@gchq.gov.uk>\r\n")
	scanRespect(t, scanner, "250 2.0.0 I'll make sure <root@gchq.gov.uk> gets this")

	io.WriteString(c, "DATA\r\n")
	scanRespect(t, scanner, "354 Go ahead. End your data with <CR><LF>.<CR><LF>")

	io.WriteString(c, "Hey\r\n\r\nHow are you?\r\n.\r\n")
	scanRespect(t, scanner, "250 2.0.0 OK: queued")

	// The session must still be alive for the commands a real client sends
	// after a successful message: another transaction, or QUIT.
	io.WriteString(c, "QUIT\r\n")
	scanRespect(t, scanner, "221 2.0.0 Bye")
}
