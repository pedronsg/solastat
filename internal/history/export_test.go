package history

import (
	"bytes"
	"context"
	"encoding/csv"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// insertMinute stores one minute row directly (solar, grid in W; soc in %).
func insertMinute(t *testing.T, s *Store, ts time.Time, solar, grid, soc float64) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO power_samples_1m (ts_minute, solar_w, load_w, grid_w, battery_w, battery_soc, sample_count)
		VALUES (?, ?, NULL, ?, NULL, ?, 12)`, ts.Unix()/60, int64(solar*100), int64(grid*100), int64(soc*10)); err != nil {
		t.Fatal(err)
	}
}

func readCSV(t *testing.T, b []byte, comma rune) [][]string {
	t.Helper()
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(b, []byte("\xEF\xBB\xBF"))))
	r.Comma = comma
	recs, err := r.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestCSVIsTheRecordedMinutes(t *testing.T) {
	s := openTestStore(t)
	loc, _ := time.LoadLocation("Europe/Lisbon")
	base := time.Date(2026, 9, 29, 14, 5, 0, 0, loc)
	insertMinute(t, s, base, 1500, -300, 81)
	insertMinute(t, s, base.Add(time.Minute), 1490, -280, 81)
	insertMinute(t, s, base.Add(3*time.Hour), 0, 400, 60) // a gap before it: no rows in between

	var buf bytes.Buffer
	if err := s.WriteCSV(context.Background(), &buf, base.Add(-24*time.Hour), base.Add(24*time.Hour), loc, CSVStandard); err != nil {
		t.Fatal(err)
	}
	recs := readCSV(t, buf.Bytes(), ',')
	want := []string{"time", "battery_soc", "solar_w", "load_w", "grid_w", "battery_w"}
	if strings.Join(recs[0], ",") != strings.Join(want, ",") {
		t.Fatalf("header = %v, want %v", recs[0], want)
	}
	if len(recs) != 4 {
		t.Fatalf("got %d rows, want header + the 3 stored minutes (nothing for the gap)", len(recs))
	}
	row := recs[1]
	if row[0] != "2026-09-29T14:05:00+01:00" || row[1] != "81.0" || row[2] != "1500.00" || row[3] != "" || row[4] != "-300.00" {
		t.Fatalf("row = %v (load has no value: blank, not 0)", row)
	}
	if recs[2][0] != "2026-09-29T14:06:00+01:00" || recs[3][0] != "2026-09-29T17:05:00+01:00" {
		t.Fatalf("times = %s, %s", recs[2][0], recs[3][0])
	}
}

func TestCSVExcelFormat(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "history.db"), []Series{SeriesBatterySoc, SeriesGridV, SeriesGridA})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	loc, _ := time.LoadLocation("Europe/Lisbon")
	day := time.Date(2026, 9, 29, 0, 0, 0, 0, loc)
	// Stored integers: 2000.25 W, 90.5 %, 230.5 V, 5.25 A.
	if _, err := s.db.Exec(`INSERT INTO power_samples_1m (ts_minute, solar_w, battery_soc, grid_v, grid_a, sample_count)
		VALUES (?, 200025, 905, 2305, 525, 12)`, day.Add(12*time.Hour).Unix()/60); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.WriteCSV(context.Background(), &buf, day, day.AddDate(0, 0, 1), loc, CSVExcel); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(buf.Bytes(), []byte("\xEF\xBB\xBF")) {
		t.Fatal("Excel CSV should start with a UTF-8 BOM")
	}
	if !bytes.Contains(buf.Bytes(), []byte(`;"230,5";`)) {
		t.Fatalf("decimal-comma numbers should be quoted, got %s", buf.Bytes())
	}
	recs := readCSV(t, buf.Bytes(), ';')
	col := map[string]int{}
	for i, h := range recs[0] {
		col[h] = i
	}
	row := recs[1]
	if row[col["time"]] != "29/09/2026 12:00" || row[col["utc_offset"]] != "+01:00" {
		t.Fatalf("time = %q offset = %q", row[col["time"]], row[col["utc_offset"]])
	}
	if row[col["solar_w"]] != "2000,25" || row[col["battery_soc"]] != "90,5" || row[col["grid_v"]] != "230,5" || row[col["grid_a"]] != "5,25" {
		t.Fatalf("solar = %q, soc = %q, V = %q, A = %q: want each at its precision, decimal commas",
			row[col["solar_w"]], row[col["battery_soc"]], row[col["grid_v"]], row[col["grid_a"]])
	}
}
