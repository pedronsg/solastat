// Package history persists per-minute averages of live readings in a small
// SQLite database, and serves aggregated time series for the dashboard
// charts.
//
// Which readings are kept is described by a list of Series, one column each
// in the power_samples_1m table. Open adds the column of any series the
// table doesn't have yet, so recording a new reading (or letting the user
// choose which ones to record) never needs a migration by hand: pass it to
// Open. Series that aren't passed stay in the table, just no longer written.
// One column per series rather than one row per value keeps a year at ~525k
// rows whatever the number of series (a row per value would multiply that
// by the series count), which matters on a Pi Zero; SQLite stores the NULLs
// of an unused column almost for free.
//
// Values are stored as integers scaled by 10^Series.Decimals (W in 0.01 W,
// V in 0.1 V, A in 0.01 A): SQLite writes an integer in 1-3 bytes where a
// REAL always takes 8, which more than halves the database. They are scaled
// back on every read, so callers only ever see the reading's own unit.
//
// Every minute is kept forever. Next to it, power_samples_1h holds per UTC
// hour the sum, count (and min/max, positive sum) of each series, rebuilt
// from the hour's minutes on every write: the hour, day and year views and
// the energy totals read those instead of scanning ~525k minute rows a year.
//
// All timestamps stored in the DB are UTC unix epoch in *minutes*.
// Aggregation to local hour / local day for chart rendering is performed at
// query time using the server's current local timezone offset.
package history

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/OrbitOS-org/orbit-os-sdk-go/v26/logger"
	_ "modernc.org/sqlite"
)

const logTag = "history"

// schemaVersion is stored in the database's PRAGMA user_version.
const schemaVersion = 1

// Series is one recorded reading: a column of per-minute averages.
type Series struct {
	// Column is the SQL column name. It is also the key in the JSON buckets
	// the charts read, and the CSV header.
	Column string
	// Unit is the reading's unit ("W", "%", ...), for labels and exports.
	Unit string
	// Power marks a power in W, which integrates into energy (kWh).
	Power bool
	// Range also reports, for hour and day buckets, the lowest and highest
	// minute in each as <Column>_min and <Column>_max: a voltage's peaks
	// matter more than its hourly average.
	Range bool
	// Decimals is the precision kept: values are stored as integers scaled
	// by 10^Decimals, and exported with this many decimals.
	Decimals int
}

// scale is the factor between a value and its stored integer.
func (sr Series) scale() float64 {
	return math.Pow10(sr.Decimals)
}

// The four power series the dashboard charts and the energy totals are
// built on, and the battery state of charge.
var (
	// Sign conventions (the same as Totals): grid > 0 is bought from the
	// grid, < 0 sold to it; battery > 0 is discharging, < 0 charging.
	SeriesSolar      = Series{Column: "solar_w", Unit: "W", Power: true, Decimals: 2}
	SeriesLoad       = Series{Column: "load_w", Unit: "W", Power: true, Decimals: 2}
	SeriesGrid       = Series{Column: "grid_w", Unit: "W", Power: true, Decimals: 2}
	SeriesBattery    = Series{Column: "battery_w", Unit: "W", Power: true, Decimals: 2}
	SeriesBatterySoc = Series{Column: "battery_soc", Unit: "%", Decimals: 1}

	// AC (grid side) and DC (PV strings, battery) voltages and currents,
	// and the grid frequency. grid_v/grid_a are the only phase
	// (single-phase) or L1.
	SeriesGridV    = Series{Column: "grid_v", Unit: "V", Range: true, Decimals: 1}
	SeriesGridA    = Series{Column: "grid_a", Unit: "A", Range: true, Decimals: 2}
	SeriesGridVL2  = Series{Column: "grid_v_l2", Unit: "V", Range: true, Decimals: 1}
	SeriesGridAL2  = Series{Column: "grid_a_l2", Unit: "A", Range: true, Decimals: 2}
	SeriesGridVL3  = Series{Column: "grid_v_l3", Unit: "V", Range: true, Decimals: 1}
	SeriesGridAL3  = Series{Column: "grid_a_l3", Unit: "A", Range: true, Decimals: 2}
	SeriesGridF    = Series{Column: "grid_f", Unit: "Hz", Range: true, Decimals: 2}
	SeriesPV1V     = Series{Column: "pv1_v", Unit: "V", Range: true, Decimals: 1}
	SeriesPV1A     = Series{Column: "pv1_a", Unit: "A", Range: true, Decimals: 2}
	SeriesPV2V     = Series{Column: "pv2_v", Unit: "V", Range: true, Decimals: 1}
	SeriesPV2A     = Series{Column: "pv2_a", Unit: "A", Range: true, Decimals: 2}
	SeriesPV3V     = Series{Column: "pv3_v", Unit: "V", Range: true, Decimals: 1}
	SeriesPV3A     = Series{Column: "pv3_a", Unit: "A", Range: true, Decimals: 2}
	SeriesPV4V     = Series{Column: "pv4_v", Unit: "V", Range: true, Decimals: 1}
	SeriesPV4A     = Series{Column: "pv4_a", Unit: "A", Range: true, Decimals: 2}
	SeriesBatteryV = Series{Column: "battery_v", Unit: "V", Range: true, Decimals: 1}
	SeriesBatteryA = Series{Column: "battery_a", Unit: "A", Range: true, Decimals: 2}
)

