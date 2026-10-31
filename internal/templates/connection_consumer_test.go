package templates

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

const lullmailDSN = "postgres://lull@lullmail-db:5432/lullmail?sslmode=disable"

// The shipped Lullmail DSN deliberately delegates password handling to pgx's
// environment support; URI delimiters must remain literal password bytes.
func TestLullmailPasswordConsumer(t *testing.T) {
	t.Run("pgx consumer", func(t *testing.T) {
		for _, password := range []string{"abc#/?%@", "quote'\"colon: slash\\", "true", "generate", "auto", "$UNSET_CATALOG_FIXTURE", "secret:literal", "line\nbreak"} {
			t.Setenv("PGPASSWORD", password)
			cfg, err := pgx.ParseConfig(lullmailDSN)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Password != password || cfg.Host != "lullmail-db" || cfg.User != "lull" || cfg.Database != "lullmail" {
				t.Fatalf("connection changed: host=%s user=%s database=%s", cfg.Host, cfg.User, cfg.Database)
			}
		}
	})

	t.Run("shipped template", func(t *testing.T) {
		// The catalog is a separate repository (useteploy/templates). CI
		// checks it out and sets TEPLOY_TEMPLATES_DIR; a maintainer checkout
		// has it as the sibling ../templates. When neither is present (a
		// single-repo sandbox) there is nothing to check against.
		root := os.Getenv("TEPLOY_TEMPLATES_DIR")
		explicit := root != ""
		if !explicit {
			root = "../../../templates"
		}
		raw, err := os.ReadFile(filepath.Join(root, "lullmail", "teploy.yml"))
		if err != nil {
			if !explicit && errors.Is(err, fs.ErrNotExist) {
				t.Skip("templates catalog not available: set TEPLOY_TEMPLATES_DIR to a useteploy/templates checkout (CI does) or clone it as the sibling ../templates")
			}
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "DATABASE_URL: "+lullmailDSN) || !strings.Contains(string(raw), "PGPASSWORD: {{db_password}}") {
			t.Fatal("template no longer uses separate pgx password input")
		}
	})
}
