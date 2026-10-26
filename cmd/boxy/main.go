package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	gcstorage "cloud.google.com/go/storage"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/joho/godotenv"
	_ "modernc.org/sqlite"

	"github.com/boxysh/boxy/internal/auth"
	"github.com/boxysh/boxy/internal/db"
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
  serve [--migrate]   Start the server (default)
  migrate             Run database migrations and exit
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

func cmdServe(migrate bool) {
	port := env("PORT", "8080")

	database, err := openDB()
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer database.Close(context.Background())

	if migrate {
		if err := database.Migrate(context.Background()); err != nil {
			log.Fatalf("migration failed: %v", err)
		}
	}

	store, err := newStorage()
	if err != nil {
		log.Fatalf("failed to create storage: %v", err)
	}

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