// CoreSeries are always recorded: Totals and the charts depend on them.
// Open adds any the caller leaves out.
var CoreSeries = []Series{SeriesSolar, SeriesLoad, SeriesGrid, SeriesBattery}

// columnName keeps series columns safe to splice into SQL.
var columnName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// Sample is one live reading of every series, keyed by Series.Column.
// Missing keys, NaN and ±Inf are treated as "no value" and skipped.
type Sample map[string]float64

// Bucket is one point of an aggregated time series returned to the frontend.
// Values holds one entry per recorded series, keyed by Series.Column; a nil
// value is a gap the chart renders as such. It marshals flat, as
// {"ts": ..., "solar_w": ..., "battery_soc": ...}.
type Bucket struct {
	Ts     time.Time
	Values map[string]*float64
}

// MarshalJSON flattens Values next to "ts". Gaps (nil) are left out and
// values rounded to 2 decimals: a day view is 1440 buckets of every
// series, most of them empty on profiles without voltages and currents,
// so this keeps the response a fraction of the size (the chart treats a
// missing key as a gap).
func (b Bucket) MarshalJSON() ([]byte, error) {
	ts, err := b.Ts.MarshalJSON()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 32+16*len(b.Values))
	out = append(out, `{"ts":`...)
	out = append(out, ts...)
	for k, v := range b.Values {
		if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
			continue
		}
		out = append(out, ',', '"')
		out = append(out, k...) // a column name: [a-z0-9_] only
		out = append(out, '"', ':')
		out = strconv.AppendFloat(out, math.Round(*v*100)/100, 'f', -1, 64)
	}
	return append(out, '}'), nil
}

// Granularity selects the bucket size for a query.
type Granularity string

const (
	GranMinute Granularity = "minute"
	GranHour   Granularity = "hour"
	GranDay    Granularity = "day"
)

// Store wraps the sqlite database and the in-memory minute accumulator.
type Store struct {
	db     *sql.DB
	series []Series // recorded series, in column order

	// SQL built once from series by Open.
	upsertSQL, minuteSQL, groupSQL, rollupSQL, hourSQL, daySQL string

	mu  sync.Mutex
	cur *minuteAccum
}

// minuteAccum sums the samples of the minute in progress, per series (same
// index as Store.series).
type minuteAccum struct {
	minute int64
	sum    []float64
	count  []int64
}

// minuteRow is one stored minute, per series (same index as Store.series).
type minuteRow struct {
	minute int64
	vals   []sql.NullFloat64
	count  int64
}

// Open opens (or creates) the SQLite database at path, recording series
// (CoreSeries are added if missing). Columns the table lacks are added.
func Open(path string, series []Series) (*Store, error) {
	series, err := withCore(series)
	if err != nil {
		return nil, err
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open history db: %w", err)
	}
	db.SetMaxOpenConns(1)

	if err := ensureSchema(db, series); err != nil {
		_ = db.Close()
		return nil, err
	}

	s := &Store{db: db, series: series}
	s.buildSQL()
	logger.Infof(logTag, "history db ready at %s (%d series)", path, len(series))
	return s, nil
}

// Series returns the series this store records, in column order.
func (s *Store) Series() []Series {
	return append([]Series(nil), s.series...)
}

