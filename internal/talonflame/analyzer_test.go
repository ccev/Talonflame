package talonflame

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/UnownHash/Fletchling/internal/talonmigration"
	"github.com/go-sql-driver/mysql"
	"github.com/golang/geo/s2"
	"github.com/sirupsen/logrus"
)

func TestSpeciesRollReferenceAndThreshold(t *testing.T) {
	// Goldens from /pers/dev/encounterid/species_roll.py, including the exact
	// integers immediately below and at the quarter boundary.
	for _, test := range []struct {
		id   uint64
		want uint32
	}{{1, 12262101}, {123456789, 11140710}, {1 << 63, 12263604}, {1<<64 - 1, 4512108}, {235628208508100, 4194303}, {177407125015748, 4194304}} {
		if got := SpeciesRoll(test.id); got != test.want {
			t.Fatalf("%d: got %d, want %d", test.id, got, test.want)
		}
	}
	form := 0
	now := time.Now()
	p := Pokemon{EncounterID: "235628208508100", SpawnpointID: "47b18f21665", PokemonID: 25, Form: &form, SeenType: "wild"}
	if _, eligible, err := parsePokemon(p, now); err != nil || !eligible {
		t.Fatal("boundary predecessor rejected", err)
	}
	p.EncounterID = "177407125015748"
	if _, eligible, _ := parsePokemon(p, now); eligible {
		t.Fatal("quarter threshold accepted")
	}
	for _, text := range []string{`"18446744073709551615"`, `18446744073709551615`} {
		var id ID
		if err := json.Unmarshal([]byte(text), &id); err != nil || id != "18446744073709551615" {
			t.Fatal("lost uint64 precision", id, err)
		}
	}
}

func TestPerNestChangesAndSparseHours(t *testing.T) {
	zone, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 12, 20, 0, 0, time.UTC)
	boundary := hourBoundary(now, zone)
	if boundary != time.Date(2026, 10, 14, 12, 15, 0, 0, time.UTC).Unix() {
		t.Fatal("lost quarter-hour timezone offset")
	}
	g := &Group{ID: 1, Pair: Pair{25, 0}, Confirmed: true, zone: zone}
	var observations []Observation
	for i := 0; i < 5; i++ {
		observations = append(observations, Observation{Pair: Pair{16, 0}, At: boundary + int64(i+1), Encounter: uint64(i + 1), Roll: uint32(i%2) * 3 << 20})
	}
	d, ok := infer(g, observations, now, 5, .9)
	if !ok || d.Pair.Pokemon != 16 || d.Boundary != boundary {
		t.Fatal("missed local full-hour change", d, ok)
	}
	if g.Pair.Pokemon != 25 {
		t.Fatal("inference mutated another nest's state")
	}
	// A small nest may only produce one eligible encounter per hour. It must
	// eventually change, not remain permanently locked to its former species.
	observations = []Observation{{Pair: Pair{25, 0}, At: boundary - 6*3600}}
	for i := 0; i < 5; i++ {
		observations = append(observations, Observation{Pair: Pair{16, 0}, At: boundary - int64(4-i)*3600 + 1, Encounter: uint64(i + 1), Roll: uint32(i%2) * 3 << 20})
	}
	d, ok = infer(g, observations, now, 5, .9)
	if !ok || d.Boundary != boundary-4*3600 {
		t.Fatal("sparse evidence failed", d, ok)
	}
	if _, ok := infer(g, observations[:3], now, 5, .9); ok {
		t.Fatal("confirmed too little evidence")
	}
}

func TestMixedPolygonIsNotAMidHourMigration(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	g := &Group{ID: 1, Pair: Pair{54, 286}, Confirmed: true, zone: time.UTC}
	var observations []Observation
	for i := 0; i < 5; i++ {
		observations = append(observations, Observation{Pair: g.Pair, At: now.Add(-10 * time.Minute).Unix(), Spawnpoint: uint64(i + 1)})
	}
	for i := 0; i < 5; i++ {
		observations = append(observations, Observation{Pair: Pair{309, 1484}, At: now.Add(-5 * time.Minute).Unix(), Spawnpoint: uint64(i + 10)})
	}
	if d, ok := infer(g, observations, now, 5, .9); ok {
		t.Fatalf("invented mid-hour migration: %+v", d)
	}
	if conflictingEvidence(g, observations, now, 5, .9) == nil {
		t.Fatal("missed mixed populations")
	}
	if _, ok := latestPointConsensus(observations, 5, .9); ok {
		t.Fatal("one corner represented the whole polygon")
	}
}

