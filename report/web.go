package main

import (
	"encoding/csv"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type reading struct {
	ts    time.Time
	mmolL float64
}

type cell struct {
	Value   string
	MmolL   float64
	Kind    string // "exact", "interp", "none"
	Minutes int    // delta in minutes to nearest reading (for exact)
	Style   template.CSS
}

// Style is the per-cell inline style state that filters build up.
type Style struct {
	Bg string
	Fg string
}

func (s Style) CSS() template.CSS {
	out := ""
	if s.Bg != "" {
		out += "background:" + s.Bg + ";"
	}
	if s.Fg != "" {
		out += "color:" + s.Fg + ";"
	}
	return template.CSS(out)
}

// A Filter is a pure (prev, cell, day) -> newStyle function. We reduce a list
// of Filters across each cell to produce its final Style — last writer wins
// unless the filter returns `prev` unchanged.
type Filter func(prev Style, c cell, day time.Time) Style

func compose(fs ...Filter) Filter {
	return func(prev Style, c cell, day time.Time) Style {
		for _, f := range fs {
			prev = f(prev, c, day)
		}
		return prev
	}
}

// from gates a filter so it only applies on days >= start.
func from(start time.Time, f Filter) Filter {
	return func(prev Style, c cell, day time.Time) Style {
		if day.Before(start) {
			return prev
		}
		return f(prev, c, day)
	}
}

// mmolBands: 4–7.9 green, 8–10 amber, anything else red.
func mmolBands(prev Style, c cell, _ time.Time) Style {
	v := c.MmolL
	if v <= 0 || c.Kind == "none" {
		return prev
	}
	switch {
	case v >= 4 && v < 8:
		return Style{Bg: "#c8ecc8"} // green
	case v >= 8 && v <= 10:
		return Style{Bg: "#ffe0a8"} // amber
	default:
		return Style{Bg: "#f4b4b4"} // red
	}
}

type row struct {
	Date  string
	Day   string
	Cells []cell
}

type week struct {
	Label string // e.g. "w/c 2026-04-13"
	Rows  []row  // Mon..Sun (only days within data range)
}

var slotHours = []int{8, 10, 12, 14, 16, 18, 21}
var slotLabels = []string{"08:00 wake", "10:00", "12:00", "14:00", "16:00", "18:00", "21:00"}

func loadReadings(path string, _ *time.Location) ([]reading, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := csv.NewReader(f)
	r.Comma = '\t'
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	var out []reading
	for i, row := range rows {
		if i == 0 {
			continue
		}
		if len(row) < 3 {
			continue
		}
		ts, err := time.ParseInLocation("2006-01-02 15:04:05", row[1], time.UTC)
		if err != nil {
			continue
		}
		mmol, err := strconv.ParseFloat(row[2], 64)
		if err != nil {
			continue
		}
		out = append(out, reading{ts: ts, mmolL: mmol})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ts.Before(out[j].ts) })
	return out, nil
}

func valueAt(readings []reading, target time.Time) cell {
	if len(readings) == 0 {
		return cell{Value: "", Kind: "none"}
	}
	// binary search for first reading >= target
	idx := sort.Search(len(readings), func(i int) bool {
		return !readings[i].ts.Before(target)
	})
	var before, after *reading
	if idx < len(readings) {
		r := readings[idx]
		after = &r
	}
	if idx-1 >= 0 {
		r := readings[idx-1]
		before = &r
	}

	// find closest
	var closest *reading
	var closestDelta time.Duration
	if before != nil {
		closest = before
		closestDelta = target.Sub(before.ts)
	}
	if after != nil {
		d := after.ts.Sub(target)
		if closest == nil || d < closestDelta {
			closest = after
			closestDelta = d
		}
	}

	if closest != nil && closestDelta <= 20*time.Minute {
		return cell{
			Value:   fmt.Sprintf("%.1f", closest.mmolL),
			MmolL:   closest.mmolL,
			Kind:    "exact",
			Minutes: int(closestDelta / time.Minute),
		}
	}

	// interpolate if we have both sides, within a reasonable window (same day, <= 8h span)
	if before != nil && after != nil {
		span := after.ts.Sub(before.ts)
		if span <= 8*time.Hour {
			t := float64(target.Sub(before.ts)) / float64(span)
			v := before.mmolL + (after.mmolL-before.mmolL)*t
			return cell{
				Value: fmt.Sprintf("%.1f", v),
				MmolL: v,
				Kind:  "interp",
			}
		}
	}

	return cell{Value: "", Kind: "none"}
}

func mondayOf(t time.Time) time.Time {
	// Go: Sunday=0, Monday=1, ... Saturday=6
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, -(wd - 1))
}

// colorPipeline is the ordered list of filters applied to every cell.
// Add filters here; they compose via foldl.
func colorPipeline(loc *time.Location) Filter {
	return compose(
		from(time.Date(2026, 4, 13, 0, 0, 0, 0, loc), mmolBands),
	)
}

