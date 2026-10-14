package talonmigration

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/golang/geo/s2"
	"github.com/paulmach/orb"
	"github.com/paulmach/orb/geojson"
	"github.com/sirupsen/logrus"

	"github.com/UnownHash/Fletchling/geo"
)

func tokenAt(lat, lon float64) uint64 {
	return uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(lat, lon)).Parent(20)) >> 20
}

func square(lat, lon, radius float64) orb.Ring {
	return orb.Ring{
		{lon - radius, lat - radius}, {lon + radius, lat - radius},
		{lon + radius, lat + radius}, {lon - radius, lat + radius},
		{lon - radius, lat - radius},
	}
}

func TestS2MembershipAndAmbiguity(t *testing.T) {
	token := tokenAt(53.55, 10)
	center := s2.CellID(token << 20).LatLng()
	lat, lon := center.Lat.Degrees(), center.Lng.Degrees()
	tree := geo.NewFenceRTree[int64]()
	outer := orb.Polygon{square(lat, lon, .01)}
	if err := tree.InsertGeometry(outer, 10); err != nil {
		t.Fatal(err)
	}
	row := matchSpawnpoint(token, tree)
	if row.status != "imported" || row.nest == nil || *row.nest != 10 {
		t.Fatalf("expected unique assignment, got %+v", row)
	}
	if err := tree.InsertGeometry(orb.MultiPolygon{outer}, 20); err != nil {
		t.Fatal(err)
	}
	if row := matchSpawnpoint(token, tree); row.status != "ambiguous" || row.nest != nil || len(row.candidates) != 2 {
		t.Fatalf("overlap lost: %+v", row)
	}
	if row := matchSpawnpoint(tokenAt(40, -70), tree); row.status != "outside" {
		t.Fatalf("outside point: %+v", row)
	}
	for _, invalid := range []uint64{0, 2, 1 << 44, (6 << 41) | 1} {
		if row := matchSpawnpoint(invalid, tree); row.status != "invalid" {
			t.Fatalf("invalid token %x accepted: %+v", invalid, row)
		}
	}
	holes := geo.NewFenceRTree[int64]()
	if err := holes.InsertGeometry(orb.Polygon{square(lat, lon, .01), square(lat, lon, .001)}, 30); err != nil {
		t.Fatal(err)
	}
	if row := matchSpawnpoint(token, holes); row.status != "outside" {
		t.Fatalf("point in hole assigned: %+v", row)
	}
}

