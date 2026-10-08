package templates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The shipped Lullmail DSN deliberately delegates password handling to pgx's
// environment support; URI delimiters must remain literal password bytes.
func TestLullmailPasswordConsumer(t *testing.T) {
	root := os.Getenv("TEPLOY_TEMPLATES_DIR")
	if root == "" {
		root = "../../../templates"
	}
	raw, err := os.ReadFile(filepath.Join(root, "lullmail", "teploy.yml"))
	if err != nil {
		t.Fatal(err)
	}
	const dsn = "postgres://lull@lullmail-db:5432/lullmail?sslmode=disable"
	if !strings.Contains(string(raw), "DATABASE_URL: "+dsn) || !strings.Contains(string(raw), "PGPASSWORD: {{db_password}}") {
		t.Fatal("template no longer uses separate pgx password input")
	}
	for _, password := range []string{"abc#/?%@", "quote'\"colon: slash\\", "true", "generate", "auto", "$UNSET_CATALOG_FIXTURE", "secret:literal", "line\nbreak"} {
		t.Setenv("PGPASSWORD", password)
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Password != password || cfg.Host != "lullmail-db" || cfg.User != "lull" || cfg.Database != "lullmail" {
			t.Fatalf("connection changed: host=%s user=%s database=%s", cfg.Host, cfg.User, cfg.Database)
		}
	}
}
