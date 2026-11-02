// Package spoolytest runs Spooly's API in-process for other modules' tests,
// such as the gateway's: SQLite in a temporary directory, and raw messages
// in memory.
package spoolytest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/service"
	"github.com/spoolysh/spooly/internal/storage/memory"
)

// AdminEmail is the email of the admin whose key Server.Key is.
const AdminEmail = "admin@example.com"

// Server is Spooly's API, served over HTTP until the test ends.
type Server struct {
	URL string
	// Key is an admin's API key: it can create mailboxes and labels, and
	// open every mailbox.
	Key string
	db  *db.DB
}

// New starts a server for t.
func New(t testing.TB) *Server {
	t.Helper()
	ctx := t.Context()
	database, err := db.New(ctx, "sqlite", "file:"+t.TempDir()+"/spooly.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close(context.Background()) })
	if err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	svc := service.New(database, memory.New(), service.Options{Admins: []string{AdminEmail}})
	mux := http.NewServeMux()
	if err := svc.Mount(mux); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	s := &Server{URL: srv.URL, db: database}
	s.Key = s.NewKey(t, AdminEmail)
	return s
}

// NewKey returns an API key of the user with email, adding the user if
// it's new. Only AdminEmail is an admin; others open only the mailboxes
// shared with them.
func (s *Server) NewKey(t testing.TB, email string) string {
	t.Helper()
	user, err := s.db.UpsertUser(t.Context(), "google-"+email, email, email)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := s.db.CreateAPIKey(t.Context(), user.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// Share gives the user with email access to the mailbox with address, as
// the dashboard does.
func (s *Server) Share(t testing.TB, address, email string) {
	t.Helper()
	user, err := s.db.UpsertUser(t.Context(), "google-"+email, email, email)
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.db.ResolveMailbox(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.db.AddUser(t.Context(), m.ID, user.ID); err != nil {
		t.Fatal(err)
	}
}
