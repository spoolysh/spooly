package db

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/pressly/goose/v3"

	"github.com/boxysh/boxy/internal/db/migrations"
)

type User struct {
	ID        string
	GoogleID  string
	Email     string
	Name      string
	CreatedAt time.Time
}

type APIKey struct {
	ID        string
	UserID    string
	KeyHash   string
	KeyPrefix string
	Label     string
	CreatedAt time.Time
}

// NewID returns a new message ID: a nanoid of 21 random characters, as
// stashy's file IDs. Users, API keys, and accounts are numbered by the
// database instead.
func NewID() string {
	return gonanoid.Must()
}

type DB struct {
	sql     *sql.DB
	dialect string
}

func New(ctx context.Context, driver, dsn string) (*DB, error) {
	sqlDB, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	dialect := driver
	if strings.Contains(driver, "sqlite") {
		dialect = "sqlite3"
		// SQLite allows one writer at a time; a single connection queues
		// writes in Go instead of failing them with SQLITE_BUSY.
		sqlDB.SetMaxOpenConns(1)
	}

	return &DB{sql: sqlDB, dialect: dialect}, nil
}

// Migrate runs all pending database migrations for the connected dialect.
func (d *DB) Migrate(ctx context.Context) error {
	dir := "sqlite"
	if d.dialect == "pgx" {
		dir = "postgres"
	}

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect(d.dialect); err != nil {
		return fmt.Errorf("setting goose dialect: %w", err)
	}
	if err := goose.UpContext(ctx, d.sql, dir); err != nil {
		return fmt.Errorf("running migrations: %w", err)
	}
	return nil
}

func (d *DB) Close(_ context.Context) error {
	return d.sql.Close()
}

func (d *DB) UpsertUser(ctx context.Context, googleID, email, name string) (*User, error) {
	query := d.q(`INSERT INTO users (google_id, email, name, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(google_id) DO UPDATE SET email = excluded.email, name = excluded.name
		RETURNING id, google_id, email, name, created_at`)
	var user User
	err := d.sql.QueryRowContext(ctx, query, googleID, email, name, time.Now().UTC()).
		Scan(&user.ID, &user.GoogleID, &user.Email, &user.Name, &user.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("upserting user: %w", err)
	}
	return &user, nil
}

func (d *DB) GetUserByID(ctx context.Context, id string) (*User, error) {
	query := d.q(`SELECT id, google_id, email, name, created_at FROM users WHERE id = ?`)

	var user User
	err := d.sql.QueryRowContext(ctx, query, id).
		Scan(&user.ID, &user.GoogleID, &user.Email, &user.Name, &user.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: %s", ErrUserNotFound, id)
		}
		return nil, fmt.Errorf("getting user: %w", err)
	}
	return &user, nil
}

func (d *DB) CreateAPIKey(ctx context.Context, userID, label string) (string, *APIKey, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generating key: %w", err)
	}

	plaintext := base64.URLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(plaintext))
	keyHash := base64.URLEncoding.EncodeToString(hash[:])
	keyPrefix := plaintext[:8]

	var id string
	var createdAt time.Time
	query := d.q(`INSERT INTO api_keys (user_id, key_hash, key_prefix, label, created_at)
		VALUES (?, ?, ?, ?, ?) RETURNING id, created_at`)
	err := d.sql.QueryRowContext(ctx, query, userID, keyHash, keyPrefix, label, time.Now().UTC()).Scan(&id, &createdAt)
	if err != nil {
		return "", nil, fmt.Errorf("inserting api key: %w", err)
	}

	key := &APIKey{
		ID:        id,
		UserID:    userID,
		KeyHash:   keyHash,
		KeyPrefix: keyPrefix,
		Label:     label,
		CreatedAt: createdAt,
	}
	return plaintext, key, nil
}

func (d *DB) LookupAPIKey(ctx context.Context, plaintext string) (*APIKey, error) {
	hash := sha256.Sum256([]byte(plaintext))
	keyHash := base64.URLEncoding.EncodeToString(hash[:])

	query := d.q(`SELECT id, user_id, key_hash, key_prefix, label, created_at
		FROM api_keys WHERE key_hash = ?`)

	var key APIKey
	err := d.sql.QueryRowContext(ctx, query, keyHash).
		Scan(&key.ID, &key.UserID, &key.KeyHash, &key.KeyPrefix, &key.Label, &key.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("api key not found")
		}
		return nil, fmt.Errorf("looking up api key: %w", err)
	}
	return &key, nil
}

func (d *DB) ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error) {
	query := d.q(`SELECT id, user_id, key_hash, key_prefix, label, created_at
		FROM api_keys WHERE user_id = ? ORDER BY created_at DESC`)

	rows, err := d.sql.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, fmt.Errorf("listing api keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.UserID, &k.KeyHash, &k.KeyPrefix, &k.Label, &k.CreatedAt); err != nil {
			return nil, fmt.Errorf("scanning api key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (d *DB) DeleteAPIKey(ctx context.Context, keyID, userID string) error {
	query := d.q(`DELETE FROM api_keys WHERE id = ? AND user_id = ?`)

	result, err := d.sql.ExecContext(ctx, query, keyID, userID)
	if err != nil {
		return fmt.Errorf("deleting api key: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("api key not found")
	}
	return nil
}

// CountAPIKeys returns how many API keys each user has, by user ID.
func (d *DB) CountAPIKeys(ctx context.Context) (map[string]int, error) {
	rows, err := d.sql.QueryContext(ctx, `SELECT user_id, COUNT(*) FROM api_keys GROUP BY user_id`)
	if err != nil {
		return nil, fmt.Errorf("counting api keys: %w", err)
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var userID string
		var n int
		if err := rows.Scan(&userID, &n); err != nil {
			return nil, fmt.Errorf("scanning api key count: %w", err)
		}
		counts[userID] = n
	}
	return counts, rows.Err()
}

// DeleteUserAPIKeys deletes all of a user's API keys and returns how many
// there were.
func (d *DB) DeleteUserAPIKeys(ctx context.Context, userID string) (int, error) {
	result, err := d.sql.ExecContext(ctx, d.q(`DELETE FROM api_keys WHERE user_id = ?`), userID)
	if err != nil {
		return 0, fmt.Errorf("deleting api keys: %w", err)
	}
	n, _ := result.RowsAffected()
	return int(n), nil
}

// q rewrites ? placeholders to $1, $2, ... for postgres. SQLite uses ? natively.
func (d *DB) q(query string) string {
	if d.dialect != "pgx" {
		return query
	}
	var b strings.Builder
	n := 1
	for _, c := range query {
		if c == '?' {
			fmt.Fprintf(&b, "$%d", n)
			n++
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// placeholders returns n comma-separated ? placeholders, for IN lists.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}
