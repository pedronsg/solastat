package history

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "history.db"), []Series{SeriesBatterySoc})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// rebuildHours rolls up every hour that has minutes, for tests that insert
// minute rows directly instead of through AddSample.
func rebuildHours(t *testing.T, s *Store) {
	t.Helper()
	rows, err := s.db.Query(`SELECT DISTINCT ts_minute / 60 FROM power_samples_1m`)
	if err != nil {
		t.Fatal(err)
	}
	var hours []int64
	for rows.Next() {
		var h int64
		if err := rows.Scan(&h); err != nil {
			t.Fatal(err)
		}
		hours = append(hours, h)
	}
	rows.Close()
	for _, h := range hours {
		if err := s.rollupHour(s.db, h); err != nil {
			t.Fatal(err)
		}
	}
}

// insertRandomMinutes fills [start, end) with stored power minutes, some
// missing and some series NULL, and rolls the hours up.
func insertRandomMinutes(t *testing.T, s *Store, start, end time.Time) {
	t.Helper()
	rng := rand.New(rand.NewSource(1))
	tx, _ := s.db.Begin()
	for m := start.Unix() / 60; m < end.Unix()/60; m++ {
		if rng.Intn(10) == 0 {
			continue // missing minute
		}
		vals := make([]interface{}, 4)
		for i := range vals {
			if rng.Intn(20) == 0 {
				vals[i] = nil
			} else {
				vals[i] = rng.Int63n(5000) - 1000
			}
		}
		if _, err := tx.Exec(`INSERT INTO power_samples_1m (ts_minute, solar_w, load_w, grid_w, battery_w, sample_count) VALUES (?,?,?,?,?,12)`,
			m, vals[0], vals[1], vals[2], vals[3]); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	rebuildHours(t, s)
}

// referenceBuckets is the old algorithm: average every stored minute value
// that falls in each local bucket.
func referenceBuckets(t *testing.T, s *Store, start, end time.Time, loc *time.Location, size time.Duration) map[time.Time][4]*float64 {
	t.Helper()
	rows, err := s.db.Query(`SELECT ts_minute, solar_w, load_w, grid_w, battery_w FROM power_samples_1m
		WHERE ts_minute >= ? AND ts_minute < ?`, start.Unix()/60, (end.Unix()+59)/60)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type acc struct {
		sum [4]float64
		n   [4]int
	}
	accs := map[time.Time]*acc{}
	for rows.Next() {
		var m int64
		var v [4]sql.NullFloat64
		if err := rows.Scan(&m, &v[0], &v[1], &v[2], &v[3]); err != nil {
			t.Fatal(err)
		}
		ts := time.Unix(m*60, 0).In(loc)
		var key time.Time
		if size == time.Hour {
			// Hours are counted in absolute time from the start, like the
			// store does, so the repeated hour on a DST change stays separate.
			key = start.Add(ts.Sub(start).Truncate(time.Hour)).In(loc)
		} else {
			key = time.Date(ts.Year(), ts.Month(), ts.Day(), 0, 0, 0, 0, loc)
		}
		a := accs[key]
		if a == nil {
			a = &acc{}
			accs[key] = a
		}
		for i := range v {
			if v[i].Valid {
				a.sum[i] += v[i].Float64 / SeriesSolar.scale()
				a.n[i]++
			}
		}
	}
	out := map[time.Time][4]*float64{}
	for k, a := range accs {
		var r [4]*float64
		for i := range r {
			if a.n[i] > 0 {
				avg := a.sum[i] / float64(a.n[i])
				r[i] = &avg
			}
		}
		out[k] = r
	}
	return out
}

// Hour and day buckets, read from the hourly table (Lisbon, New York) or
// from 15-minute groups of minutes (Kolkata, +05:30), must equal the plain
// average of the minutes in each local bucket, across a DST change.
func TestGroupedQueryMatchesMinuteAverages(t *testing.T) {
	for _, zone := range []string{"Europe/Lisbon", "America/New_York", "Asia/Kolkata"} {
		t.Run(zone, func(t *testing.T) {
			s := openTestStore(t)
			loc, err := time.LoadLocation(zone)
			if err != nil {
				t.Skip("tzdata not available")
			}
			// Ten days around the European and US DST changes, with gaps
			// and NULL series.
			start := time.Date(2026, 10, 24, 0, 0, 0, 0, loc)
			end := time.Date(2026, 11, 3, 0, 0, 0, 0, loc)
			insertRandomMinutes(t, s, start, end)

			for _, c := range []struct {
				gran Granularity
				size time.Duration
			}{{GranHour, time.Hour}, {GranDay, 24 * time.Hour}} {
				got, err := s.Query(context.Background(), start, end, c.gran, loc)
				if err != nil {
					t.Fatal(err)
				}
				want := referenceBuckets(t, s, start, end, loc, c.size)
				if len(got) == 0 {
					t.Fatalf("%s: no buckets", c.gran)
				}
				for _, b := range got {
					w := want[b.Ts]
					for i, g := range [4]*float64{b.Values["solar_w"], b.Values["load_w"], b.Values["grid_w"], b.Values["battery_w"]} {
						switch {
						case g == nil && w[i] == nil:
						case g == nil || w[i] == nil:
							t.Fatalf("%s %v series %d: got %v want %v", c.gran, b.Ts, i, g, w[i])
						case math.Abs(*g-*w[i]) > 1e-6:
							t.Fatalf("%s %v series %d: got %.6f want %.6f", c.gran, b.Ts, i, *g, *w[i])
						}
					}
				}
			}
		})
	}
}

// Totals combine whole hours from the hourly table with the minutes at
// either end; they must equal summing the minutes directly.
func TestTotalsMatchMinutes(t *testing.T) {
	s := openTestStore(t)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	insertRandomMinutes(t, s, start, start.Add(5*time.Hour))

	minuteTotals := func(from, to time.Time) Totals {
		var tt Totals
		rows, err := s.db.Query(`SELECT solar_w, load_w, grid_w, battery_w FROM power_samples_1m WHERE ts_minute >= ? AND ts_minute < ?`,
			from.Unix()/60, to.Unix()/60)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var v [4]sql.NullFloat64
			if err := rows.Scan(&v[0], &v[1], &v[2], &v[3]); err != nil {
				t.Fatal(err)
			}
			for i := range v {
				v[i].Float64 /= SeriesSolar.scale()
			}
			tt.SolarKWh += math.Max(v[0].Float64, 0) / 60000
			tt.LoadKWh += math.Max(v[1].Float64, 0) / 60000
			tt.ImportKWh += math.Max(v[2].Float64, 0) / 60000
			tt.ExportKWh += math.Max(-v[2].Float64, 0) / 60000
			tt.BDischargedKWh += math.Max(v[3].Float64, 0) / 60000
			tt.BChargedKWh += math.Max(-v[3].Float64, 0) / 60000
		}
		return tt
	}
	near := func(a, b Totals) bool {
		x := [6]float64{a.SolarKWh, a.LoadKWh, a.ImportKWh, a.ExportKWh, a.BChargedKWh, a.BDischargedKWh}
		y := [6]float64{b.SolarKWh, b.LoadKWh, b.ImportKWh, b.ExportKWh, b.BChargedKWh, b.BDischargedKWh}
		for i := range x {
			if math.Abs(x[i]-y[i]) > 1e-9 {
				return false
			}
		}
		return true
	}
	for _, r := range [][2]time.Duration{
		{0, 5 * time.Hour}, // whole hours only
		{17 * time.Minute, 4*time.Hour + 3*time.Minute}, // minutes at both ends
		{70 * time.Minute, 100 * time.Minute},           // within one hour
		{2 * time.Hour, 2 * time.Hour},                  // empty
	} {
		from, to := start.Add(r[0]), start.Add(r[1])
		got, err := s.QueryTotals(context.Background(), from, to)
		if err != nil {
			t.Fatal(err)
		}
		if want := minuteTotals(from, to); !near(got, want) {
			t.Fatalf("totals %v-%v = %+v, want %+v", r[0], r[1], got, want)
		}
	}
	all, err := s.QueryTotals(context.Background(), time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if want := minuteTotals(start, start.Add(5*time.Hour)); !near(all, want) {
		t.Fatalf("lifetime totals = %+v, want %+v", all, want)
	}
}

// A minute written twice (a restart within the same minute) replaces the
// first in its hour rather than counting twice.
func TestRewrittenMinuteIsNotCountedTwice(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	s.AddSample(base, Sample{"solar_w": 1000})
	s.AddSample(base.Add(time.Minute), Sample{"solar_w": 3000})
	s.AddSample(base, Sample{"solar_w": 2000}) // the minute again: rewritten on the next rollover
	s.AddSample(base.Add(2*time.Minute), Sample{"solar_w": 0})

	var sum, n int64
	if err := s.db.QueryRow(`SELECT solar_w_sum, solar_w_n FROM power_samples_1h WHERE ts_hour = ?`, base.Unix()/3600).Scan(&sum, &n); err != nil {
		t.Fatal(err)
	}
	if sum != 500000 || n != 2 {
		t.Fatalf("hour = sum %d over %d minutes, want 500000 (5000 W) over 2", sum, n)
	}
}

// Values are stored as integers at their series' precision and read back
// in their own unit.
func TestValuesAreStoredScaled(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.db"), []Series{SeriesBatterySoc, SeriesGridV, SeriesGridA})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	s.AddSample(base, Sample{"solar_w": 1234.6, "battery_soc": 87.3, "grid_v": 231.44, "grid_a": 5.326})
	s.AddSample(base.Add(time.Minute), Sample{"solar_w": 0})

	var solar, soc, v, a interface{}
	if err := s.db.QueryRow(`SELECT solar_w, battery_soc, grid_v, grid_a FROM power_samples_1m WHERE ts_minute = ?`, base.Unix()/60).Scan(&solar, &soc, &v, &a); err != nil {
		t.Fatal(err)
	}
	if solar != int64(123460) || soc != int64(873) || v != int64(2314) || a != int64(533) {
		t.Fatalf("stored = %#v %#v %#v %#v, want integers 123460 873 2314 533", solar, soc, v, a)
	}
	got, err := s.Query(context.Background(), base, base.Add(time.Minute), GranMinute, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	vals := got[0].Values
	if *vals["solar_w"] != 1234.6 || *vals["battery_soc"] != 87.3 || *vals["grid_v"] != 231.4 || *vals["grid_a"] != 5.33 {
		t.Fatalf("read back = %v %v %v %v", *vals["solar_w"], *vals["battery_soc"], *vals["grid_v"], *vals["grid_a"])
	}
	hours, err := s.Query(context.Background(), base, base.Add(time.Hour), GranHour, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if hv := hours[0].Values; *hv["grid_v_max"] != 231.4 || *hv["grid_a"] != 5.33 {
		t.Fatalf("hour = grid_v_max %v grid_a %v, want 231.4 and 5.33", *hv["grid_v_max"], *hv["grid_a"])
	}
}

func TestAddSampleWritesCompletedMinutes(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	count := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM power_samples_1m`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	for i := 0; i < 12; i++ {
		s.AddSample(base.Add(time.Duration(i)*5*time.Second), Sample{"solar_w": 1000, "load_w": 500, "grid_w": 0, "battery_w": 200})
	}
	if n := count(); n != 0 {
		t.Fatalf("minute in progress should not be written yet, got %d rows", n)
	}
	s.AddSample(base.Add(time.Minute), Sample{"solar_w": 900, "load_w": 500, "grid_w": 0, "battery_w": 100})
	if n := count(); n != 1 {
		t.Fatalf("completed minute should be written on rollover, got %d rows", n)
	}
	var solar float64
	var samples int
	if err := s.db.QueryRow(`SELECT solar_w, sample_count FROM power_samples_1m WHERE ts_minute = ?`, base.Unix()/60).Scan(&solar, &samples); err != nil {
		t.Fatal(err)
	}
	if solar != 100000 || samples != 12 {
		t.Fatalf("minute row = %v over %d samples, want 100000 (1000 W) over 12", solar, samples)
	}
}

func TestOpenAddsNewSeriesToAnExistingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	// A database with only the four power columns: a series added later
	// (like battery_soc here) must get its column without losing rows.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE power_samples_1m (ts_minute INTEGER PRIMARY KEY, solar_w REAL, load_w REAL,
		grid_w REAL, battery_w REAL, sample_count INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO power_samples_1m VALUES (1000, 1500, 400, -300, 800, 12)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	temp := Series{Column: "battery_temp_c", Unit: "°C"}
	s, err := Open(path, []Series{SeriesBatterySoc, temp})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	cols, err := tableColumns(s.db, "power_samples_1m")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"solar_w", "battery_soc", "battery_temp_c"} {
		if !cols[c] {
			t.Fatalf("column %s missing after Open", c)
		}
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("user_version = %d (%v), want %d", version, err, schemaVersion)
	}
	var solar float64
	var soc sql.NullFloat64
	if err := s.db.QueryRow(`SELECT solar_w, battery_soc FROM power_samples_1m WHERE ts_minute = 1000`).Scan(&solar, &soc); err != nil {
		t.Fatal(err)
	}
	if solar != 1500 || soc.Valid {
		t.Fatalf("old row = solar %v soc %v, want 1500 and no SOC", solar, soc)
	}
}

func TestExtraSeriesAreRecordedAndQueried(t *testing.T) {
	s := openTestStore(t)
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	for i, soc := range []float64{80, 82} {
		s.AddSample(base.Add(time.Duration(i)*30*time.Second), Sample{"solar_w": 1000, "battery_soc": soc})
	}
	s.AddSample(base.Add(time.Minute), Sample{"solar_w": 1000}) // rolls the minute over

	got, err := s.Query(context.Background(), base, base.Add(time.Minute), GranMinute, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Values["battery_soc"] == nil || *got[0].Values["battery_soc"] != 81 {
		t.Fatalf("battery_soc = %v, want the minute average 81", got[0].Values["battery_soc"])
	}
	if got[0].Values["grid_w"] != nil {
		t.Fatal("a series with no samples should be a gap, not 0")
	}

	b, err := json.Marshal(got[0])
	if err != nil {
		t.Fatal(err)
	}
	var flat map[string]interface{}
	if err := json.Unmarshal(b, &flat); err != nil {
		t.Fatal(err)
	}
	if flat["solar_w"] != 1000.0 || flat["battery_soc"] != 81.0 || flat["ts"] == nil {
		t.Fatalf("bucket JSON should stay flat for the charts, got %s", b)
	}
	if _, ok := flat["grid_w"]; ok {
		t.Fatalf("a gap should be left out of the JSON, got %s", b)
	}
	v := 231.23456
	b, _ = json.Marshal(Bucket{Ts: base, Values: map[string]*float64{"grid_v": &v}})
	if !strings.Contains(string(b), `"grid_v":231.23`) {
		t.Fatalf("values should be rounded to 2 decimals, got %s", b)
	}
}

func TestOpenRejectsUnsafeColumnNames(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "h.db"), []Series{{Column: "x; DROP TABLE power_samples_1m"}})
	if err == nil {
		t.Fatal("expected an error for an unsafe column name")
	}
}

// Range queries must use the INTEGER PRIMARY KEY (the rowid), with no
// extra index on ts_minute to write on every minute.
func TestNoRedundantIndexAndRangeQueriesUseTheKey(t *testing.T) {
	s := openTestStore(t)
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND tbl_name IN ('power_samples_1m', 'power_samples_1h')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("history tables have %d index(es), want none", n)
	}
	for _, q := range []string{s.minuteSQL, s.groupSQL, s.hourSQL} {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN "+q, groupArgs(q)...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		joined := strings.Join(plan, "; ")
		if !strings.Contains(joined, "INTEGER PRIMARY KEY") {
			t.Fatalf("query plan %q doesn't use the primary key range", joined)
		}
	}
}

// groupArgs returns placeholder values for minuteSQL/hourSQL (2 args) or groupSQL (4 args).
func groupArgs(q string) []interface{} {
	if strings.Count(q, "?") == 4 {
		return []interface{}{15, 15, 0, 1000}
	}
	return []interface{}{0, 1000}
}

func TestRangeSeriesReportMinAndMaxPerBucket(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.db"), []Series{SeriesGridV})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	// Minutes 0..3 of the hour (and one in the next, to roll over): a peak
	// of 252 V in an hour averaging 236 V.
	for i, v := range []float64{230, 252, 228, 234, 231} {
		s.AddSample(base.Add(time.Duration(i)*time.Minute), Sample{"solar_w": 0, "grid_v": v})
	}
	s.AddSample(base.Add(time.Hour), Sample{"solar_w": 0})

	got, err := s.Query(context.Background(), base, base.Add(2*time.Hour), GranHour, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d buckets, want 2", len(got))
	}
	v := got[0].Values
	if v["grid_v"] == nil || *v["grid_v"] != 235 || v["grid_v_min"] == nil || *v["grid_v_min"] != 228 || v["grid_v_max"] == nil || *v["grid_v_max"] != 252 {
		t.Fatalf("hour bucket = avg %v min %v max %v, want 235 / 228 / 252", v["grid_v"], v["grid_v_min"], v["grid_v_max"])
	}
	if got[1].Values["grid_v_min"] != nil || got[1].Values["grid_v_max"] != nil {
		t.Fatal("an hour without readings should have no min/max")
	}
	if _, ok := got[0].Values["solar_w_min"]; ok {
		t.Fatal("power series shouldn't get min/max")
	}
}