// withCore validates series and appends any CoreSeries missing from it.
func withCore(series []Series) ([]Series, error) {
	seen := map[string]bool{}
	out := make([]Series, 0, len(series)+len(CoreSeries))
	for _, sr := range series {
		if !columnName.MatchString(sr.Column) || sr.Column == "ts_minute" || sr.Column == "sample_count" {
			return nil, fmt.Errorf("invalid history series column %q", sr.Column)
		}
		if seen[sr.Column] {
			continue
		}
		seen[sr.Column] = true
		out = append(out, sr)
	}
	for _, sr := range CoreSeries {
		if !seen[sr.Column] {
			seen[sr.Column] = true
			out = append(out, sr)
		}
	}
	return out, nil
}

func ensureSchema(db *sql.DB, series []Series) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	// No index on ts_minute / ts_hour: as the INTEGER PRIMARY KEY each is
	// its table's own rowid, already ordered, so range queries use it
	// directly and an extra index would only double the writes.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS power_samples_1m (
			ts_minute     INTEGER PRIMARY KEY,
			solar_w       INTEGER,
			load_w        INTEGER,
			grid_w        INTEGER,
			battery_w     INTEGER,
			sample_count  INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS power_samples_1h (
			ts_hour       INTEGER PRIMARY KEY
		)`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
	}

	minuteCols, err := tableColumns(db, "power_samples_1m")
	if err != nil {
		return err
	}
	hourCols, err := tableColumns(db, "power_samples_1h")
	if err != nil {
		return err
	}
	for _, sr := range series {
		if !minuteCols[sr.Column] {
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE power_samples_1m ADD COLUMN %s INTEGER`, sr.Column)); err != nil {
				return fmt.Errorf("add history column %s: %w", sr.Column, err)
			}
			logger.Infof(logTag, "added history column %s", sr.Column)
		}
		for _, c := range hourColumns(sr) {
			if hourCols[c] {
				continue
			}
			if _, err := db.Exec(fmt.Sprintf(`ALTER TABLE power_samples_1h ADD COLUMN %s INTEGER`, c)); err != nil {
				return fmt.Errorf("add hourly column %s: %w", c, err)
			}
		}
	}

	if version < schemaVersion {
		if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
	}
	return nil
}

// hourColumns are the power_samples_1h columns of sr: the sum of its minute
// values and how many minutes had one, the lowest and highest minute for a
// Range series, and the sum of the positive minutes for a Power series (the
// negative ones are sum - pos), which the energy totals split on.
func hourColumns(sr Series) []string {
	c := sr.Column
	cols := []string{c + "_sum", c + "_n"}
	if sr.Range {
		cols = append(cols, c+"_min", c+"_max")
	}
	if sr.Power {
		cols = append(cols, c+"_pos")
	}
	return cols
}

