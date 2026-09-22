package collab

import (
	"errors"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"moesekai/server/internal/auth"
)

func requireTicketQueryRejectedAndConsumed(t *testing.T, fixture contractServiceFixture, rawQuery string, tickets ...Ticket) {
	t.Helper()
	request := httptest.NewRequest("GET", "http://example.test/yjs/lyrics/42", nil)
	request.URL.RawQuery = rawQuery
	request.SetPathValue("musicId", "42")
	if _, accepted := fixture.service.authorize(request); accepted {
		for _, ticket := range tickets {
			fixture.service.untrack(ticket.Room, fixture.claims.Username)
		}
		t.Fatal("invalid collaboration ticket query was accepted")
	}

	fixture.service.ticketsMu.Lock()
	for _, ticket := range tickets {
		if _, remains := fixture.service.tickets[ticket.Ticket]; remains {
			fixture.service.ticketsMu.Unlock()
			t.Fatalf("ticket %q survived an invalid query attempt", ticket.Ticket)
		}
	}
	fixture.service.ticketsMu.Unlock()

	for _, ticket := range tickets {
		replay := httptest.NewRequest("GET", "http://example.test/yjs/lyrics/42?ticket="+url.QueryEscape(ticket.Ticket), nil)
		replay.SetPathValue("musicId", "42")
		if _, accepted := fixture.service.authorize(replay); accepted {
			fixture.service.untrack(ticket.Room, fixture.claims.Username)
			t.Fatalf("ticket %q was replayable after a rejected query", ticket.Ticket)
		}
	}
}

func TestAuthorizeInvalidQueryConsumesEveryIdentifiableTicket(t *testing.T) {
	t.Run("duplicate parameter", func(t *testing.T) {
		fixture := setupContractService(t)
		ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
		if err != nil {
			t.Fatal(err)
		}
		encoded := url.QueryEscape(ticket.Ticket)
		requireTicketQueryRejectedAndConsumed(t, fixture, "ticket="+encoded+"&ticket="+encoded, ticket)
	})

	t.Run("multiple distinct tickets", func(t *testing.T) {
		fixture := setupContractService(t)
		first, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
		if err != nil {
			t.Fatal(err)
		}
		second, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
		if err != nil {
			t.Fatal(err)
		}
		requireTicketQueryRejectedAndConsumed(t, fixture,
			"ticket="+url.QueryEscape(first.Ticket)+"&ticket="+url.QueryEscape(second.Ticket), first, second)
	})

	t.Run("additional parameter", func(t *testing.T) {
		fixture := setupContractService(t)
		ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
		if err != nil {
			t.Fatal(err)
		}
		requireTicketQueryRejectedAndConsumed(t, fixture,
			"ticket="+url.QueryEscape(ticket.Ticket)+"&unexpected=1", ticket)
	})

	t.Run("malformed additional parameter", func(t *testing.T) {
		fixture := setupContractService(t)
		ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
		if err != nil {
			t.Fatal(err)
		}
		requireTicketQueryRejectedAndConsumed(t, fixture,
			"ticket="+url.QueryEscape(ticket.Ticket)+"&unexpected=%zz", ticket)
	})
}

func TestAuthorizeConsumesExpiredTicket(t *testing.T) {
	fixture := setupContractService(t)
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.ticketsMu.Lock()
	grant := fixture.service.tickets[ticket.Ticket]
	grant.expiresAt = time.Now().Add(-time.Second)
	fixture.service.tickets[ticket.Ticket] = grant
	fixture.service.ticketsMu.Unlock()
	requireTicketQueryRejectedAndConsumed(t, fixture, "ticket="+url.QueryEscape(ticket.Ticket), ticket)
}

func TestAuthorizeRejectsTicketFromRetiredEpoch(t *testing.T) {
	fixture := setupContractService(t)
	ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.persistence.db.Exec(`UPDATE lyrics_collab_documents SET epoch=epoch+1 WHERE music_id=42`); err != nil {
		t.Fatal(err)
	}
	requireTicketQueryRejectedAndConsumed(t, fixture, "ticket="+url.QueryEscape(ticket.Ticket), ticket)
}

func TestAuthorizeRejectsRevokedTicketIdentity(t *testing.T) {
	tests := []struct {
		name   string
		revoke func(contractServiceFixture) error
	}{
		{
			name: "role changed",
			revoke: func(fixture contractServiceFixture) error {
				return fixture.service.auth.SetRole(fixture.claims.Username, auth.RoleAdmin)
			},
		},
		{
			name: "token version changed",
			revoke: func(fixture contractServiceFixture) error {
				return fixture.service.auth.SetPassword(fixture.claims.Username, "replacement-password-456")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := setupContractService(t)
			ticket, err := fixture.service.IssueTicket(t.Context(), fixture.claims, fixture.bearer, 42, fixture.service.gate.Status())
			if err != nil {
				t.Fatal(err)
			}
			if err := test.revoke(fixture); err != nil {
				t.Fatal(err)
			}
			requireTicketQueryRejectedAndConsumed(t, fixture, "ticket="+url.QueryEscape(ticket.Ticket), ticket)
		})
	}
}

func TestIssueTicketRejectsCrossUserBearer(t *testing.T) {
	fixture := setupContractService(t)
	other, err := fixture.service.auth.CreateUser("other-editor", "strong-password-456", auth.RoleEditor)
	if err != nil {
		t.Fatal(err)
	}
	otherBearer, _, err := fixture.service.auth.IssueToken(other)
	if err != nil {
		t.Fatal(err)
	}
	otherClaims, err := fixture.service.auth.VerifyToken(otherBearer)
	if err != nil {
		t.Fatal(err)
	}
	status := fixture.service.gate.Status()
	if _, err := fixture.service.IssueTicket(t.Context(), fixture.claims, otherBearer, 42, status); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("claims A with bearer B error=%v", err)
	}
	if _, err := fixture.service.IssueTicket(t.Context(), otherClaims, fixture.bearer, 42, status); !errors.Is(err, ErrTicketInvalid) {
		t.Fatalf("claims B with bearer A error=%v", err)
	}
}