func buildWeeks(readings []reading, loc *time.Location) []week {
	if len(readings) == 0 {
		return nil
	}
	first := readings[0].ts.In(loc)
	last := readings[len(readings)-1].ts.In(loc)
	firstDay := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, loc)
	lastDay := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, loc)

	pipeline := colorPipeline(loc)

	// start from Monday of the first data day
	weekStart := mondayOf(firstDay)
	var weeks []week
	for !weekStart.After(lastDay) {
		w := week{Label: "w/c " + weekStart.Format("2006-01-02")}
		for i := 0; i < 7; i++ {
			day := weekStart.AddDate(0, 0, i)
			if day.Before(firstDay) || day.After(lastDay) {
				continue
			}
			r := row{
				Date: day.Format("2006-01-02"),
				Day:  day.Format("Mon"),
			}
			for _, h := range slotHours {
				slot := time.Date(day.Year(), day.Month(), day.Day(), h, 0, 0, 0, loc)
				c := valueAt(readings, slot)
				c.Style = pipeline(Style{}, c, day).CSS()
				r.Cells = append(r.Cells, c)
			}
			w.Rows = append(w.Rows, r)
		}
		if len(w.Rows) > 0 {
			weeks = append(weeks, w)
		}
		weekStart = weekStart.AddDate(0, 0, 7)
	}
	return weeks
}

const tpl = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>glucose — 2hr blocks</title>
<style>
  body { font: 14px/1.4 -apple-system, system-ui, sans-serif; margin: 2em; color: #222; }
  h1 { font-size: 18px; margin: 0 0 .2em 0; }
  .meta { color: #888; margin-bottom: 1em; font-size: 12px; }
  table { border-collapse: collapse; }
  th, td { padding: 6px 10px; text-align: right; border-bottom: 1px solid #eee; min-width: 60px; }
  th { text-align: center; font-weight: 600; background: #fafafa; border-bottom: 2px solid #ddd; }
  th:first-child, td:first-child { text-align: left; }
  td.interp { font-style: italic; }
  td.none { color: #ddd; }
  td.exact { color: #111; }
  tr.week td { background: #222; color: #fff; text-align: left; font-weight: 600; font-size: 12px; padding: 4px 10px; }
  .legend { margin-top: 1em; font-size: 12px; color: #666; }
  .legend span { display: inline-block; padding: 2px 6px; margin-right: 8px; border-radius: 3px; }
</style>
</head>
<body>
<h1>glucose — 2hr blocks (Sydney)</h1>
<div class="meta">{{.Count}} readings · {{.First}} → {{.Last}}</div>
<table>
<thead>
<tr>
  <th>date</th>
  <th>day</th>
  {{range .Labels}}<th>{{.}}</th>{{end}}
</tr>
</thead>
<tbody>
{{range .Weeks}}
<tr class="week"><td colspan="{{$.ColCount}}">{{.Label}}</td></tr>
{{range .Rows}}
<tr>
  <td>{{.Date}}</td>
  <td>{{.Day}}</td>
  {{range .Cells}}
    <td class="{{.Kind}}" style="{{.Style}}">{{.Value}}</td>
  {{end}}
</tr>
{{end}}
{{end}}
</tbody>
</table>
<div class="legend">
  <span class="exact" style="background:#f4f4f4">exact (≤20min)</span>
  <span class="interp" style="background:#f4f4f4">interpolated</span>
</div>
</body>
</html>
`

func findTSV() (string, error) {
	candidates := []string{
		"../records.tsv",
		"records.tsv",
		"../../records.tsv",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs, nil
		}
	}
	return "", fmt.Errorf("records.tsv not found")
}

const port = 10943

func main() {
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		log.Fatalf("load tz: %v", err)
	}

	tsvPath, err := findTSV()
	if err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("reading %s", tsvPath)

	t := template.Must(template.New("r").Parse(tpl))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		readings, err := loadReadings(tsvPath, loc)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		weeks := buildWeeks(readings, loc)
		// newest week first, days inside each week stay Mon..Sun
		sort.Slice(weeks, func(i, j int) bool { return weeks[i].Label > weeks[j].Label })

		data := struct {
			Count    int
			First    string
			Last     string
			Labels   []string
			Weeks    []week
			ColCount int
		}{
			Count:    len(readings),
			Labels:   slotLabels,
			Weeks:    weeks,
			ColCount: 2 + len(slotLabels),
		}
		if len(readings) > 0 {
			data.First = readings[0].ts.In(loc).Format("2006-01-02 15:04")
			data.Last = readings[len(readings)-1].ts.In(loc).Format("2006-01-02 15:04")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := t.Execute(w, data); err != nil {
			log.Printf("template: %v", err)
		}
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("http://localhost:%d", port)
	log.Fatal(http.ListenAndServe(addr, nil))
}
