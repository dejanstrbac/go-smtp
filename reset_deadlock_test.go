package smtp_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"sync"
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

// serializedSession records whether Logout ran while Reset was still in
// flight. Reset blocks until the test releases it.
type serializedSession struct {
	resetEntered  chan struct{}
	releaseReset  chan struct{}
	mu            sync.Mutex
	inReset       bool
	logoutOverlap bool
	loggedOut     chan struct{}
}

func (s *serializedSession) Reset() {
	s.mu.Lock()
	s.inReset = true
	s.mu.Unlock()

	close(s.resetEntered)
	<-s.releaseReset

	s.mu.Lock()
	s.inReset = false
	s.mu.Unlock()
}

func (s *serializedSession) Logout() error {
	s.mu.Lock()
	if s.inReset {
		s.logoutOverlap = true
	}
	s.mu.Unlock()
	close(s.loggedOut)
	return nil
}

func (s *serializedSession) Mail(ctx context.Context, from string, opts *smtp.MailOptions) error {
	return nil
}

func (s *serializedSession) Rcpt(ctx context.Context, to string, opts *smtp.RcptOptions) error {
	return nil
}

func (s *serializedSession) Data(ctx context.Context, r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}

type serializedBackend struct{ session *serializedSession }

func (be *serializedBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return be.session, nil
}

// Reset and Logout must never run at the same time: Server.Close calls Logout
// from its own goroutine while the serve goroutine may be inside Reset, and a
// backend that mutates session state in both would race. Conn.reset used to
// serialize them by holding the connection lock across Reset; it no longer
// holds that lock (it deadlocks backends), so the serialization has to come
// from somewhere else.
func TestServerResetAndLogoutDoNotOverlap(t *testing.T) {
	session := &serializedSession{
		resetEntered: make(chan struct{}),
		releaseReset: make(chan struct{}),
		loggedOut:    make(chan struct{}),
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := smtp.NewServer(&serializedBackend{session: session})
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	go s.Serve(l)
	defer l.Close()

	c, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	scanner := bufio.NewScanner(c)
	scanRespect(t, scanner, "220 localhost ESMTP Service Ready")

	// EHLO first: the session is created there, and RSET only calls Reset
	// once one exists.
	io.WriteString(c, "EHLO localhost\r\n")
	for scanner.Scan() {
		if line := scanner.Text(); len(line) >= 4 && line[3] == ' ' {
			break
		}
	}

	// Park the connection goroutine inside Session.Reset.
	io.WriteString(c, "RSET\r\n")
	select {
	case <-session.resetEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Session.Reset was never called")
	}

	// Close the server while Reset is still running; its Logout must wait.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		s.Close()
	}()

	select {
	case <-session.loggedOut:
		t.Fatal("Logout ran while Reset was still in flight")
	case <-time.After(200 * time.Millisecond):
	}

	close(session.releaseReset)

	select {
	case <-session.loggedOut:
	case <-time.After(5 * time.Second):
		t.Fatal("Logout never ran after Reset returned")
	}
	<-closed

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.logoutOverlap {
		t.Error("Logout overlapped Reset")
	}
}
