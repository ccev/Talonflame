package talonflame

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/sirupsen/logrus"
)

func TestMariaDBEvidenceRetention(t *testing.T) {
	dsn := os.Getenv("TALONFLAME_RETENTION_TEST_DSN")
	if dsn == "" {
		t.Skip("set TALONFLAME_RETENTION_TEST_DSN to a disposable database")
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
	exec(`CREATE TABLE talonflame_evidence (
	 hour_start BIGINT UNSIGNED NOT NULL,encounter_id BIGINT UNSIGNED NOT NULL,spawnpoint_id BIGINT UNSIGNED NOT NULL,
	 nest_id BIGINT NULL,pokemon_id INT NOT NULL DEFAULT 1,pokemon_form INT NULL,roll_u24 INT UNSIGNED NOT NULL DEFAULT 0,
	 observed_at BIGINT UNSIGNED NOT NULL,
	 PRIMARY KEY (hour_start,encounter_id,spawnpoint_id),
	 KEY ix_talonflame_evidence_nest_hour (nest_id,hour_start),
	 KEY ix_talonflame_evidence_spawnpoint (spawnpoint_id,hour_start),
	 KEY ix_talonflame_evidence_retention (observed_at)
	 ) ENGINE=InnoDB`)
	exec("CREATE TABLE talonflame_spawnpoint_pending (spawnpoint_id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB")
	a := newEvidenceCache()
	a.db, a.cfg, a.logger = db, DefaultConfig(), logrus.New()
	a.logger.SetOutput(io.Discard)
	now := time.Now()
	cutoff := now.Add(-a.cfg.Retention).Unix()
	hour := cutoff - 7200
	const encounter = uint64(1<<63 + 123)
	observations := []Observation{
		{Hour: hour, Encounter: encounter, Spawnpoint: 11, At: cutoff - 1},
		{Hour: hour, Encounter: encounter, Spawnpoint: 12, At: cutoff - 1},
		{Hour: hour, Encounter: encounter + 1, Spawnpoint: 11, At: cutoff},
		{Hour: hour + 3600, Encounter: encounter, Spawnpoint: 11, At: cutoff + 1},
		{Hour: hour + 3600, Encounter: encounter, Spawnpoint: 12, At: cutoff - 1},
	}
	for _, o := range observations {
		exec("INSERT INTO talonflame_evidence (hour_start,encounter_id,spawnpoint_id,observed_at) VALUES (?,?,?,?)", o.Hour, o.Encounter, o.Spawnpoint, o.At)
		a.remember(o)
	}
	keys, err := a.expiredEvidenceKeys(ctx, cutoff)
	if err != nil || len(keys) != 3 {
		t.Fatalf("expired key selection: %v, %v", keys, err)
	}
	// Refresh after selection, before the DELETE acquires locks. Rows sharing
	// only part of the composite identity, and the exact cutoff, must survive.
	refreshed := observations[1]
	refreshed.At = cutoff
	exec("UPDATE talonflame_evidence SET observed_at=? WHERE hour_start=? AND encounter_id=? AND spawnpoint_id=?",
		refreshed.At, refreshed.Hour, refreshed.Encounter, refreshed.Spawnpoint)
	a.remember(refreshed)
	if n, err := a.deleteEvidenceBatch(ctx, keys, cutoff); err != nil || n != 2 {
		t.Fatalf("refreshed row was lost or expired rows survived: deleted=%d, err=%v", n, err)
	}
	if err := a.Maintain(ctx, now); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM talonflame_evidence") != 3 || len(a.evidence) != 3 {
		t.Fatal("database and memory retention lost a boundary/refreshed observation")
	}
	for _, o := range []Observation{refreshed, observations[2], observations[3]} {
		if a.evidence[o.key()] == nil || count(`SELECT COUNT(*) FROM talonflame_evidence
		 WHERE hour_start=? AND encounter_id=? AND spawnpoint_id=?`, o.Hour, o.Encounter, o.Spawnpoint) != 1 {
			t.Fatal("cleanup did not preserve the complete evidence identity", o)
		}
	}

	exec("DELETE FROM talonflame_evidence")
	total := 2*evidenceBatchSize + 7
	const retained = 100
	failing := evidenceBatchSize + 44
	var values []string
	var args []any
	for id := 1; id <= total; id++ {
		at := cutoff - 1
		if id == retained {
			at = cutoff
		}
		values = append(values, "(?,?,?,?)")
		args = append(args, hour, encounter, id, at)
	}
	exec("INSERT INTO talonflame_evidence (hour_start,encounter_id,spawnpoint_id,observed_at) VALUES "+strings.Join(values, ","), args...)
	exec("CREATE TABLE retention_failures (spawnpoint_id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB")
	exec("INSERT INTO retention_failures VALUES (?)", failing)
	exec(`CREATE TRIGGER fail_retention BEFORE DELETE ON talonflame_evidence FOR EACH ROW
	 BEGIN IF EXISTS(SELECT 1 FROM retention_failures WHERE spawnpoint_id=OLD.spawnpoint_id)
	 THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='injected retention failure'; END IF; END`)
	// Hold an unrelated live row inside the PK span of the first batch. Exact
	// key deletion must neither wait for this lock nor remove that observation.
	blocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	if _, err := blocker.ExecContext(ctx, `UPDATE talonflame_evidence SET pokemon_id=2
	 WHERE hour_start=? AND encounter_id=? AND spawnpoint_id=?`, hour, encounter, retained); err != nil {
		t.Fatal(err)
	}
	attempt, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = a.deleteExpiredEvidence(attempt, cutoff)
	cancel()
	if err == nil || !strings.Contains(err.Error(), "injected retention failure") {
		t.Fatalf("expected failure in batch two without blocking on a live row, got %v", err)
	}
	remaining := total - 1 - evidenceBatchSize
	if count("SELECT COUNT(*) FROM talonflame_evidence WHERE observed_at<?", cutoff) != remaining ||
		count("SELECT COUNT(*) FROM talonflame_evidence WHERE spawnpoint_id>=?", evidenceBatchSize+2) != remaining {
		t.Fatal("earlier batch was not committed or failed batch was partially deleted")
	}
	exec("DELETE FROM retention_failures")
	// Cancellation while waiting on an expired row also rolls back the current
	// batch, without restoring the already committed first batch.
	expiredBlocker, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer expiredBlocker.Rollback()
	if _, err := expiredBlocker.ExecContext(ctx, `UPDATE talonflame_evidence SET pokemon_id=2
	 WHERE hour_start=? AND encounter_id=? AND spawnpoint_id=?`, hour, encounter, failing); err != nil {
		t.Fatal(err)
	}
	attempt, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
	err = a.deleteExpiredEvidence(attempt, cutoff)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup ignored cancellation: %v", err)
	}
	if count("SELECT COUNT(*) FROM talonflame_evidence WHERE observed_at<?", cutoff) != remaining {
		t.Fatal("cancelled batch left partial deletes")
	}
	if err := expiredBlocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	// A new worker has no cursor to restore: remaining expired keys are enough.
	restarted := &Analyzer{db: db, cfg: a.cfg, logger: a.logger}
	attempt, cancel = context.WithTimeout(ctx, 5*time.Second)
	err = restarted.Maintain(attempt, now)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	if count("SELECT COUNT(*) FROM talonflame_evidence") != 1 ||
		count("SELECT COUNT(*) FROM talonflame_evidence WHERE spawnpoint_id=? AND observed_at=?", retained, cutoff) != 1 {
		t.Fatal("cleanup did not converge or removed retained evidence")
	}
	if err := restarted.Maintain(ctx, now); err != nil {
		t.Fatal(err)
	}
}