func TestOrdinaryTableSliceIsNotANest(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	g := &Group{zone: time.UTC}
	var observations []Observation
	// Live false-positive Ponyta support occupied only roll .042.. .126.
	for i := 0; i < 6; i++ {
		observations = append(observations, Observation{Pair: Pair{77, 1011}, Encounter: uint64(i + 1), Spawnpoint: uint64(i + 1), At: now.Unix(), Roll: 705410 + uint32(i)*280000})
	}
	if _, ok := infer(g, observations, now, 5, .9); ok {
		t.Fatal("ordinary spawn-table slice confirmed as nest")
	}
	// A genuine nest override occupies the complete gate, regardless of whether
	// its species is also abundant in the ordinary spawn table (e.g. Electrike).
	for i := range observations {
		observations[i].Pair = Pair{309, 1484}
		observations[i].Roll = uint32(i) * 800000
	}
	if d, ok := infer(g, observations, now, 5, .9); !ok || d.Pair.Pokemon != 309 {
		t.Fatal("full-gate override rejected", d, ok)
	}
	// A single surviving encounter observed in five different hours is one vote.
	for i := range observations {
		observations[i].Encounter = 1
		observations[i].Spawnpoint = 1
		observations[i].At = now.Add(-time.Duration(i) * time.Hour).Unix()
	}
	if _, ok := infer(g, observations, now, 5, .9); ok {
		t.Fatal("repeated encounter counted as independent confirmations")
	}
}

func TestDistributedContradictionsWithdrawConfirmation(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 30, 0, 0, time.UTC)
	g := &Group{Pair: Pair{77, 1011}, Confirmed: true, zone: time.UTC}
	var observations []Observation
	for i := 0; i < 6; i++ {
		observations = append(observations, Observation{Pair: g.Pair, At: now.Unix()})
	}
	observations = append(observations, Observation{Pair: Pair{63, 304}, At: now.Unix()}, Observation{Pair: Pair{25, 598}, At: now.Unix()})
	if conflictingEvidence(g, observations, now, 5, .9) == nil {
		t.Fatal("distributed rivals left sticky 6/8 confirmation")
	}
	for i := range observations {
		observations[i].At = now.Add(-time.Hour).Unix()
	}
	if conflictingEvidence(g, observations, now, 5, .9) != nil {
		t.Fatal("stale contradictions defeated carried label")
	}
}

func TestWebhookValidationAndBackpressure(t *testing.T) {
	s := NewServer(nil)
	for _, test := range []struct {
		method, body string
		want         int
	}{{"GET", "", 405}, {"POST", "{}", 400}, {"POST", "[] {}", 400}, {"POST", "[]", 200}} {
		r := httptest.NewRequest(test.method, "/webhook", strings.NewReader(test.body))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != test.want {
			t.Fatalf("%s %s: %d", test.method, test.body, w.Code)
		}
	}
	for len(s.queue) < cap(s.queue) {
		s.queue <- batch{}
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("[]")))
	if w.Code != 503 {
		t.Fatal("full queue accepted an unqueued batch")
	}
}

func TestShutdownDrainsAcceptedBatches(t *testing.T) {
	service := NewServer(&Analyzer{})
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		service.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`[{"type":"ignored","message":{}}]`)))
		if w.Code != 200 {
			t.Fatal("batch not accepted")
		}
	}
	service.Drain()
	if err := service.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(service.queue) != 0 {
		t.Fatal("shutdown discarded accepted batches")
	}
}

