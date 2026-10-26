package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"

	gcstorage "cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"

	"github.com/boxysh/boxy/internal/auth"
	"github.com/boxysh/boxy/internal/db"
	"github.com/boxysh/boxy/internal/pop3"
	"github.com/boxysh/boxy/internal/service"
	"github.com/boxysh/boxy/internal/storage"
	"github.com/boxysh/boxy/internal/storage/gcs"
	"github.com/boxysh/boxy/internal/storage/local"
	"github.com/boxysh/boxy/internal/storage/memory"
	s3storage "github.com/boxysh/boxy/internal/storage/s3"
	"github.com/boxysh/boxy/internal/web"
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
// disk: memory loses mail on restart, so it's only for trying Boxy out.
func newStorage() (storage.Storage, error) {
	switch backend := env("STORAGE_BACKEND", "local"); backend {
	case "gcs":
		client, err := gcstorage.NewClient(context.Background())
		if err != nil {
			return nil, err
		}
		return gcs.New(client, envRequired("GCS_BUCKET")), nil
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

// mailTLS loads MAIL_TLS_CERT and MAIL_TLS_KEY, the certificate every mail
// protocol uses. It returns nil if they aren't set.
func mailTLS() (*tls.Config, error) {
	cert, key := os.Getenv("MAIL_TLS_CERT"), os.Getenv("MAIL_TLS_KEY")
	if cert == "" && key == "" {
		return nil, nil
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("loading MAIL_TLS_CERT and MAIL_TLS_KEY: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

// startMail starts the mail protocols, for serve --mail and mail, and exits
// if none is enabled. POP3 listens on POP3_ADDR (default :1110; empty
// disables it), and with TLS also on POP3S_ADDR, if set. With a certificate,
// the plain port offers STLS and logins need TLS; without one, logins are
// plaintext.
func startMail(database *db.DB, svc *service.Service) {
	started, err := startProtocols(database, svc)
	if err != nil {
		log.Fatalf("failed to start mail protocols: %v", err)
	}
	if !started {
		log.Fatal("no mail protocol is enabled: set POP3_ADDR or POP3S_ADDR")
	}
}

// startProtocols starts the mail protocols that are configured, and reports
// whether any is.
func startProtocols(database *db.DB, svc *service.Service) (bool, error) {
	tlsConfig, err := mailTLS()
	if err != nil {
		return false, err
	}

	addr, ok := os.LookupEnv("POP3_ADDR")
	if !ok {
		addr = ":1110"
	}
	tlsAddr := os.Getenv("POP3S_ADDR")
	if addr == "" && tlsAddr == "" {
		return false, nil
	}
	if tlsAddr != "" && tlsConfig == nil {
		return false, fmt.Errorf("POP3S_ADDR needs MAIL_TLS_CERT and MAIL_TLS_KEY")
	}

	opts := pop3.Options{TLS: tlsConfig, Delete: os.Getenv("POP3_DELETE")}
	if v := os.Getenv("POP3_MARK_READ"); v != "" {
		markRead, err := strconv.ParseBool(v)
		if err != nil {
			return false, fmt.Errorf("POP3_MARK_READ: %w", err)
		}
		opts.KeepUnread = !markRead
	}
	if v := os.Getenv("POP3_MAX_MESSAGES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return false, fmt.Errorf("POP3_MAX_MESSAGES: %w", err)
		}
		opts.MaxMessages = n
	}
	server, err := pop3.New(database, svc, opts)
	if err != nil {
		return false, err
	}
	listen := func(addr string, secure bool) error {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("POP3: %w", err)
		}
		if secure {
			l = tls.NewListener(l, tlsConfig)
		}
		go func() {
			if err := server.Serve(l); err != nil {
				log.Fatalf("POP3 server error: %v", err)
			}
		}()
		return nil
	}
	if addr != "" {
		if err := listen(addr, false); err != nil {
			return false, err
		}
		if tlsConfig == nil {
			log.Printf("POP3 listening on %s without TLS: passwords are sent in plaintext", addr)
		} else {
			log.Printf("POP3 listening on %s (STLS)", addr)
		}
	}
	if tlsAddr != "" {
		if err := listen(tlsAddr, true); err != nil {
			return false, err
		}
		log.Printf("POP3S listening on %s", tlsAddr)
	}
	return true, nil
}

func driverFromDSN(dsn string) string {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return "pgx"
	default:
		return "sqlite"
	}
}

func openDB() (*db.DB, error) {
	dsn := env("DATABASE_URL", "file:boxy.db")
	return db.New(context.Background(), driverFromDSN(dsn), dsn)
}

var usage = "Usage: boxy " + Version + ` <command>

Commands:
  serve [--migrate] [--mail]   Start the API and dashboard (default);
                               --mail also starts the mail protocols
  mail [--migrate]             Start only the mail protocols: POP3
  migrate                      Run database migrations and exit
`

func main() {
	godotenv.Load()

	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	migrate, mail := false, false
	for _, arg := range os.Args[1:] {
		switch arg {
		case "--migrate":
			migrate = true
		case "--mail":
			mail = true
		}
	}

	switch cmd {
	case "serve":
		cmdServe(migrate, mail)
	case "mail":
		cmdMail(migrate)
	case "migrate":
		cmdMigrate()
	case "version", "-v", "--version":
		fmt.Println("boxy " + Version)
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

// cmdMail serves only the mail-client protocols, for hosts that take raw TCP
// when the API runs where only HTTP gets through, such as Cloud Run. Both
// must share a PostgreSQL database and S3 or GCS storage.
func cmdMail(migrate bool) {
	database, store := openBackend(migrate)
	defer database.Close(context.Background())

	startMail(database, service.New(database, store))
	select {}
}

// cmdServe serves the API and dashboard, and with mail, the mail protocols
// too, for a single server or local development.
func cmdServe(migrate, mail bool) {
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

	svc := service.New(database, store)

	if mail {
		startMail(database, svc)
	}

	webUI := web.NewHandler(database, sessions, svc, hostname, Version)

	mux := http.NewServeMux()

	oauth.RegisterRoutes(mux)
	webUI.RegisterRoutes(mux)

	if err := svc.Mount(mux); err != nil {
		log.Fatalf("failed to mount API: %v", err)
	}

	publicFS := http.FileServer(http.Dir("public"))
	if spec, err := os.ReadFile("public/openapi.yaml"); err == nil {
		spec = bytes.ReplaceAll(spec, []byte("https://boxy.example.com"), []byte(hostname))
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
