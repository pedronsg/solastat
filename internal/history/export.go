package history

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// MaxExportDays bounds one export: a year at minute resolution is ~525k rows
// (~25 MB of CSV), which the Pi Zero streams fine; more is refused.
const MaxExportDays = 366

// Export calls fn for every stored minute in [start, end), in time order,
// exactly as recorded: the history's own resolution, one row per minute that
// has data. Rows are streamed from the database (a year is too many to hold).
func (s *Store) Export(ctx context.Context, start, end time.Time, loc *time.Location, fn func(Bucket) error) error {
	if loc == nil {
		loc = time.Local
	}
	if !start.Before(end) {
		return nil
	}
	return s.streamMinutes(ctx, start.UTC().Unix()/60, (end.UTC().Unix()+59)/60, loc, fn)
}

// streamMinutes calls fn for each stored minute in [startMin, endMin),
// reading rows one at a time.
func (s *Store) streamMinutes(ctx context.Context, startMin, endMin int64, loc *time.Location, fn func(Bucket) error) error {
	rows, err := s.db.QueryContext(ctx, s.minuteSQL, startMin, endMin)
	if err != nil {
		return fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	var (
		minute int64
		count  int64
		vals   = make([]sql.NullFloat64, len(s.series))
		dest   = make([]interface{}, 0, len(s.series)+2)
	)
	dest = append(dest, &minute)
	for i := range vals {
		dest = append(dest, &vals[i])
	}
	dest = append(dest, &count)

	for rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return fmt.Errorf("scan history row: %w", err)
		}
		s.unscale(vals)
		b := Bucket{Ts: time.Unix(minute*60, 0).In(loc), Values: make(map[string]*float64, len(s.series))}
		for i, sr := range s.series {
			b.Values[sr.Column] = nullToPtr(vals[i])
		}
		if err := fn(b); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CSVFormat selects how numbers and fields are written.
type CSVFormat string

const (
	// CSVStandard: comma separator, decimal point, ISO 8601 times with the
	// UTC offset ("2026-09-29T14:05:00+01:00") — for scripts, Home
	// Assistant, spreadsheets set to English.
	CSVStandard CSVFormat = "standard"
	// CSVExcel: semicolon separator, decimal comma, "29/09/2026 14:05" (day
	// first) plus a utc_offset column, and a UTF-8 BOM — what Excel set to a
	// comma-decimal language (pt, es, de, it, fr) opens directly. Numbers are
	// quoted ("1363,2") so programs that also split on commas (LibreOffice
	// with both separators ticked) don't cut them in two.
	CSVExcel CSVFormat = "excel"
)

// WriteCSV writes the history in [start, end) as CSV, as recorded: a time
// column and one column per recorded series (the minute's average; blank
// where a series has no value, never 0).
func (s *Store) WriteCSV(ctx context.Context, w io.Writer, start, end time.Time, loc *time.Location, format CSVFormat) error {
	bw := bufio.NewWriterSize(w, 32*1024)
	excel := format == CSVExcel
	if excel {
		if _, err := bw.WriteString("\xEF\xBB\xBF"); err != nil {
			return err
		}
	}
	cw := csv.NewWriter(bw)
	sep := ","
	if excel {
		sep = ";"
	}
	// write puts one record: encoding/csv for the standard format; for
	// Excel, fields holding a comma are quoted too (csv.Writer only quotes
	// its own separator).
	write := func(rec []string) error {
		if !excel {
			return cw.Write(rec)
		}
		for i, f := range rec {
			if strings.ContainsAny(f, ",;\"\r\n") {
				f = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
			}
			if i > 0 {
				if _, err := bw.WriteString(sep); err != nil {
					return err
				}
			}
			if _, err := bw.WriteString(f); err != nil {
				return err
			}
		}
		_, err := bw.WriteString("\r\n")
		return err
	}

	header := []string{"time"}
	if excel {
		header = append(header, "utc_offset")
	}
	for _, sr := range s.series {
		header = append(header, sr.Column)
	}
	if err := write(header); err != nil {
		return err
	}

	num := func(v float64, decimals int) string {
		out := strconv.FormatFloat(v, 'f', decimals, 64)
		if excel {
			out = strings.Replace(out, ".", ",", 1)
		}
		return out
	}
	timeLayout := "2006-01-02T15:04:05Z07:00"
	if excel {
		timeLayout = "02/01/2006 15:04"
	}

	record := make([]string, 0, len(header))
	err := s.Export(ctx, start, end, loc, func(b Bucket) error {
		record = record[:0]
		record = append(record, b.Ts.Format(timeLayout))
		if excel {
			record = append(record, b.Ts.Format("-07:00"))
		}
		for _, sr := range s.series {
			if v := b.Values[sr.Column]; v != nil {
				record = append(record, num(*v, sr.Decimals))
			} else {
				record = append(record, "")
			}
		}
		return write(record)
	})
	if err != nil {
		return err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return err
	}
	return bw.Flush()
}
