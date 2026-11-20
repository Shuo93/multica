package migrations

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestJiuwenSwarmRuntimeProfileMigration(t *testing.T) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("integration test requires Postgres at DATABASE_URL")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TEMP TABLE runtime_profile (protocol_family TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	apply := func(name string) {
		t.Helper()
		sql, err := os.ReadFile(filepath.Join("..", "..", "migrations", name+".sql"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatal(err)
		}
	}
	insert := func(provider string, accepted bool) {
		t.Helper()
		_, err := conn.Exec(ctx, "INSERT INTO runtime_profile VALUES ($1)", provider)
		if accepted {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("expected CHECK rejection for %q, got %v", provider, err)
			}
		}
	}
	apply("656_runtime_profile_add_amp.up")
	insert("jiuwenswarm", false)
	apply("711_runtime_profile_add_jiuwenswarm.up")
	insert("jiuwenswarm", true)
	insert("amp", true)
	insert("unknown", false)
	apply("711_runtime_profile_add_jiuwenswarm.down")
	insert("jiuwenswarm", false)
	insert("amp", true)
	var count int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM runtime_profile WHERE protocol_family = 'jiuwenswarm'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("historical profile count = %d, err = %v", count, err)
	}
}
