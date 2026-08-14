package smtp_test

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-smtp"
)

// sendEhloReadCaps sends EHLO and returns the first reply line (without the
// "250-" prefix) and the set of capability lines.
func sendEhloReadCaps(t *testing.T, c io.Writer, scanner *bufio.Scanner) (first string, caps []string) {
	t.Helper()

	io.WriteString(c, "EHLO client.example.org\r\n")

	scanner.Scan()
	line := scanner.Text()
	if !strings.HasPrefix(line, "250-") && !strings.HasPrefix(line, "250 ") {
		t.Fatal("Invalid EHLO response:", line)
	}
	first = line[4:]
	if strings.HasPrefix(line, "250 ") {
		return first, nil // single-line reply, no capabilities
	}

	for scanner.Scan() {
		s := scanner.Text()
		if strings.HasPrefix(s, "250 ") {
			caps = append(caps, strings.TrimPrefix(s, "250 "))
			return first, caps
		}
		if !strings.HasPrefix(s, "250-") {
			t.Fatal("Invalid capability response:", s)
		}
		caps = append(caps, strings.TrimPrefix(s, "250-"))
	}
	t.Fatal("EHLO reply ended without a terminating 250 line")
	return "", nil
}

func hasCap(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// RFC 5321 §4.1.1.1: the first line of an EHLO reply must carry the server's
// domain, not free text. The classic Outlook lineage reads this line.
func TestEhloFirstLineCarriesServerDomain(t *testing.T) {
	_, s, c, scanner := testServerGreeted(t)
	defer s.Close()
	defer c.Close()

	first, _ := sendEhloReadCaps(t, c, scanner)
	if first != "localhost Hello client.example.org" {
		t.Fatal("EHLO first line must start with the server domain, got:", first)
	}
}

// When Server.Domain is unset the historical "Hello <client>" form is kept.
func TestEhloFirstLineNoDomainFallback(t *testing.T) {
	_, s, c, scanner := testServer(t, func(s *smtp.Server) {
		s.Domain = ""
	})
	defer s.Close()
	defer c.Close()

	scanner.Scan() // greeting (domain-less)

	first, _ := sendEhloReadCaps(t, c, scanner)
	if first != "Hello client.example.org" {
		t.Fatal("Expected fallback greeting without domain, got:", first)
	}
}

// HELO replies must also lead with the server domain.
func TestHeloReplyCarriesServerDomain(t *testing.T) {
	_, s, c, scanner := testServerGreeted(t)
	defer s.Close()
	defer c.Close()

	io.WriteString(c, "HELO client.example.org\r\n")
	scanner.Scan()
	if scanner.Text() != "250 2.0.0 localhost Hello client.example.org" {
		t.Fatal("HELO reply must start with the server domain, got:", scanner.Text())
	}
}

// EnableLegacyAuthCap adds the obsolete "AUTH=<mechs>" line that Microsoft
// clients (Outlook) look for — the same workaround as Postfix's
// broken_sasl_auth_clients. It must mirror the standard AUTH line exactly.
func TestLegacyAuthCapEnabled(t *testing.T) {
	_, s, c, scanner := testServerGreeted(t, func(s *smtp.Server) {
		s.EnableLegacyAuthCap = true
	})
	defer s.Close()
	defer c.Close()

	_, caps := sendEhloReadCaps(t, c, scanner)
	if !hasCap(caps, "AUTH PLAIN") {
		t.Fatal("Missing standard AUTH capability, got:", caps)
	}
	if !hasCap(caps, "AUTH=PLAIN") {
		t.Fatal("Missing legacy AUTH= capability, got:", caps)
	}
}

// Off by default: no legacy line unless explicitly enabled.
func TestLegacyAuthCapDisabledByDefault(t *testing.T) {
	_, s, c, scanner := testServerGreeted(t)
	defer s.Close()
	defer c.Close()

	_, caps := sendEhloReadCaps(t, c, scanner)
	if !hasCap(caps, "AUTH PLAIN") {
		t.Fatal("Missing standard AUTH capability, got:", caps)
	}
	for _, cap := range caps {
		if strings.HasPrefix(cap, "AUTH=") {
			t.Fatal("Legacy AUTH= capability advertised without EnableLegacyAuthCap:", cap)
		}
	}
}

// No AUTH advertised at all -> no legacy line either, even when enabled.
func TestLegacyAuthCapNoMechanisms(t *testing.T) {
	be, s, c, scanner := testServerGreeted(t, func(s *smtp.Server) {
		s.EnableLegacyAuthCap = true
	})
	defer s.Close()
	defer c.Close()
	be.authDisabled = true

	_, caps := sendEhloReadCaps(t, c, scanner)
	for _, cap := range caps {
		if strings.HasPrefix(cap, "AUTH") {
			t.Fatal("AUTH capability advertised with auth disabled:", cap)
		}
	}
}

// LIMITS (RFC 9422) is advertised when MaxRecipients is set...
func TestLimitsCapDefault(t *testing.T) {
	_, s, c, scanner := testServerGreeted(t, func(s *smtp.Server) {
		s.MaxRecipients = 100
	})
	defer s.Close()
	defer c.Close()

	_, caps := sendEhloReadCaps(t, c, scanner)
	if !hasCap(caps, "LIMITS RCPTMAX=100") {
		t.Fatal("Missing LIMITS capability, got:", caps)
	}
}

// ...and suppressed by DisableLimitsCap for clients whose parsers choke on it.
func TestLimitsCapDisabled(t *testing.T) {
	_, s, c, scanner := testServerGreeted(t, func(s *smtp.Server) {
		s.MaxRecipients = 100
		s.DisableLimitsCap = true
	})
	defer s.Close()
	defer c.Close()

	_, caps := sendEhloReadCaps(t, c, scanner)
	for _, cap := range caps {
		if strings.HasPrefix(cap, "LIMITS") {
			t.Fatal("LIMITS capability advertised despite DisableLimitsCap:", cap)
		}
	}
}