// This test ONLY runs when explicitly pointed at a disposable database whose
// name starts with talonflame_test_. It never loads application credentials.
func TestMariaDBMigrationAndBackfill(t *testing.T) {
	dsn := os.Getenv("TALONFLAME_TEST_DSN")
	if dsn == "" {
		t.Skip("set TALONFLAME_TEST_DSN to a disposable MariaDB database")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "talonflame_test_") {
		t.Fatal("integration database name must start with talonflame_test_")
	}
	ctx := context.Background()
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := admin.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`CREATE TABLE nests (nest_id BIGINT PRIMARY KEY, polygon GEOMETRY NOT NULL,
		pokemon_id INT NULL, updated BIGINT NULL, active BOOL DEFAULT 1) ENGINE=InnoDB`)
	exec(`CREATE TABLE spawnpoint (id BIGINT UNSIGNED PRIMARY KEY, lat DOUBLE DEFAULT 1, lon DOUBLE DEFAULT 2) ENGINE=InnoDB`)
	tokens := []uint64{tokenAt(53.55, 10), tokenAt(53.55, 10.002), tokenAt(40, -70), 2}
	for _, token := range tokens {
		exec("INSERT INTO spawnpoint (id) VALUES (?)", token)
	}
	polygon, err := geojson.NewGeometry(orb.Polygon{square(53.55, 10, .01)}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	exec("INSERT INTO nests (nest_id,polygon,pokemon_id,updated,active) VALUES (10,ST_GeomFromGeoJSON(?),25,1791417600,0)", polygon)
	// Exercise the exact documented permissions, not an administrative account.
	exec("CREATE USER 'talon'@'localhost' IDENTIFIED BY 'talon'")
	quotedDB := "`" + cfg.DBName + "`"
	exec("GRANT ALL PRIVILEGES ON " + quotedDB + ".nests TO 'talon'@'localhost'")
	exec("GRANT UPDATE, ALTER ON " + quotedDB + ".spawnpoint TO 'talon'@'localhost'")
	exec("GRANT SELECT (id) ON " + quotedDB + ".spawnpoint TO 'talon'@'localhost'")
	for _, table := range []string{"schema_migrations", "nest_groups", "nest_sources", "spawnpoint_candidates", "spawnpoint_state", "evidence", "backfill_assignments", "backfill_candidates"} {
		exec("GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER ON " + quotedDB + ".talonflame_" + table + " TO 'talon'@'localhost'")
	}
	cfg.User, cfg.Passwd = "talon", "talon"
	limited, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	conn, err := limited.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	run := func(apply bool) {
		t.Helper()
		if err := Run(ctx, conn, logger, Options{Mode: "all", Apply: apply, BatchSize: 2}); err != nil {
			t.Fatal(err)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := admin.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	run(false)
	if count("SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND LEFT(TABLE_NAME,11)='talonflame_'") != 0 {
		t.Fatal("preview created tables")
	}
	if count("SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='nests' AND COLUMN_NAME='last_changed'") != 0 {
		t.Fatal("preview altered nests")
	}
	run(true)
	if count("SELECT COUNT(*) FROM spawnpoint WHERE talonflame_nest_id=10") != 2 {
		t.Fatal("inactive polygon membership not backfilled")
	}
	if count("SELECT COUNT(*) FROM nests WHERE last_changed IS NULL AND pokemon_id=25 AND updated=1791417600") != 1 {
		t.Fatal("invented a migration time or altered source species")
	}
	if count("SELECT COUNT(*) FROM spawnpoint WHERE lat=1 AND lon=2") != 4 {
		t.Fatal("changed Golbat-owned coordinates")
	}
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_state WHERE status='invalid'") != 1 {
		t.Fatal("invalid token not recorded")
	}
	if _, err := conn.QueryContext(ctx, "SELECT lat FROM spawnpoint"); err == nil {
		t.Fatal("dedicated account can read unrelated spawnpoint columns")
	}
	// Reimport must preserve learned rejections and explicit reassignment.
	exec("UPDATE spawnpoint SET talonflame_nest_id=NULL WHERE id=?", tokens[0])
	exec("UPDATE talonflame_spawnpoint_state SET status='rejected',reason='varied low-roll species' WHERE spawnpoint_id=?", tokens[0])
	exec("INSERT INTO talonflame_nest_groups (nest_id,pokemon_id,last_changed) VALUES (20,16,1791417600)")
	exec("UPDATE spawnpoint SET talonflame_nest_id=20 WHERE id=?", tokens[1])
	exec("UPDATE talonflame_spawnpoint_state SET status='learned',nest_id=20 WHERE spawnpoint_id=?", tokens[1])
	run(true)
	if count("SELECT COUNT(*) FROM spawnpoint WHERE id=? AND talonflame_nest_id IS NULL", tokens[0]) != 1 || count("SELECT COUNT(*) FROM spawnpoint WHERE id=? AND talonflame_nest_id=20", tokens[1]) != 1 {
		t.Fatal("reimport overwrote learned decisions")
	}
	// A new point in a source that has been merged resolves to the logical group.
	newToken := tokenAt(53.55, 10.004)
	exec("INSERT INTO spawnpoint (id) VALUES (?)", newToken)
	exec("UPDATE talonflame_nest_sources SET group_id=20 WHERE source_nest_id=10")
	run(true)
	if count("SELECT COUNT(*) FROM spawnpoint WHERE id=? AND talonflame_nest_id=20", newToken) != 1 {
		t.Fatal("backfill ignored learned group merge")
	}
	// Changed source geometry clears obsolete imports, while rejection history stays.
	exec("UPDATE nests SET polygon=ST_GeomFromGeoJSON(?)", fmt.Sprintf(`{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}`))
	run(true)
	if count("SELECT COUNT(*) FROM spawnpoint WHERE id=? AND talonflame_nest_id IS NULL", newToken) != 1 {
		t.Fatal("obsolete imported membership not cleared")
	}
	if count("SELECT COUNT(*) FROM talonflame_spawnpoint_state WHERE status='rejected' AND reason='varied low-roll species'") != 1 {
		t.Fatal("lost persistent rejection reason")
	}
	// A checksum change fails before modifying the live assignments.
	exec("UPDATE talonflame_schema_migrations SET sha256=REPEAT('0',64)")
	if err := Run(ctx, conn, logger, Options{Mode: "all", Apply: true, BatchSize: 2}); err == nil {
		t.Fatal("accepted a changed applied migration")
	}
}
