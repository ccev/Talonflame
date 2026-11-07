package talonflame

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/golang/geo/s2"
	"github.com/paulmach/orb/geojson"
	"github.com/sirupsen/logrus"
)

func TestMariaDBMapConfirmationIgnoresLegacyActivation(t *testing.T) {
	dsn := os.Getenv("TALONFLAME_MAP_TEST_DSN")
	if dsn == "" {
		t.Skip("set TALONFLAME_MAP_TEST_DSN to a disposable database")
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
	// Temporary fixtures stay on one connection and cannot overwrite real tables.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TEMPORARY TABLE nests (nest_id BIGINT PRIMARY KEY,name TEXT,area_name TEXT,lat DOUBLE,lon DOUBLE,spawnpoints INT,active BOOLEAN,discarded TEXT,polygon GEOMETRY NOT NULL)`)
	exec(`CREATE TEMPORARY TABLE talonflame_nest_groups (nest_id BIGINT PRIMARY KEY,pokemon_id INT NULL,pokemon_form INT,last_changed BIGINT NULL,status TEXT)`)
	exec(`CREATE TEMPORARY TABLE talonflame_nest_sources (source_nest_id BIGINT,group_id BIGINT)`)
	exec(`CREATE TEMPORARY TABLE talonflame_spawnpoint_state (spawnpoint_id BIGINT UNSIGNED PRIMARY KEY,nest_id BIGINT,INDEX(nest_id))`)
	raw, err := json.Marshal(geojson.NewGeometry(testPark))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, active, count int
		reason, status    string
		pokemon           any
	}{
		{10, 0, 0, "spawnpoints", "confirmed", 16},
		{20, 0, 10, "overlap", "confirmed", 276},
		{30, 1, 10, "", "provisional", 769},
		{40, 0, 0, "spawnpoints", "conflicting", 54},
		{50, 1, 10, "", "confirmed", nil},
		{60, 1, 10, "", "confirmed", 25},
		{70, 0, 0, "", "confirmed", 10},
		{80, 0, 0, "spawnpoints", "confirmed", 63},
	} {
		exec(`INSERT INTO nests VALUES (?,'Park','Area',53.56,9.98,?,?,?,ST_GeomFromGeoJSON(?))`, row.id, row.count, row.active, row.reason, string(raw))
		exec(`INSERT INTO talonflame_nest_groups VALUES (?,?,0,NULL,?)`, row.id, row.pokemon, row.status)
		exec(`INSERT INTO talonflame_nest_sources VALUES (?,?)`, row.id, row.id)
	}
	// An inactive imported source can also contain two independently confirmed
	// populations. Both must survive the existing splitting/alias machinery.
	exec(`INSERT INTO talonflame_nest_groups VALUES (-80,66,0,NULL,'confirmed')`)
	exec(`INSERT INTO talonflame_nest_sources VALUES (80,-80)`)
	for i, lon := range []float64{9.975, 9.985} {
		group := 80
		if i == 1 {
			group = -80
		}
		point := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(53.56, lon)).Parent(20)) >> 20
		exec(`INSERT INTO talonflame_spawnpoint_state VALUES (?,?)`, point, group)
	}
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	m := newMapStore(db, logger)
	if err := m.refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	first := m.current.Load()
	for _, id := range []string{"10", "20", "60", "70", "80", "-80"} {
		if first.entries[id] == nil {
			t.Errorf("confirmed group %s hidden by legacy activation", id)
		}
	}
	for _, id := range []string{"30", "40", "50"} {
		if first.entries[id] != nil {
			t.Errorf("unconfirmed/species-less group %s displayed", id)
		}
	}
	exec(`UPDATE nests SET active=1,discarded=NULL WHERE nest_id=10`)
	exec(`UPDATE nests SET active=0,discarded='spawnpoints' WHERE nest_id=60`)
	if err := m.refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	if m.current.Load().revision != first.revision {
		t.Fatal("legacy activation changed confirmed map output")
	}
	exec(`UPDATE talonflame_nest_groups SET status='confirmed' WHERE nest_id=30`)
	m.invalidate(30)
	if err := m.refresh(ctx, false); err != nil {
		t.Fatal(err)
	}
	if m.current.Load().entries["30"] == nil {
		t.Fatal("incremental source load still gates confirmed groups")
	}
	exec(`UPDATE talonflame_nest_groups SET status='provisional' WHERE nest_id=10`)
	exec(`DELETE FROM nests WHERE nest_id=20`)
	if err := m.refresh(ctx, true); err != nil {
		t.Fatal(err)
	}
	last := m.current.Load()
	if last.entries["10"] != nil || last.entries["20"] != nil || last.entries["60"] == nil {
		t.Fatal("confirmation withdrawal/source deletion or legacy flag handling failed")
	}
}