func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, fmt.Errorf("read %s columns: %w", table, err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("scan %s column: %w", table, err)
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

// buildSQL prepares the statements that depend on the series list.
func (s *Store) buildSQL() {
	cols := make([]string, len(s.series))
	sets := make([]string, len(s.series))
	sums := make([]string, len(s.series))
	var hourCols, hourAggs, hourSets, hourSel, daySel []string
	for i, sr := range s.series {
		c := sr.Column
		cols[i] = c
		sets[i] = fmt.Sprintf("%s = excluded.%s", c, c)
		sums[i] = fmt.Sprintf("SUM(%s), COUNT(%s), MIN(%s), MAX(%s)", c, c, c, c)

		hourCols = append(hourCols, hourColumns(sr)...)
		hourAggs = append(hourAggs, fmt.Sprintf("SUM(%s), COUNT(%s)", c, c))
		sel := fmt.Sprintf("%s_sum, %s_n", c, c)
		day := fmt.Sprintf("SUM(%s_sum), SUM(%s_n)", c, c)
		if sr.Range {
			hourAggs = append(hourAggs, fmt.Sprintf("MIN(%s), MAX(%s)", c, c))
			sel += fmt.Sprintf(", %s_min, %s_max", c, c)
			day += fmt.Sprintf(", MIN(%s_min), MAX(%s_max)", c, c)
		} else {
			sel += ", NULL, NULL"
			day += ", NULL, NULL"
		}
		daySel = append(daySel, day)
		if sr.Power {
			hourAggs = append(hourAggs, fmt.Sprintf("SUM(CASE WHEN %s > 0 THEN %s ELSE 0 END)", c, c))
		}
		hourSel = append(hourSel, sel)
	}
	for _, c := range hourCols {
		hourSets = append(hourSets, fmt.Sprintf("%s = excluded.%s", c, c))
	}
	s.upsertSQL = fmt.Sprintf(`
		INSERT INTO power_samples_1m (ts_minute, %s, sample_count)
		VALUES (?%s, ?)
		ON CONFLICT(ts_minute) DO UPDATE SET
			%s,
			sample_count = excluded.sample_count`,
		strings.Join(cols, ", "), strings.Repeat(", ?", len(cols)), strings.Join(sets, ",\n\t\t\t"))
	s.minuteSQL = fmt.Sprintf(`
		SELECT ts_minute, %s, sample_count
		FROM power_samples_1m
		WHERE ts_minute >= ? AND ts_minute < ?
		ORDER BY ts_minute`, strings.Join(cols, ", "))
	s.groupSQL = fmt.Sprintf(`
		SELECT (ts_minute / ?) * ? AS g, %s
		FROM power_samples_1m
		WHERE ts_minute >= ? AND ts_minute < ?
		GROUP BY g
		ORDER BY g`, strings.Join(sums, ", "))
	// rollupSQL rebuilds one hour from its minutes (args: hour, first
	// minute, end minute). Rebuilding rather than adding the new minute
	// keeps the hour right when a minute is written twice (a restart within
	// the same minute) and costs at most 60 rows. "WHERE true" lets SQLite
	// parse the ON CONFLICT of an INSERT ... SELECT.
	s.rollupSQL = fmt.Sprintf(`
		INSERT INTO power_samples_1h (ts_hour, %s)
		SELECT ?, %s
		FROM power_samples_1m
		WHERE ts_minute >= ? AND ts_minute < ? AND true
		ON CONFLICT(ts_hour) DO UPDATE SET
			%s`,
		strings.Join(hourCols, ", "), strings.Join(hourAggs, ", "), strings.Join(hourSets, ",\n\t\t\t"))
	// hourSQL reads hours in the groupRow layout of groupSQL.
	s.hourSQL = fmt.Sprintf(`
		SELECT ts_hour * 60, %s
		FROM power_samples_1h
		WHERE ts_hour >= ? AND ts_hour < ?
		ORDER BY ts_hour`, strings.Join(hourSel, ", "))
	// daySQL sums hours into local days for one constant UTC offset (args:
	// offset in s, offset in s, first hour, end hour), each row starting at
	// its local midnight, in the groupRow layout: a year view gets ~365 rows
	// out of SQLite instead of ~8.8k.
	s.daySQL = fmt.Sprintf(`
		SELECT ((ts_hour * 3600 + ?) / 86400) * 1440 - ? / 60 AS d, %s
		FROM power_samples_1h
		WHERE ts_hour >= ? AND ts_hour < ?
		GROUP BY d
		ORDER BY d`, strings.Join(daySel, ", "))
}

// Close flushes the current minute (best effort) and closes the DB.
func (s *Store) Close() error {
	s.mu.Lock()
	if s.cur != nil {
		_ = s.flushLocked(s.cur)
		s.cur = nil
	}
	s.mu.Unlock()
	return s.db.Close()
}

// AddSample feeds a new live reading into the current minute accumulator.
// On a minute rollover the previous accumulator is flushed (upserted).
// The minute in progress is only written once it is complete (and on Close):
// writing it on every ~5 s reading cost CPU and SD-card writes all day, for
// at most one minute of data saved on a crash.
func (s *Store) AddSample(ts time.Time, sample Sample) {
	minute := ts.UTC().Unix() / 60

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cur == nil || s.cur.minute != minute {
		if s.cur != nil {
			if err := s.flushLocked(s.cur); err != nil {
				logger.Warnf(logTag, "flush previous minute %d: %v", s.cur.minute, err)
			}
		}
		s.cur = &minuteAccum{minute: minute, sum: make([]float64, len(s.series)), count: make([]int64, len(s.series))}
	}

	for i, sr := range s.series {
		if v, ok := sample[sr.Column]; ok && !math.IsNaN(v) && !math.IsInf(v, 0) {
			s.cur.sum[i] += v
			s.cur.count[i]++
		}
	}
}

func (s *Store) flushLocked(a *minuteAccum) error {
	if a == nil {
		return nil
	}
	var maxCount int64
	args := make([]interface{}, 0, len(s.series)+2)
	args = append(args, a.minute)
	for i, sr := range s.series {
		if a.count[i] > 0 {
			args = append(args, int64(math.Round(a.sum[i]/float64(a.count[i])*sr.scale())))
		} else {
			args = append(args, nil)
		}
		if a.count[i] > maxCount {
			maxCount = a.count[i]
		}
	}
	if maxCount == 0 {
		return nil
	}
	args = append(args, maxCount)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(s.upsertSQL, args...); err != nil {
		return err
	}
	if err := s.rollupHour(tx, a.minute/60); err != nil {
		return err
	}
	return tx.Commit()
}

// execer is a *sql.DB or *sql.Tx.
type execer interface {
	Exec(query string, args ...interface{}) (sql.Result, error)
}

// rollupHour rebuilds the power_samples_1h row of hour from its minutes.
func (s *Store) rollupHour(db execer, hour int64) error {
	_, err := db.Exec(s.rollupSQL, hour, hour*60, hour*60+60)
	return err
}

// Query returns aggregated time-series buckets over the half-open range
// [start, end). The granularity controls the bucket size:
//
//   - GranMinute: rows are returned 1:1 from the DB (1 bucket per minute).
//   - GranHour:   minute rows are grouped by *local* hour (loc).
//   - GranDay:    minute rows are grouped by *local* day (loc).
//
// Every series is averaged within its bucket. Missing buckets inside the
// range are filled with nil values so the frontend can render time-aware
// charts with gaps.
func (s *Store) Query(ctx context.Context, start, end time.Time, gran Granularity, loc *time.Location) ([]Bucket, error) {
	if loc == nil {
		loc = time.Local
	}
	if !start.Before(end) {
		return []Bucket{}, nil
	}

	startMin := start.UTC().Unix() / 60
	endMin := (end.UTC().Unix() + 59) / 60

	// Hour and day views read power_samples_1h, a day view summed into local
	// days by SQLite: a year is ~365 rows rather than ~525k minutes, which
	// matters on a Pi Zero. That needs the local hours to be UTC hours,
	// which holds for whole-hour UTC offsets and an hour-aligned range;
	// otherwise (India, Nepal, Newfoundland...) the minutes are grouped by
	// SQLite into 15-minute groups, which line up with every real offset.
	// Averages are the same either way (sum/count of the minutes).
	if gran == GranHour || gran == GranDay {
		var groups []groupRow
		var err error
		switch {
		case startMin%60 != 0 || endMin%60 != 0 || !wholeHourOffsets(loc, start, end):
			groups, err = s.queryGroups(ctx, s.groupSQL, groupMinutes, groupMinutes, startMin, endMin)
		case gran == GranDay:
			groups, err = s.queryDays(ctx, startMin/60, endMin/60, loc)
		default:
			groups, err = s.queryGroups(ctx, s.hourSQL, startMin/60, endMin/60)
		}
		if err != nil {
			return nil, err
		}
		return s.buildBucketedSeries(start, end, groups, loc, bucketSize(gran)), nil
	}
	if gran != GranMinute {
		return nil, fmt.Errorf("unsupported granularity %q", gran)
	}

	fetched, err := s.queryMinutes(ctx, startMin, endMin)
	if err != nil {
		return nil, err
	}
	return s.buildMinuteSeries(start, end, fetched, loc), nil
}

// queryMinutes returns the stored minute rows in [startMin, endMin).
func (s *Store) queryMinutes(ctx context.Context, startMin, endMin int64) ([]minuteRow, error) {
	rows, err := s.db.QueryContext(ctx, s.minuteSQL, startMin, endMin)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	var fetched []minuteRow
	for rows.Next() {
		m := minuteRow{vals: make([]sql.NullFloat64, len(s.series))}
		dest := make([]interface{}, 0, len(s.series)+2)
		dest = append(dest, &m.minute)
		for i := range m.vals {
			dest = append(dest, &m.vals[i])
		}
		dest = append(dest, &m.count)
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan history row: %w", err)
		}
		s.unscale(m.vals)
		fetched = append(fetched, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history rows: %w", err)
	}
	return fetched, nil
}

// groupMinutes is the size of the SQL pre-aggregation of minutes used by
// hour/day views when the hourly table can't be (see Query).
const groupMinutes = 15

// groupRow is one slice of history, groupMinutes wide or an hour: per series
// (same index as Store.series), the sum of the stored minute values, how
// many minutes had a value, and the lowest and highest of them.
type groupRow struct {
	minute   int64 // first minute of the group
	sum      []sql.NullFloat64
	n        []int64
	min, max []sql.NullFloat64
}

func bucketSize(gran Granularity) time.Duration {
	if gran == GranDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// wholeHourOffsets reports whether loc is a whole number of hours from UTC
// throughout [start, end]. Offsets are checked once a day: DST periods last
// months, so none is missed.
func wholeHourOffsets(loc *time.Location, start, end time.Time) bool {
	for t := start; ; t = t.Add(24 * time.Hour) {
		if !t.Before(end) {
			t = end
		}
		if _, off := t.In(loc).Zone(); off%3600 != 0 {
			return false
		}
		if t.Equal(end) {
			return true
		}
	}
}

// queryDays returns the local days of the hours [startHour, endHour), split
// where loc's UTC offset changes (DST) so each daySQL run has one offset. A
// day split that way comes back as two rows, which buildBucketedSeries adds
// into the same bucket.
func (s *Store) queryDays(ctx context.Context, startHour, endHour int64, loc *time.Location) ([]groupRow, error) {
	offset := func(h int64) int {
		_, off := time.Unix(h*3600, 0).In(loc).Zone()
		return off
	}
	var out []groupRow
	for from := startHour; from < endHour; {
		off := offset(from)
		// Next change: a day at a time, then the hour within that day.
		to := from
		for to < endHour && offset(to) == off {
			to += 24
		}
		if to > endHour {
			to = endHour
		}
		for to-1 > from && offset(to-1) != off {
			to--
		}
		rows, err := s.queryGroups(ctx, s.daySQL, off, off, from, to)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		from = to
	}
	return out, nil
}

// queryGroups runs groupSQL, hourSQL or daySQL and returns their rows, in the
// readings' own units.
func (s *Store) queryGroups(ctx context.Context, query string, args ...interface{}) ([]groupRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()
	var out []groupRow
	for rows.Next() {
		n := len(s.series)
		g := groupRow{sum: make([]sql.NullFloat64, n), n: make([]int64, n), min: make([]sql.NullFloat64, n), max: make([]sql.NullFloat64, n)}
		dest := make([]interface{}, 0, 1+4*n)
		dest = append(dest, &g.minute)
		for i := range s.series {
			dest = append(dest, &g.sum[i], &g.n[i], &g.min[i], &g.max[i])
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan history group: %w", err)
		}
		s.unscale(g.sum)
		s.unscale(g.min)
		s.unscale(g.max)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate history groups: %w", err)
	}
	return out, nil
}

// unscale turns stored integers (one per series, same index as
// Store.series) back into the readings' units, in place.
func (s *Store) unscale(vals []sql.NullFloat64) {
	for i, sr := range s.series {
		if sr.Decimals != 0 && vals[i].Valid {
			vals[i].Float64 /= sr.scale()
		}
	}
}

// LatestSampleTime returns the timestamp (local) of the most recent stored
// minute, or zero if the DB is empty.
func (s *Store) LatestSampleTime(ctx context.Context, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	var minute sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(ts_minute) FROM power_samples_1m`).Scan(&minute)
	if err != nil {
		return time.Time{}, err
	}
	if !minute.Valid {
		return time.Time{}, nil
	}
	return time.Unix(minute.Int64*60, 0).In(loc), nil
}

// emptyValues is a Values map with every series present and nil.
func (s *Store) emptyValues() map[string]*float64 {
	v := make(map[string]*float64, len(s.series))
	for _, sr := range s.series {
		v[sr.Column] = nil
	}
	return v
}

func (s *Store) buildMinuteSeries(start, end time.Time, rows []minuteRow, loc *time.Location) []Bucket {
	startMin := start.UTC().Unix() / 60
	endMin := end.UTC().Unix() / 60
	if endMin <= startMin {
		return []Bucket{}
	}
	totalMinutes := endMin - startMin

	const maxMinutes = 24 * 60 * 7
	if totalMinutes > maxMinutes {
		totalMinutes = maxMinutes
	}

	out := make([]Bucket, 0, totalMinutes)
	rowIdx := 0
	for i := int64(0); i < totalMinutes; i++ {
		min := startMin + i
		b := Bucket{Ts: time.Unix(min*60, 0).In(loc), Values: s.emptyValues()}
		if rowIdx < len(rows) && rows[rowIdx].minute == min {
			for j, sr := range s.series {
				b.Values[sr.Column] = nullToPtr(rows[rowIdx].vals[j])
			}
			rowIdx++
		}
		out = append(out, b)
	}
	return out
}

// maxChartBuckets bounds a chart query: a two-month hourly or one-year daily view.
const maxChartBuckets = 24*31*2 + 366

func (s *Store) buildBucketedSeries(start, end time.Time, rows []groupRow, loc *time.Location, bucketSize time.Duration) []Bucket {
	startLocal := start.In(loc)
	endLocal := end.In(loc)

	var alignStart time.Time
	switch bucketSize {
	case time.Hour:
		alignStart = time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), startLocal.Hour(), 0, 0, 0, loc)
	case 24 * time.Hour:
		alignStart = time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), 0, 0, 0, 0, loc)
	default:
		alignStart = startLocal
	}

	var bucketCount int
	switch bucketSize {
	case time.Hour:
		diff := endLocal.Sub(alignStart)
		bucketCount = int((diff + time.Hour - 1) / time.Hour)
	case 24 * time.Hour:
		y1, m1, d1 := alignStart.Date()
		y2, m2, d2 := endLocal.Date()
		t1 := time.Date(y1, m1, d1, 0, 0, 0, 0, loc)
		t2 := time.Date(y2, m2, d2, 0, 0, 0, 0, loc)
		bucketCount = int(t2.Sub(t1) / (24 * time.Hour))
	default:
		bucketCount = int(endLocal.Sub(alignStart) / bucketSize)
	}
	if bucketCount <= 0 {
		return []Bucket{}
	}
	if bucketCount > maxChartBuckets {
		bucketCount = maxChartBuckets
	}

	n := len(s.series)
	sums := make([]float64, bucketCount*n) // bucket i, series j at i*n+j
	counts := make([]int64, bucketCount*n)
	mins := make([]float64, bucketCount*n) // valid where counts > 0
	maxs := make([]float64, bucketCount*n)

	bucketIdx := func(ts time.Time) int {
		switch bucketSize {
		case time.Hour:
			diff := ts.In(loc).Sub(alignStart)
			return int(diff / time.Hour)
		case 24 * time.Hour:
			localTs := ts.In(loc)
			y1, m1, d1 := alignStart.Date()
			y2, m2, d2 := localTs.Date()
			t1 := time.Date(y1, m1, d1, 0, 0, 0, 0, loc)
			t2 := time.Date(y2, m2, d2, 0, 0, 0, 0, loc)
			return int(t2.Sub(t1) / (24 * time.Hour))
		}
		return 0
	}

	for _, r := range rows {
		idx := bucketIdx(time.Unix(r.minute*60, 0).In(loc))
		if idx < 0 || idx >= bucketCount {
			continue
		}
		for j := 0; j < n; j++ {
			k := idx*n + j
			if r.n[j] == 0 {
				continue
			}
			if counts[k] == 0 || r.min[j].Float64 < mins[k] {
				mins[k] = r.min[j].Float64
			}
			if counts[k] == 0 || r.max[j].Float64 > maxs[k] {
				maxs[k] = r.max[j].Float64
			}
			sums[k] += r.sum[j].Float64
			counts[k] += r.n[j]
		}
	}

	out := make([]Bucket, 0, bucketCount)
	for i := 0; i < bucketCount; i++ {
		var ts time.Time
		switch bucketSize {
		case time.Hour:
			ts = alignStart.Add(time.Duration(i) * time.Hour)
		case 24 * time.Hour:
			ts = alignStart.AddDate(0, 0, i)
		default:
			ts = alignStart.Add(time.Duration(i) * bucketSize)
		}
		b := Bucket{Ts: ts, Values: s.emptyValues()}
		for j, sr := range s.series {
			k := i*n + j
			if sr.Range {
				b.Values[sr.Column+"_min"], b.Values[sr.Column+"_max"] = nil, nil
			}
			if c := counts[k]; c > 0 {
				v := sums[k] / float64(c)
				b.Values[sr.Column] = &v
				if sr.Range {
					lo, hi := mins[k], maxs[k]
					b.Values[sr.Column+"_min"], b.Values[sr.Column+"_max"] = &lo, &hi
				}
			}
		}
		out = append(out, b)
	}
	return out
}

// Totals holds aggregate energy totals derived from the history database.
type Totals struct {
	SolarKWh       float64 `json:"solar_kwh"`
	LoadKWh        float64 `json:"load_kwh"`
	ImportKWh      float64 `json:"import_kwh"`
	ExportKWh      float64 `json:"export_kwh"`
	BChargedKWh    float64 `json:"bcharged_kwh"`
	BDischargedKWh float64 `json:"bdischarged_kwh"`
}

// QueryTotals returns aggregate energy totals over [start, end).
// Pass zero-value times to query all available data.
//
// Whole hours are read from power_samples_1h and only the minutes before
// the first and after the last whole hour from power_samples_1m, so a
// lifetime total costs the same in the tenth year as in the first.
func (s *Store) QueryTotals(ctx context.Context, start, end time.Time) (Totals, error) {
	// Per power series (solar, load, grid, battery): the sum of the
	// positive minutes and the sum of all of them, in stored units.
	var pos, sum [4]float64
	add := func(row *sql.Row) error {
		var v [8]sql.NullFloat64
		if err := row.Scan(&v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7]); err != nil {
			return fmt.Errorf("query totals: %w", err)
		}
		for i := 0; i < 4; i++ {
			pos[i] += v[2*i].Float64
			sum[i] += v[2*i+1].Float64
		}
		return nil
	}
	const hourQ = `
		SELECT SUM(solar_w_pos), SUM(solar_w_sum), SUM(load_w_pos), SUM(load_w_sum),
			SUM(grid_w_pos), SUM(grid_w_sum), SUM(battery_w_pos), SUM(battery_w_sum)
		FROM power_samples_1h`
	const minuteQ = `
		SELECT
			SUM(CASE WHEN solar_w > 0 THEN solar_w ELSE 0 END), SUM(solar_w),
			SUM(CASE WHEN load_w > 0 THEN load_w ELSE 0 END), SUM(load_w),
			SUM(CASE WHEN grid_w > 0 THEN grid_w ELSE 0 END), SUM(grid_w),
			SUM(CASE WHEN battery_w > 0 THEN battery_w ELSE 0 END), SUM(battery_w)
		FROM power_samples_1m
		WHERE ts_minute >= ? AND ts_minute < ?`

	if start.IsZero() || end.IsZero() {
		// Every stored minute is in its hour row.
		if err := add(s.db.QueryRowContext(ctx, hourQ)); err != nil {
			return Totals{}, err
		}
	} else {
		startMin, endMin := start.UTC().Unix()/60, end.UTC().Unix()/60
		firstHour, endHour := (startMin+59)/60, endMin/60
		if firstHour >= endHour {
			if err := add(s.db.QueryRowContext(ctx, minuteQ, startMin, endMin)); err != nil {
				return Totals{}, err
			}
		} else {
			// One at a time: the store has a single connection, which a
			// *sql.Row holds until it is scanned.
			parts := []struct {
				q          string
				from, upto int64
			}{
				{minuteQ, startMin, firstHour * 60},
				{hourQ + " WHERE ts_hour >= ? AND ts_hour < ?", firstHour, endHour},
				{minuteQ, endHour * 60, endMin},
			}
			for _, p := range parts {
				if err := add(s.db.QueryRowContext(ctx, p.q, p.from, p.upto)); err != nil {
					return Totals{}, err
				}
			}
		}
	}

	// W·min → kWh. Sign conventions: grid > 0 bought, battery > 0 discharging.
	kWh := func(v float64) float64 { return v / SeriesSolar.scale() / 60000 }
	return Totals{
		SolarKWh:       kWh(pos[0]),
		LoadKWh:        kWh(pos[1]),
		ImportKWh:      kWh(pos[2]),
		ExportKWh:      kWh(pos[2] - sum[2]),
		BChargedKWh:    kWh(pos[3] - sum[3]),
		BDischargedKWh: kWh(pos[3]),
	}, nil
}

func nullToPtr(n sql.NullFloat64) *float64 {
	if !n.Valid {
		return nil
	}
	v := n.Float64
	return &v
}
