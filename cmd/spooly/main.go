package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	gcsapi "google.golang.org/api/storage/v1"
	_ "modernc.org/sqlite"

	"github.com/spoolysh/spooly/internal/auth"
	"github.com/spoolysh/spooly/internal/db"
	"github.com/spoolysh/spooly/internal/delivery"
	"github.com/spoolysh/spooly/internal/delivery/cloudflare"
	"github.com/spoolysh/spooly/internal/delivery/smtp"
	"github.com/spoolysh/spooly/internal/service"
	"github.com/spoolysh/spooly/internal/storage"
	"github.com/spoolysh/spooly/internal/storage/gcs"
	"github.com/spoolysh/spooly/internal/storage/local"
	"github.com/spoolysh/spooly/internal/storage/memory"
	s3storage "github.com/spoolysh/spooly/internal/storage/s3"
	"github.com/spoolysh/spooly/internal/web"
)

var Version = "dev"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envRequired(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("%s is required", key)
	}
	return v
}

// newStorage returns the backend for raw messages. The default is local
// disk: memory loses mail on restart, so it's only for trying Spooly out.
func newStorage() (storage.Storage, error) {
	switch backend := env("STORAGE_BACKEND", "local"); backend {
	case "gcs":
		service, err := gcsapi.NewService(context.Background())
		if err != nil {
			return nil, err
		}
		return gcs.New(service, envRequired("GCS_BUCKET")), nil
	case "s3":
		cfg, err := config.LoadDefaultConfig(context.Background())
		if err != nil {
			return nil, fmt.Errorf("loading AWS config: %w", err)
		}
		client := s3.NewFromConfig(cfg)
		return s3storage.New(client, envRequired("S3_BUCKET")), nil
	case "local":
		return local.New(env("LOCAL_STORAGE_DIR", "./storage"))
	case "memory":
		return memory.New(), nil
	default:
		return nil, fmt.Errorf("unknown STORAGE_BACKEND %q", backend)
	}
}

func driverFromDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "pgx"
	default:
		return "sqlite"
	}
}

func databaseURL() string {
	return env("DATABASE_URL", "file:spooly.db")
}

func openDB() (*db.DB, error) {
	dsn := databaseURL()
	return db.New(context.Background(), driverFromDSN(dsn), dsn)
}

var usage = "Usage: spooly " + Version + ` <command>

Commands:
  serve [--migrate]   Start the API and dashboard (default)
  migrate             Run database migrations and exit

Mail clients (POP3, IMAP) connect through the bridge, which calls the API.
`

