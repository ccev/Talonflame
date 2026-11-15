package talonflame

import (
	"context"
	"database/sql"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/UnownHash/Fletchling/internal/talonmigration"
	"github.com/go-sql-driver/mysql"
	"github.com/golang/geo/s2"
	"github.com/sirupsen/logrus"
)

func TestMariaDBSpawnpointProjection(t *testing.T) {
	dsn := os.Getenv("TALONFLAME_PROJECTION_TEST_DSN")
	if dsn == "" {
		t.Skip("set TALONFLAME_PROJECTION_TEST_DSN to a disposable database")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "talonflame_test_") {
		t.Fatal("disposable test database required")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	ctx := context.Background()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	exec(`CREATE TABLE nests (nest_id BIGINT PRIMARY KEY,polygon GEOMETRY NOT NULL) ENGINE=InnoDB`)
	exec(`CREATE TABLE spawnpoint (id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB`)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := talonmigration.Run(ctx, conn, logger, talonmigration.Options{Mode: "schema", Apply: true, BatchSize: 256}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	exec(`INSERT INTO nests (nest_id,polygon) VALUES (10,ST_GeomFromText('POLYGON((9.9 53.4,10.1 53.4,10.1 53.7,9.9 53.7,9.9 53.4))'))`)
	exec(`INSERT INTO talonflame_nest_groups (nest_id) VALUES (10)`)
	exec(`INSERT INTO talonflame_nest_sources VALUES (10,10)`)
	a, err := New(ctx, db, logger, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	point := func(lon float64) uint64 {
		return uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(53.55, lon)).Parent(20)) >> 20
	}
	late, rejected, ready := point(10), point(10.002), point(10.004)
	exec("INSERT INTO spawnpoint (id) VALUES (?)", ready)
	for _, id := range []uint64{late, rejected, ready} {
		if _, err := a.point(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending") != 2 {
		t.Fatal("only missing spawnpoints should be queued")
	}
	// Repeated changes must coalesce, and the retry must publish the latest
	// assignment (including rejection to NULL), not the original import.
	if err := a.assign(ctx, late, -10, "learned", "alternate", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.assign(ctx, late, -20, "learned", "new alternate", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.assign(ctx, rejected, 0, "rejected", "contradictions", nil); err != nil {
		t.Fatal(err)
	}
	if err := a.Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending") != 2 {
		t.Fatal("maintenance discarded missing rows")
	}
	// State and retry markers are committed or rolled back together.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := projectSpawnpoint(ctx, tx, point(10.006), 10); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending") != 2 {
		t.Fatal("retry marker escaped rollback")
	}
	a.Close()
	restored, err := New(ctx, db, logger, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	a = restored
	exec("INSERT INTO spawnpoint (id) VALUES (?)", late)
	exec("INSERT INTO spawnpoint (id,talonflame_nest_id) VALUES (?,10)", rejected)
	if err := a.Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM spawnpoint WHERE id=? AND talonflame_nest_id=-20", late) != 1 ||
		count("SELECT COUNT(*) FROM spawnpoint WHERE id=? AND talonflame_nest_id IS NULL", rejected) != 1 ||
		count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending") != 0 {
		t.Fatal("restart lost a pending assignment or published stale state")
	}

	// More than two batches, an absent lowest ID, and a failure partway through
	// batch two verify keyset progress, bounded commits and atomic retry.
	exec("DELETE FROM spawnpoint")
	exec("DELETE FROM talonflame_spawnpoint_state")
	total := 2*spawnpointBatchSize + 3
	var stateValues, pendingValues, pointValues []string
	var stateArgs, pendingArgs, pointArgs []any
	for id := 1; id <= total; id++ {
		var nest any = int64(-10)
		if id%2 == 1 {
			nest = nil
		}
		stateValues = append(stateValues, "(?,?,'learned')")
		stateArgs = append(stateArgs, id, nest)
		pendingValues = append(pendingValues, "(?)")
		pendingArgs = append(pendingArgs, id)
		if id > 1 {
			current := any(int64(99))
			if id == 2 || id == 3 {
				current = nest // Same non-NULL and same NULL must not be written.
			}
			pointValues = append(pointValues, "(?,?)")
			pointArgs = append(pointArgs, id, current)
		}
	}
	exec("INSERT INTO talonflame_spawnpoint_state (spawnpoint_id,nest_id,status) VALUES "+strings.Join(stateValues, ","), stateArgs...)
	exec("INSERT INTO talonflame_spawnpoint_pending VALUES "+strings.Join(pendingValues, ","), pendingArgs...)
	exec("INSERT INTO spawnpoint VALUES "+strings.Join(pointValues, ","), pointArgs...)
	// A locked, unqueued mismatch must be untouched, even within the scanned
	// ID range. A full reconciliation would block on it or overwrite it.
	const unqueued = 200
	exec("DELETE FROM talonflame_spawnpoint_pending WHERE spawnpoint_id=?", unqueued)
	exec("CREATE TABLE projection_writes (id BIGINT UNSIGNED) ENGINE=InnoDB")
	exec(`CREATE TRIGGER record_projection AFTER UPDATE ON spawnpoint
	 FOR EACH ROW INSERT INTO projection_writes VALUES (NEW.id)`)
	exec("CREATE TABLE projection_failures (id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB")
	exec("INSERT INTO projection_failures VALUES (300)")
	exec(`CREATE TRIGGER fail_projection BEFORE UPDATE ON spawnpoint FOR EACH ROW
	 BEGIN IF NEW.id=300 AND EXISTS(SELECT 1 FROM projection_failures WHERE id=300)
	 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected projection failure'; END IF; END`)
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, "UPDATE spawnpoint SET talonflame_nest_id=99 WHERE id=?", unqueued); err != nil {
		t.Fatal(err)
	}
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = a.Maintain(attempt, time.Now())
	cancel()
	if err == nil || !strings.Contains(err.Error(), "injected projection failure") {
		t.Fatalf("expected injected failure, got %v", err)
	}
	// First batch includes 1..257 except 200: 255 existing rows acknowledged.
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending") != total-spawnpointBatchSize {
		t.Fatal("earlier batch did not commit independently")
	}
	if count("SELECT COUNT(*) FROM projection_writes WHERE id>=258") != 0 ||
		count("SELECT COUNT(*) FROM projection_writes WHERE id IN (2,3)") != 0 {
		t.Fatal("failed batch was partially committed or unchanged rows were written")
	}
	exec("DELETE FROM projection_failures")
	// Reconstructing the worker leaves the retry entirely in the database.
	restarted := &Analyzer{db: db, logger: logger, cfg: DefaultConfig()}
	attempt, cancel = context.WithTimeout(ctx, 5*time.Second)
	err = restarted.Maintain(attempt, time.Now())
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending") != 1 ||
		count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending WHERE spawnpoint_id=1") != 1 {
		t.Fatal("missing first row blocked later batches or was discarded")
	}
	if count(`SELECT COUNT(*) FROM spawnpoint p JOIN talonflame_spawnpoint_state s ON s.spawnpoint_id=p.id
	 WHERE p.id<>? AND NOT (p.talonflame_nest_id <=> s.nest_id)`, unqueued) != 0 {
		t.Fatal("pending assignments did not converge")
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM spawnpoint WHERE id=? AND talonflame_nest_id=99", unqueued) != 1 {
		t.Fatal("maintenance touched an unqueued spawnpoint")
	}
	writes := count("SELECT COUNT(*) FROM projection_writes")
	if err := restarted.Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM projection_writes") != writes {
		t.Fatal("repeated maintenance rewrote completed assignments")
	}
	exec("INSERT INTO spawnpoint VALUES (1,99)")
	if err := restarted.Maintain(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_pending") != 0 ||
		count("SELECT COUNT(*) FROM spawnpoint WHERE id=1 AND talonflame_nest_id IS NULL") != 1 {
		t.Fatal("late low ID was not retried on the next pass")
	}
}
