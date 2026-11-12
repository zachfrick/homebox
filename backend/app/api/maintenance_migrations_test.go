package main

import (
	"context"
	"database/sql"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/require"
	"github.com/sysadminsmedia/homebox/backend/internal/data/ent"
	"github.com/sysadminsmedia/homebox/backend/internal/data/migrations"
	_ "github.com/sysadminsmedia/homebox/backend/pkgs/cgofreesqlite"
)

// Build the pre-feature database using every main migration, then upgrade it
// through the application's real migration entry point (without AllowMissing).
func TestMaintenancePlansUpgradeFromMain(t *testing.T) {
	t.Run("sqlite3", func(t *testing.T) { testMaintenanceUpgrade(t, "sqlite3") })
	t.Run("postgres", func(t *testing.T) { testMaintenanceUpgrade(t, "postgres") })
}

func testMaintenanceUpgrade(t *testing.T, driver string) {
	var client *ent.Client
	var err error
	if driver == "sqlite3" {
		client, err = ent.Open("sqlite3", filepath.Join(t.TempDir(), "upgrade.db")+"?_fk=1&_time_format=sqlite")
	} else {
		dsn := os.Getenv("TEST_MAINTENANCE_POSTGRES_URL")
		if dsn == "" {
			t.Skip("set TEST_MAINTENANCE_POSTGRES_URL to test PostgreSQL upgrade")
		}
		admin, openErr := sql.Open("pgx", dsn)
		require.NoError(t, openErr)
		defer func() { _ = admin.Close() }()
		name := "maintenance_upgrade_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		_, err = admin.ExecContext(context.Background(), "CREATE DATABASE "+name)
		require.NoError(t, err)
		defer func() { _, _ = admin.ExecContext(context.Background(), "DROP DATABASE "+name) }()
		u, parseErr := url.Parse(dsn)
		require.NoError(t, parseErr)
		u.Path = "/" + name
		db, openErr := sql.Open("pgx", u.String())
		require.NoError(t, openErr)
		client = ent.NewClient(ent.Driver(entsql.OpenDB("postgres", db)))
	}
	require.NoError(t, err)
	defer func() { _ = client.Close() }()
	embedded, err := migrations.Migrations(driver)
	require.NoError(t, err)
	before := fstest.MapFS{}
	require.NoError(t, fs.WalkDir(embedded, driver, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.Contains(path, "add_maintenance_plans") {
			return nil
		}
		data, err := fs.ReadFile(embedded, path)
		if err != nil {
			return err
		}
		before[path] = &fstest.MapFile{Data: data}
		return nil
	}))
	goose.SetBaseFS(before)
	require.NoError(t, goose.SetDialect(driver))
	require.NoError(t, goose.Up(client.Sql(), driver))
	require.NoError(t, runMigrations(client, driver))
	_, err = client.MaintenancePlan.Query().Count(context.Background())
	require.NoError(t, err)
	_, err = client.MaintenanceEntry.Query().Count(context.Background())
	require.NoError(t, err)
}