func main() {
	godotenv.Load()

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	migrate := false
	for _, arg := range os.Args[1:] {
		if arg == "--migrate" {
			migrate = true
		}
	}

	switch cmd {
	case "serve":
		cmdServe(migrate)
	case "migrate":
		cmdMigrate()
	case "version", "-v", "--version":
		fmt.Println("spooly " + Version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

func cmdMigrate() {
	database, err := openDB()
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer database.Close(context.Background())

	if err := database.Migrate(context.Background()); err != nil {
		log.Fatalf("migration failed: %v", err)
	}
	log.Println("migrations complete")
}

// newService builds the service, with the admins from ADMIN_EMAILS: a
// comma-separated list of the emails they sign in with.
func newService(database *db.DB, store storage.Storage, transport delivery.Transport) *service.Service {
	var admins []string
	if v := os.Getenv("ADMIN_EMAILS"); v != "" {
		admins = strings.Split(v, ",")
	}
	if len(admins) == 0 {
		log.Printf("ADMIN_EMAILS is empty: no one can create mailboxes")
	}
	expungeAfter, eventsRetention := duration("EXPUNGE_AFTER", "168h"), duration("EVENTS_RETENTION", "1080h")
	if eventsRetention != 0 && expungeAfter > eventsRetention {
		log.Fatalf("EVENTS_RETENTION (%s) must be at least EXPUNGE_AFTER (%s), or 0 to keep events forever", eventsRetention, expungeAfter)
	}
	saveToSent, err := strconv.ParseBool(env("SAVE_TO_SENT", "true"))
	if err != nil {
		log.Fatalf("SAVE_TO_SENT: %q isn't true or false", os.Getenv("SAVE_TO_SENT"))
	}
	return service.New(database, store, service.Options{
		Admins: admins, ExpungeAfter: expungeAfter, EventsRetention: eventsRetention,
		Transport: transport, NoSentCopy: !saveToSent,
	})
}

// newTransport returns the mail transport MAIL_TRANSPORT names, or nil to
// turn sending off. hostname's host is the name it greets relays with.
func newTransport(hostname string) delivery.Transport {
	hello := "localhost"
	if u, err := url.Parse(hostname); err == nil && u.Hostname() != "" {
		hello = u.Hostname()
	}
	switch name := os.Getenv("MAIL_TRANSPORT"); name {
	case "":
		log.Printf("MAIL_TRANSPORT is empty: sending is off")
		return nil
	case "smtp":
		port, err := strconv.Atoi(env("SMTP_PORT", "587"))
		if err != nil {
			log.Fatalf("SMTP_PORT: %v", err)
		}
		host := envRequired("SMTP_HOST")
		if smtp.IsLocal(host) {
			log.Printf("SMTP_HOST %s is this machine: mail may go to it in the clear", host)
		}
		return smtp.New(smtp.Options{
			Host: host, Port: port, ImplicitTLS: port == 465, Plaintext: smtp.IsLocal(host),
			Username: os.Getenv("SMTP_USERNAME"), Password: os.Getenv("SMTP_PASSWORD"), Hello: hello,
		})
	case "cloudflare":
		return cloudflare.New(envRequired("CLOUDFLARE_API_TOKEN"), hello)
	default:
		log.Fatalf("unknown MAIL_TRANSPORT %q: use smtp or cloudflare", name)
		return nil
	}
}

// duration reads an environment variable as a duration, such as 720h.
func duration(key, fallback string) time.Duration {
	v := env(key, fallback)
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		log.Fatalf("%s: %q isn't a duration such as %s", key, v, fallback)
	}
	return d
}

// openBackend opens the database, migrating it if asked, and raw message
// storage.
func openBackend(migrate bool) (*db.DB, storage.Storage) {
	database, err := openDB()
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}

	if migrate {
		if err := database.Migrate(context.Background()); err != nil {
			log.Fatalf("migration failed: %v", err)
		}
	}

	store, err := newStorage()
	if err != nil {
		log.Fatalf("failed to create storage: %v", err)
	}
	return database, store
}

// prune expunges deleted messages and prunes old changes, now and then
// hourly.
func prune(svc *service.Service) {
	for {
		if err := svc.Prune(context.Background(), time.Now()); err != nil {
			log.Printf("pruning: %v", err)
		}
		time.Sleep(time.Hour)
	}
}

// cmdServe serves the API and dashboard.
func cmdServe(migrate bool) {
	port := env("PORT", "8080")

	database, store := openBackend(migrate)
	defer database.Close(context.Background())

	sessions := auth.NewSessionManager(envRequired("SESSION_SECRET"))
	hostname := env("HOSTNAME", "http://localhost:"+port)

	var allowedDomains []string
	if v := os.Getenv("ALLOWED_DOMAINS"); v != "" {
		allowedDomains = strings.Split(v, ",")
	}

	oauth := auth.NewOAuthHandler(
		envRequired("GOOGLE_CLIENT_ID"),
		envRequired("GOOGLE_CLIENT_SECRET"),
		hostname+"/auth/google/callback",
		database,
		sessions,
		allowedDomains,
	)

	svc := newService(database, store, newTransport(hostname))
	go prune(svc)

	webUI := web.NewHandler(database, sessions, svc, hostname, Version)

	mux := http.NewServeMux()

	oauth.RegisterRoutes(mux)
	webUI.RegisterRoutes(mux)

	if err := svc.Mount(mux); err != nil {
		log.Fatalf("failed to mount API: %v", err)
	}

	publicFS := http.FileServer(http.Dir("public"))
	if spec, err := os.ReadFile("public/openapi.yaml"); err == nil {
		spec = bytes.ReplaceAll(spec, []byte("https://spooly.example.com"), []byte(hostname))
		mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
			w.Write(spec)
		})
	}
	if entries, err := os.ReadDir("public"); err == nil {
		for _, e := range entries {
			if !e.IsDir() && !strings.HasPrefix(e.Name(), ".") && e.Name() != "openapi.yaml" {
				mux.Handle("GET /"+e.Name(), publicFS)
			}
		}
	}

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := ":" + port
	log.Printf("listening on %s", addr)

	// Serve HTTP/2 without TLS (h2c) alongside HTTP/1.1, for gRPC clients and
	// proxies such as Cloud Run that talk HTTP/2 to the container.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	server := &http.Server{Addr: addr, Handler: mux, Protocols: &protocols}
	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