func TestMariaDBAnalyzerLifecycle(t *testing.T) {
	dsn := os.Getenv("TALONFLAME_ANALYZER_TEST_DSN")
	if dsn == "" {
		t.Skip("set TALONFLAME_ANALYZER_TEST_DSN to a disposable database")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil || !strings.HasPrefix(cfg.DBName, "talonflame_test_") {
		t.Fatal("disposable test database required")
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
	exec(`CREATE TABLE nests (nest_id BIGINT PRIMARY KEY,polygon GEOMETRY NOT NULL,pokemon_id INT NULL,pokemon_form INT NULL,updated BIGINT NULL) ENGINE=InnoDB`)
	exec(`CREATE TABLE spawnpoint (id BIGINT UNSIGNED PRIMARY KEY) ENGINE=InnoDB`)
	exec(`INSERT INTO nests (nest_id,polygon) VALUES (10,ST_GeomFromGeoJSON('{"type":"Polygon","coordinates":[[[9.9,53.4],[10.1,53.4],[10.1,53.7],[9.9,53.7],[9.9,53.4]]]}')),
	 (20,ST_GeomFromGeoJSON('{"type":"Polygon","coordinates":[[[11,54],[12,54],[12,55],[11,55],[11,54]]]}'))`)
	var points []uint64
	for i := 0; i < 8; i++ {
		id := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(53.55, 10+float64(i)*.0002)).Parent(20)) >> 20
		points = append(points, id)
		exec("INSERT INTO spawnpoint (id) VALUES (?)", id)
	}
	logger := logrus.New()
	var logs bytes.Buffer
	logger.SetOutput(&logs)
	conn, err := admin.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := talonmigration.Run(ctx, conn, logger, talonmigration.Options{Mode: "all", Apply: true, BatchSize: 3}); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	// Runtime uses the same narrow existing-table privileges as migration.
	exec("CREATE USER 'talon_analyzer_test'@'localhost' IDENTIFIED BY 'talon'")
	quoted := "`" + cfg.DBName + "`"
	exec("GRANT ALL PRIVILEGES ON " + quoted + ".nests TO 'talon_analyzer_test'@'localhost'")
	exec("GRANT SELECT (id), UPDATE, ALTER ON " + quoted + ".spawnpoint TO 'talon_analyzer_test'@'localhost'")
	rows, err := admin.Query("SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE() AND LEFT(TABLE_NAME,11)='talonflame_'")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var table string
		rows.Scan(&table)
		tables = append(tables, table)
	}
	rows.Close()
	for _, table := range tables {
		exec("GRANT SELECT,INSERT,UPDATE,DELETE,CREATE,ALTER ON " + quoted + ".`" + table + "` TO 'talon_analyzer_test'@'localhost'")
	}
	cfg.User, cfg.Passwd = "talon_analyzer_test", "talon"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(4)
	a, err := New(ctx, db, logger, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	base := time.Now().Truncate(time.Hour).Add(15 * time.Minute)
	form := 0
	var encounter uint64 = 1
	var idCount int
	nextID := func() uint64 {
		idCount++
		for {
			encounter += 0x9e3779b97f4a7c15
			roll := SpeciesRoll(encounter)
			if (idCount%2 == 1 && roll < 1<<20) || (idCount%2 == 0 && roll >= 3<<20 && roll < 1<<22) {
				return encounter
			}
		}
	}
	feed := func(point uint64, species int, at time.Time, id uint64) {
		t.Helper()
		stamp := at.Unix()
		if err := a.Process(ctx, Pokemon{EncounterID: ID(fmt.Sprint(id)), SpawnpointID: fmt.Sprintf("%x", point), PokemonID: species, Form: &form, SeenType: "wild", Modified: &stamp}, at); err != nil {
			t.Fatal(err)
		}
	}
	var reused uint64
	for i := 0; i < 5; i++ {
		id := nextID()
		if i == 0 {
			reused = id
		}
		feed(points[i], 25, base, id)
	}
	if err := a.Evaluate(ctx, base); err != nil {
		t.Fatal(err)
	}
	if !a.groups[10].Confirmed || a.groups[10].Pair.Pokemon != 25 || a.groups[10].LastChanged != nil {
		t.Fatal("baseline confirmation invented migration")
	}
	if strings.Contains(logs.String(), "low-roll encounter") {
		t.Fatal("INFO logged individual encounters")
	}
	logger.SetLevel(logrus.DebugLevel)
	feed(points[0], 25, base, reused)
	if len(a.byGroup[10]) != 5 || !strings.Contains(logs.String(), "low-roll encounter") {
		t.Fatal("duplicate counted or DEBUG missing")
	}
	changed := base.Add(time.Hour)
	for i := 0; i < 5; i++ {
		id := nextID()
		if i == 0 {
			id = reused
		}
		feed(points[i], 16, changed, id)
	}
	if err := a.Evaluate(ctx, changed); err != nil {
		t.Fatal(err)
	}
	if a.groups[10].Pair.Pokemon != 16 || a.groups[10].LastChanged == nil || *a.groups[10].LastChanged != changed.Truncate(time.Hour).Unix() {
		t.Fatal("hour boundary change failed")
	}
	if a.groups[20].Confirmed {
		t.Fatal("applied global migration")
	}
	// Three varied contradictions and a coherent alternate series coexist.
	for i := 0; i < 3; i++ {
		at := changed.Add(time.Duration(i) * time.Hour)
		feed(points[5], 40+i, at, nextID())
		feed(points[6], 99, at, nextID())
		feed(points[0], 16, at.Add(time.Second), nextID())
	}
	now := changed.Add(2*time.Hour + time.Minute)
	if err := a.Evaluate(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := a.Repair(ctx, now); err != nil {
		t.Fatal(err)
	}
	if a.points[points[5]].Status != "rejected" || a.points[points[5]].Nest != 0 {
		t.Fatal("varied point not rejected")
	}
	child := a.points[points[6]].Nest
	if child == 0 || child == 10 || a.groups[child].Pair.Pokemon != 99 {
		t.Fatal("consistent alternate deleted instead of split")
	}
	for i := 0; i < 2; i++ {
		feed(points[6], 99, now.Add(time.Duration(i+1)*time.Hour), nextID())
	}
	if err := a.Evaluate(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !a.groups[child].Confirmed {
		t.Fatal("subnest failed confirmation")
	}
	var source sql.NullInt64
	if err := admin.QueryRow("SELECT pokemon_id FROM nests WHERE nest_id=10").Scan(&source); err != nil || source.Valid {
		t.Fatal("combined polygon still advertised a single species", err)
	}
	if err := a.Maintain(ctx, now); err != nil {
		t.Fatal(err)
	}
	a.Close()
	restored, err := New(ctx, db, logger, DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if !restored.groups[child].Confirmed {
		t.Fatal("lost subnest on restart")
	}
	state, err := restored.point(ctx, points[5])
	if err != nil || state.Status != "rejected" || state.Nest != 0 {
		t.Fatal("lost rejection on restart", state, err)
	}
	// A scan-order-dependent rival population in another source must become
	// explicitly conflicting, without falsely writing a migration boundary.
	a = restored
	mixedAt := now.Add(3 * time.Hour)
	var mixedPoints []uint64
	for i := 0; i < 10; i++ {
		id := uint64(s2.CellIDFromLatLng(s2.LatLngFromDegrees(54.5, 11.5+float64(i)*.0002)).Parent(20)) >> 20
		mixedPoints = append(mixedPoints, id)
		exec("INSERT INTO spawnpoint (id) VALUES (?)", id)
	}
	for i := 0; i < 5; i++ {
		feed(mixedPoints[i], 54, mixedAt, nextID())
	}
	if err := a.Evaluate(ctx, mixedAt); err != nil {
		t.Fatal(err)
	}
	for i := 5; i < 10; i++ {
		feed(mixedPoints[i], 309, mixedAt.Add(time.Minute), nextID())
	}
	if err := a.Evaluate(ctx, mixedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !a.groups[20].Conflicting || a.groups[20].LastChanged != nil {
		t.Fatal("mixed polygon misreported as migration")
	}
	for _, group := range a.Groups() {
		if group.ID == 20 && group.Confirmed {
			t.Fatal("mixed group API still advertised confirmation")
		}
	}
	var status string
	if err := admin.QueryRow("SELECT status FROM talonflame_nest_groups WHERE nest_id=20").Scan(&status); err != nil || status != "conflicting" {
		t.Fatal("conflict not persisted", status, err)
	}
	if err := admin.QueryRow("SELECT pokemon_id FROM nests WHERE nest_id=20").Scan(&source); err != nil || source.Valid {
		t.Fatal("mixed source kept a single-species label", err)
	}
	// Sampling only the rival corner first in the next hour is insufficient
	// to resolve the conflict. Fresh agreement across known points can resolve it.
	resolvedAt := mixedAt.Add(time.Hour)
	for i := 5; i < 10; i++ {
		feed(mixedPoints[i], 309, resolvedAt, nextID())
	}
	if err := a.Evaluate(ctx, resolvedAt); err != nil {
		t.Fatal(err)
	}
	if !a.groups[20].Conflicting {
		t.Fatal("one-corner sampling cleared persistent conflict")
	}
	for i := 0; i < 5; i++ {
		feed(mixedPoints[i], 309, resolvedAt, nextID())
	}
	if err := a.Evaluate(ctx, resolvedAt); err != nil {
		t.Fatal(err)
	}
	if a.groups[20].Conflicting || a.groups[20].Pair.Pokemon != 309 {
		t.Fatal("fresh spatial agreement failed to resolve conflict")
	}
	// Correcting a startup baseline inside the first observed hour must not
	// invent a migration at a boundary for which no prior-period evidence exists.
	exec("INSERT INTO talonflame_nest_groups (nest_id,pokemon_id,pokemon_form,status) VALUES (30,25,0,'confirmed')")
	a.groups[30] = &Group{ID: 30, Pair: Pair{25, 0}, Confirmed: true, zone: time.UTC}
	a.knownGroups++
	for i := 0; i < 55; i++ {
		pair := Pair{16, 0}
		at := base.Add(5 * time.Minute)
		if i < 5 {
			pair = Pair{25, 0}
			at = base
		}
		id := nextID()
		a.remember(Observation{Hour: hourBoundary(at, time.UTC), Encounter: id, Spawnpoint: uint64(10000 + i), Nest: 30, Pair: pair, Roll: SpeciesRoll(id), At: at.Unix()})
	}
	if err := a.Evaluate(ctx, base.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if a.groups[30].Pair.Pokemon != 16 || a.groups[30].LastChanged != nil {
		t.Fatal("startup correction invented a migration timestamp")
	}
	// Exercise persistence and API withdrawal for an older, narrow-roll false
	// positive, then verify that fresh, broad evidence can restore confirmation.
	exec("INSERT INTO nests (nest_id,polygon,pokemon_id,pokemon_form) VALUES (40,ST_GeomFromText('POLYGON((13 55,14 55,14 56,13 56,13 55))'),77,1011)")
	exec("INSERT INTO talonflame_nest_groups (nest_id,pokemon_id,pokemon_form,status) VALUES (40,77,1011,'confirmed')")
	exec("INSERT INTO talonflame_nest_sources (group_id,source_nest_id) VALUES (40,40)")
	a.groups[40] = &Group{ID: 40, Pair: Pair{77, 1011}, Confirmed: true, zone: time.UTC}
	a.knownGroups++
	for i := 0; i < 6; i++ {
		a.remember(Observation{Hour: hourBoundary(base, time.UTC), Encounter: uint64(20000 + i), Spawnpoint: uint64(20000 + i), Nest: 40, Pair: Pair{77, 1011}, Roll: 705410 + uint32(i)*280000, At: base.Unix()})
	}
	if err := a.Evaluate(ctx, base); err != nil {
		t.Fatal(err)
	}
	if a.groups[40].Confirmed {
		t.Fatal("old narrow-roll confirmation survived revalidation")
	}
	if err := admin.QueryRow("SELECT status FROM talonflame_nest_groups WHERE nest_id=40").Scan(&status); err != nil || status != "provisional" {
		t.Fatal("withdrawal not persisted", status, err)
	}
	if err := admin.QueryRow("SELECT pokemon_id FROM nests WHERE nest_id=40").Scan(&source); err != nil || source.Valid {
		t.Fatal("withdrawn source label still published", err)
	}
	for i := 0; i < 2; i++ {
		a.remember(Observation{Hour: hourBoundary(base, time.UTC), Encounter: uint64(30000 + i), Spawnpoint: uint64(30000 + i), Nest: 40, Pair: Pair{77, 1011}, Roll: uint32(i) * 3 << 20, At: base.Unix()})
	}
	if err := a.Evaluate(ctx, base); err != nil {
		t.Fatal(err)
	}
	if !a.groups[40].Confirmed {
		t.Fatal("broad new evidence failed to restore confirmation")
	}
	for i := 0; i < 2; i++ {
		a.remember(Observation{Hour: hourBoundary(base, time.UTC), Encounter: uint64(40000 + i), Spawnpoint: uint64(40000 + i), Nest: 40, Pair: Pair{63 + i, 0}, Roll: uint32(i) * 3 << 20, At: base.Unix()})
	}
	if err := a.Evaluate(ctx, base); err != nil {
		t.Fatal(err)
	}
	if !a.groups[40].Conflicting {
		t.Fatal("distributed contradictions did not retract old label")
	}
	if err := admin.QueryRow("SELECT pokemon_id FROM nests WHERE nest_id=40").Scan(&source); err != nil || source.Valid {
		t.Fatal("conflicting source label still published", err)
	}
	logger.SetOutput(io.Discard)
}
