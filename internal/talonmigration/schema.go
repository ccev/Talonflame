package talonmigration

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"

	migrations "github.com/UnownHash/Fletchling/migrations/talonflame"
)

type Options struct {
	Mode      string
	Apply     bool
	BatchSize int
}

func Run(ctx context.Context, conn *sql.Conn, logger *logrus.Logger, opts Options) error {
	if opts.Mode != "all" && opts.Mode != "schema" && opts.Mode != "backfill" {
		return fmt.Errorf("invalid mode %q", opts.Mode)
	}
	if opts.BatchSize < 1 || opts.BatchSize > 10000 {
		return fmt.Errorf("batch size must be between 1 and 10000")
	}
	if err := preflight(ctx, conn); err != nil {
		return err
	}
	logger.WithFields(logrus.Fields{"mode": opts.Mode, "apply": opts.Apply}).Info("database preparation started")
	if opts.Apply {
		var locked sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(CONCAT(DATABASE(), ':talonflame-writer'), 0)").Scan(&locked); err != nil {
			return err
		}
		if !locked.Valid || locked.Int64 != 1 {
			return fmt.Errorf("another Talonflame analyzer or migration/backfill is running")
		}
		defer func() {
			releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn.ExecContext(releaseCtx, "DO RELEASE_LOCK(CONCAT(DATABASE(), ':talonflame-writer'))")
		}()
	}
	if opts.Mode != "backfill" {
		if err := schema(ctx, conn, logger, opts.Apply); err != nil {
			return err
		}
	}
	if opts.Mode != "schema" {
		if err := backfill(ctx, conn, logger, opts); err != nil {
			return err
		}
	}
	logger.Info("database preparation completed")
	return nil
}

func preflight(ctx context.Context, conn *sql.Conn) error {
	// Never create or replace the source tables. This runner is for an existing
	// Golbat DB with Fletchling's imported polygons, not its old procedure setup.
	for _, table := range []string{"nests", "spawnpoint"} {
		var engine string
		err := conn.QueryRowContext(ctx, `SELECT ENGINE FROM information_schema.TABLES
			WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=?`, table).Scan(&engine)
		if err != nil {
			return fmt.Errorf("existing %s table required: %w", table, err)
		}
		if !strings.EqualFold(engine, "InnoDB") {
			return fmt.Errorf("%s must use InnoDB for atomic backfill publication", table)
		}
	}
	for _, query := range []string{
		"SELECT nest_id, ST_AsGeoJSON(polygon) FROM nests LIMIT 0",
		"SELECT id FROM spawnpoint LIMIT 0",
	} {
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("source schema: %w", err)
		}
		rows.Close()
	}
	return nil
}

func schema(ctx context.Context, conn *sql.Conn, logger *logrus.Logger, apply bool) error {
	var exists bool
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) > 0 FROM information_schema.TABLES
		WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='talonflame_schema_migrations'`).Scan(&exists); err != nil {
		return err
	}
	if apply && !exists {
		if _, err := conn.ExecContext(ctx, `CREATE TABLE talonflame_schema_migrations (
			version VARCHAR(128) NOT NULL PRIMARY KEY,
			sha256 CHAR(64) NOT NULL,
			applied_at BIGINT UNSIGNED NOT NULL
		) ENGINE=InnoDB`); err != nil {
			return err
		}
		exists = true
	}
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, err := migrations.Files.ReadFile(name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])
		if exists {
			var recorded string
			err := conn.QueryRowContext(ctx, "SELECT sha256 FROM talonflame_schema_migrations WHERE version=?", name).Scan(&recorded)
			if err == nil {
				if recorded != checksum {
					return fmt.Errorf("migration %s changed after application", name)
				}
				logger.WithField("migration", name).Debug("schema migration already applied")
				continue
			}
			if err != sql.ErrNoRows {
				return err
			}
		}
		logger.WithFields(logrus.Fields{"migration": name, "apply": apply}).Info("pending schema migration")
		if !apply {
			continue
		}
		// All files contain simple, idempotent MariaDB DDL. DDL autocommits,
		// so retries resume safely after a partially completed schema migration.
		for _, statement := range statements(string(body)) {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("migration %s: %w", name, err)
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO talonflame_schema_migrations (version,sha256,applied_at) VALUES (?,?,?)", name, checksum, time.Now().Unix()); err != nil {
			return err
		}
		logger.WithField("migration", name).Info("schema migration applied")
	}
	return nil
}

func statements(body string) []string {
	var clean strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			clean.WriteString(line)
			clean.WriteByte('\n')
		}
	}
	var result []string
	for _, statement := range strings.Split(clean.String(), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			result = append(result, statement)
		}
	}
	return result
}
