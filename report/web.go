package main

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type reading struct {
	ts    time.Time
	mmolL float64
}

type cell struct {
	Value   string
	MmolL   float64
	Kind    string
	Minutes int
	Style   template.CSS
}

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

type Filter func(prev Style, c cell, day time.Time) Style

func compose(fs ...Filter) Filter {
	return func(prev Style, c cell, day time.Time) Style {
		for _, f := range fs {
			prev = f(prev, c, day)
		}
		return prev
	}
}

func from(start time.Time, f Filter) Filter {
	return func(prev Style, c cell, day time.Time) Style {
		if day.Before(start) {
			return prev
		}
		return f(prev, c, day)
	}
}

func mmolBands(prev Style, c cell, _ time.Time) Style {
	v := c.MmolL
	if v <= 0 || c.Kind == "none" {
		return prev
	}
	switch {
	case v >= 4 && v < 8:
		return Style{Bg: "#c8ecc8"}
	case v >= 8 && v <= 10:
		return Style{Bg: "#ffe0a8"}
	default:
		return Style{Bg: "#f4b4b4"}
	}
}

type row struct {
	Date  string
	Day   string
	Cells []cell
	Note  string
}

type week struct {
	Label   string
	Rows    []row
	HbA1c   string
	EAG     string
	Damage  string
	year    int
	weekNum int
}

var slotHours = []int{8, 10, 12, 14, 16, 18, 21}
var slotLabels = []string{"08:00 wake", "10:00", "12:00", "14:00", "16:00", "18:00", "21:00"}

func loadReadings(path string) ([]reading, error) {
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
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, -(wd - 1))
}

func colorPipeline(loc *time.Location) Filter {
	return compose(
		from(time.Date(2026, 4, 13, 0, 0, 0, 0, loc), mmolBands),
	)
}

func eagToHbA1c(eag float64) float64 {
	return (eag*18.0182 + 46.7) / 28.7
}

func damageIndex(hba1c float64) float64 {
	return math.Pow(hba1c/6, 5) * 10
}

func fmtDamage(h float64) string {
	d := damageIndex(h)
	return strconv.Itoa(int(math.Round(d)))
}

var excludedDays = map[string]bool{
	"2026-04-13": true,
}

func weekAverageMmol(readings []reading, loc *time.Location, year, wk int) (float64, int) {
	var sum float64
	var n int
	for _, r := range readings {
		t := r.ts.In(loc)
		y, w := t.ISOWeek()
		if y != year || w != wk {
			continue
		}
		if excludedDays[t.Format("2006-01-02")] {
			continue
		}
		sum += r.mmolL
		n++
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), n
}

func buildWeeks(readings []reading, loc *time.Location, notes map[string]string) []week {
	if len(readings) == 0 {
		return nil
	}
	first := readings[0].ts.In(loc)
	last := readings[len(readings)-1].ts.In(loc)
	firstDay := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, loc)
	lastDay := time.Date(last.Year(), last.Month(), last.Day(), 0, 0, 0, 0, loc)

	pipeline := colorPipeline(loc)

	weekStart := mondayOf(firstDay)
	var weeks []week
	for !weekStart.After(lastDay) {
		yr, wn := weekStart.ISOWeek()
		w := week{
			Label:   fmt.Sprintf("Week %d", wn),
			year:    yr,
			weekNum: wn,
		}
		for i := 0; i < 7; i++ {
			day := weekStart.AddDate(0, 0, i)
			if day.Before(firstDay) || day.After(lastDay) {
				continue
			}
			dateStr := day.Format("2006-01-02")
			r := row{
				Date: dateStr,
				Day:  day.Format("Mon"),
				Note: notes[dateStr],
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
			eag, n := weekAverageMmol(readings, loc, yr, wn)
			if n > 0 {
				hb := eagToHbA1c(eag)
				w.HbA1c = fmt.Sprintf("%.1f", hb)
				w.EAG = fmt.Sprintf("%.1f", eag)
				w.Damage = fmtDamage(hb)
			}
			weeks = append(weeks, w)
		}
		weekStart = weekStart.AddDate(0, 0, 7)
	}
	return weeks
}

func loadNotes(path string) (map[string]string, error) {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
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
	for i, row := range rows {
		if i == 0 {
			continue
		}
		if len(row) < 2 {
			continue
		}
		out[row[0]] = row[1]
	}
	return out, nil
}

func saveNotes(path string, notes map[string]string) error {
	dates := make([]string, 0, len(notes))
	for d := range notes {
		dates = append(dates, d)
	}
	sort.Strings(dates)

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := csv.NewWriter(f)
	w.Comma = '\t'
	if err := w.Write([]string{"date", "note"}); err != nil {
		f.Close()
		return err
	}
	for _, d := range dates {
		if notes[d] == "" {
			continue
		}
		if err := w.Write([]string{d, notes[d]}); err != nil {
			f.Close()
			return err
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadScratch(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func saveScratch(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var hba1cRe = regexp.MustCompile(`(?i)hba1c\s*([0-9.]+)\s*%?`)

func parseGoalHbA1c(scratch string, fallback float64) float64 {
	m := hba1cRe.FindStringSubmatch(scratch)
	if len(m) < 2 {
		return fallback
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return fallback
	}
	return v
}

func findFile(names ...string) (string, error) {
	dirs := []string{"..", ".", "../.."}
	for _, name := range names {
		for _, d := range dirs {
			p := filepath.Join(d, name)
			if _, err := os.Stat(p); err == nil {
				abs, _ := filepath.Abs(p)
				return abs, nil
			}
		}
	}
	return "", fmt.Errorf("not found: %v", names)
}

type pagePayload struct {
	Count     int
	Labels    []string
	Weeks     []week
	ColCount  int
	Scratch   string
	WeekHbA1c template.JS
	GoalHbA1c template.JS
}

const tpl = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>Glucose Tracker</title>
<style>
  body { font: 14px/1.4 -apple-system, system-ui, sans-serif; margin: 2em; color: #222; }
  h1 { font-size: 18px; margin: 0 0 .2em 0; }
  .meta { color: #888; margin-bottom: 1em; font-size: 12px; }
  table { border-collapse: collapse; }
  th, td { padding: 6px 10px; text-align: right; border-bottom: 1px solid #eee; min-width: 60px; }
  th { text-align: left; font-weight: 600; background: #fafafa; border-bottom: 2px solid #ddd; }
  th:first-child, td:first-child { text-align: left; }
  td.interp { font-style: italic; }
  td.none { color: #ddd; }
  td.exact { color: #111; }
  tr.week td { background: #222; color: #fff; text-align: left; font-weight: 600; font-size: 12px; padding: 4px 10px; }
  td.note { text-align: left; color: #555; font-size: 12px; min-width: 180px; cursor: text; white-space: nowrap; }
  td.note:empty::before { content: "…"; color: #ccc; }
  td.note:focus { outline: none; background: #ffc; }
  td.note.saved { background: #e8f5e8; transition: background 0.3s; }
  tr.hba1c td { text-align: left; font-size: 12px; color: #666; padding: 10px 10px 16px 10px; border-bottom: none; font-style: italic; }
  .graph-btn { cursor: pointer; border: 1px solid #ccc; background: #fafafa; border-radius: 3px; padding: 2px 7px; font-size: 11px; color: #555; }
  .graph-btn:hover { background: #eee; border-color: #999; }
  .modal-overlay { display: none; position: fixed; inset: 0; background: rgba(0,0,0,.45); z-index: 100; justify-content: center; align-items: flex-start; padding-top: 60px; }
  .modal-overlay.open { display: flex; }
  .modal { background: #fff; border-radius: 8px; padding: 20px; box-shadow: 0 8px 30px rgba(0,0,0,.25); position: relative; }
  .modal h2 { margin: 0 0 12px 0; font-size: 15px; font-weight: 600; }
  .modal .close { position: absolute; top: 8px; right: 12px; cursor: pointer; font-size: 18px; color: #999; background: none; border: none; }
  .modal .close:hover { color: #333; }
  .hba1c-modal { display: flex; gap: 0; min-height: 340px; }
  .hba1c-modal canvas { display: block; flex-shrink: 0; }
  #hba1cModal .modal { width: 860px; }
  .hba1c-list { width: 90px; max-height: 340px; overflow-y: auto; border-left: 1px solid #eee; margin: 0; padding: 0; list-style: none; flex-shrink: 0; }
  .hba1c-list li { padding: 4px 8px; font-size: 12px; cursor: pointer; white-space: nowrap; color: #555; }
  .hba1c-list li:hover, .hba1c-list li.active { background: #2266cc; color: #fff; }
  .hba1c-list .zone-header { padding: 4px 8px; font-size: 9px; text-transform: uppercase; letter-spacing: 0.05em; color: #999; font-weight: 600; cursor: default; background: #f8f8f8; border-bottom: 1px solid #eee; border-top: 1px solid #eee; pointer-events: none; user-select: none; }
  .hba1c-tabs { display: flex; gap: 0; margin-bottom: 10px; }
  .hba1c-tabs button { flex: 1; padding: 6px 12px; font-size: 12px; border: 1px solid #ddd; background: #fafafa; cursor: pointer; color: #555; }
  .hba1c-tabs button:first-child { border-radius: 4px 0 0 4px; }
  .hba1c-tabs button:last-child { border-radius: 0 4px 4px 0; border-left: none; }
  .hba1c-tabs button.active { background: #2266cc; color: #fff; border-color: #2266cc; }
  .hba1c-side { display: flex; flex-direction: column; gap: 8px; padding: 10px 14px; width: 160px; border-left: 1px solid #eee; flex-shrink: 0; justify-content: flex-start; }
  .hba1c-card { padding: 12px; border-radius: 5px; border: 2px solid #ddd; cursor: pointer; transition: border-color 0.15s, background 0.15s, opacity 0.15s; }
  .hba1c-card:hover, .hba1c-card.active { border-color: #2266cc; background: rgba(34,102,204,0.04); }
  .hba1c-card .card-title { font-size: 10px; text-transform: uppercase; letter-spacing: 0.06em; color: #666; background: #f0f0f0; padding: 5px 12px; margin: -12px -12px 8px -12px; border-radius: 3px 3px 0 0; font-weight: 600; border-bottom: 1px solid #e4e4e4; }
  .hba1c-card .card-hba1c { font-size: 22px; font-weight: 700; color: #222; margin: 0 0 4px 0; }
  .hba1c-card .card-range { font-size: 12px; color: #666; margin: 0 0 6px 0; }
  .hba1c-card .card-damage { font-size: 13px; color: #222; margin: 0; font-weight: 600; }
  .hba1c-card .card-damage .dmg-val { font-size: 18px; }
  .hba1c-card .card-damage .dmg-label { font-size: 10px; text-transform: uppercase; letter-spacing: 0.04em; color: #999; font-weight: 400; display: block; margin-top: 2px; }
  .hba1c-card.dim { opacity: 0.35; }
  #scratch { font-family: inherit; font-size: 20px; line-height: 1.3; color: #222; padding: 12px 16px; margin: 0 0 1em 0; border: 1px solid #eee; border-radius: 6px; width: 100%; box-sizing: border-box; resize: none; overflow: hidden; outline: none; }
  #scratch::placeholder { color: #ccc; }
  #scratch:focus { background: #ffc; }
  #scratch.saved { background: #e8f5e8; transition: background 0.3s; }
</style>
</head>
<body>
<h1>Glucose Tracker</h1>
<div class="meta">{{.Count}} readings · <button class="graph-btn" id="hba1cBtn">Explore HBA1c and Set Goals</button></div>
<textarea id="scratch" rows="1" spellcheck="false" placeholder="scratch…">{{.Scratch}}</textarea>
<table>
<thead>
<tr>
  <th>Date</th>
  <th>Day</th>
  {{range .Labels}}<th>{{.}}</th>{{end}}
  <th>Notes</th>
  <th></th>
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
  <td class="note" contenteditable="true" data-date="{{.Date}}">{{.Note}}</td>
  <td><button class="graph-btn" data-date="{{.Date}}">Graph</button></td>
</tr>
{{end}}
{{if .HbA1c}}
<tr class="hba1c"><td colspan="{{$.ColCount}}">Estimated HbA1c: {{.HbA1c}}% · eAG: {{.EAG}} mmol/L<br>Damage Index: {{.Damage}}</td></tr>
{{end}}
{{end}}
</tbody>
</table>

<div class="modal-overlay" id="graphModal">
  <div class="modal">
    <button class="close" id="graphClose">×</button>
    <h2 id="graphTitle"></h2>
    <div class="hba1c-modal">
      <canvas id="graphCanvas" width="520" height="300"></canvas>
      <div class="hba1c-side" id="graphDayCard" style="display:none">
        <div class="hba1c-card">
          <div class="card-title">This day</div>
          <div class="card-hba1c" id="dayHba1c"></div>
          <div class="card-range" id="dayRange"></div>
          <div class="card-damage"><span class="dmg-label">damage index (DCCT)</span><span class="dmg-val" id="dayDmg"></span></div>
        </div>
      </div>
    </div>
  </div>
</div>

<div class="modal-overlay" id="hba1cModal">
  <div class="modal">
    <button class="close" id="hba1cClose">×</button>
    <h2>HbA1c target ranges</h2>
    <div class="hba1c-tabs">
      <button id="tabRanges" class="active">browse ranges</button>
      <button id="tabGoal">goal vs. this week</button>
    </div>
    <div class="hba1c-modal">
      <canvas id="hba1cCanvas" width="520" height="340"></canvas>
      <ul class="hba1c-list" id="hba1cList"></ul>
      <div class="hba1c-side" id="hba1cBrowseCard" style="display: flex;">
        <div class="hba1c-card">
          <div class="card-hba1c" id="browseHba1c">6.5%</div>
          <div class="card-range" id="browseRange">5.0 – 10.5 mmol/L</div>
          <div class="card-damage"><span class="dmg-label">damage index (DCCT)</span><span class="dmg-val" id="browseDmg">15</span></div>
        </div>
      </div>
      <div class="hba1c-side" id="hba1cGoalSide" style="display: none;">
        <div class="hba1c-card" id="cardGoal">
          <div class="card-title">Goal</div>
          <div class="card-hba1c" id="cardGoalHba1c"></div>
          <div class="card-range" id="cardGoalRange"></div>
          <div class="card-damage"><span class="dmg-label">damage index (DCCT)</span><span class="dmg-val" id="goalDmg"></span></div>
        </div>
        <div class="hba1c-card" id="cardWeek">
          <div class="card-title">This week</div>
          <div class="card-hba1c" id="cardWeekHba1c"></div>
          <div class="card-range" id="cardWeekRange"></div>
          <div class="card-damage"><span class="dmg-label">damage index (DCCT)</span><span class="dmg-val" id="weekDmg"></span></div>
        </div>
      </div>
    </div>
  </div>
</div>

<script>
var WEEK_HBA1C = {{.WeekHbA1c}};
var GOAL_HBA1C = {{.GoalHbA1c}};

function applyDPR(cv, cx) {
  if (cv._dprApplied) return;
  var dpr = window.devicePixelRatio || 1;
  var w = cv.width, h = cv.height;
  cv.style.width = w + 'px';
  cv.style.height = h + 'px';
  cv.width = Math.floor(w * dpr);
  cv.height = Math.floor(h * dpr);
  cv._logicalW = w;
  cv._logicalH = h;
  cx.setTransform(dpr, 0, 0, dpr, 0, 0);
  cv._dprApplied = true;
}

var overlay = document.getElementById('graphModal');
var canvas = document.getElementById('graphCanvas');
var ctx = canvas.getContext('2d');
applyDPR(canvas, ctx);
var title = document.getElementById('graphTitle');

document.getElementById('graphClose').onclick = function() { overlay.classList.remove('open'); };
overlay.onclick = function(e) { if (e.target === overlay) overlay.classList.remove('open'); };
document.addEventListener('keydown', function(e) { if (e.key === 'Escape') { overlay.classList.remove('open'); document.getElementById('hba1cModal').classList.remove('open'); } });

var dayCard = document.getElementById('graphDayCard');
function updateDayCard(pts) {
  if (!pts || pts.length === 0) { dayCard.style.display = 'none'; return; }
  var sum = 0;
  for (var i = 0; i < pts.length; i++) sum += pts[i].mmol_l;
  var eag = sum / pts.length;
  var hba1c = (eag * 18.0182 + 46.7) / 28.7;
  var lo = Math.min(5, eag * 0.85);
  var hi = Math.min(20, eag * 2 - lo);
  if (hi < lo + 0.5) hi = lo + 0.5;
  document.getElementById('dayHba1c').textContent = hba1c.toFixed(1) + '%';
  document.getElementById('dayRange').textContent = lo.toFixed(1) + ' – ' + hi.toFixed(1) + ' mmol/L';
  if (pts.length > 5) {
    var d = Math.pow(hba1c / 6, 5) * 10;
    document.getElementById('dayDmg').textContent = d < 100 ? d.toFixed(0) : Math.round(d) + '';
  } else {
    document.getElementById('dayDmg').textContent = '-';
  }
  dayCard.style.display = 'flex';
}

document.querySelectorAll('.graph-btn[data-date]').forEach(function(btn) {
  btn.onclick = function() {
    var date = this.dataset.date;
    title.textContent = date;
    fetch('/api/readings?date=' + encodeURIComponent(date))
      .then(function(r) { return r.json(); })
      .then(function(pts) { drawGraph(pts); updateDayCard(pts); overlay.classList.add('open'); });
  };
});

function drawGraph(pts) {
  var W = canvas._logicalW || canvas.width, H = canvas._logicalH || canvas.height;
  var pad = {t: 20, r: 20, b: 35, l: 45};
  var gw = W - pad.l - pad.r, gh = H - pad.t - pad.b;
  ctx.clearRect(0, 0, W, H);

  if (!pts || pts.length === 0) {
    ctx.fillStyle = '#999'; ctx.font = '13px sans-serif'; ctx.textAlign = 'center';
    ctx.fillText('no readings', W/2, H/2);
    return;
  }

  var data = pts.map(function(p) {
    var parts = p.time.split(':');
    return { min: parseInt(parts[0])*60 + parseInt(parts[1]), v: p.mmol_l };
  });

  var minT = 0, maxT = 24*60;
  var minV = 2, maxV = 20;

  function x(m) { return pad.l + (m - minT) / (maxT - minT) * gw; }
  function y(v) { return pad.t + (1 - (v - minV) / (maxV - minV)) * gh; }

  ctx.fillStyle = 'rgba(200,236,200,0.3)';
  var y8 = Math.max(pad.t, y(8)), y4 = Math.min(pad.t + gh, y(4));
  ctx.fillRect(pad.l, y8, gw, y4 - y8);
  ctx.fillStyle = 'rgba(255,224,168,0.3)';
  var y10 = Math.max(pad.t, y(10));
  ctx.fillRect(pad.l, y10, gw, y8 - y10);

  ctx.strokeStyle = '#e0e0e0'; ctx.lineWidth = 0.5;
  for (var v = Math.ceil(minV); v <= Math.floor(maxV); v++) {
    ctx.beginPath(); ctx.moveTo(pad.l, y(v)); ctx.lineTo(pad.l + gw, y(v)); ctx.stroke();
  }

  ctx.fillStyle = '#888'; ctx.font = '11px sans-serif'; ctx.textAlign = 'right'; ctx.textBaseline = 'middle';
  for (var v = Math.ceil(minV); v <= Math.floor(maxV); v++) {
    ctx.fillText(v, pad.l - 6, y(v));
  }

  ctx.textAlign = 'center'; ctx.textBaseline = 'top';
  for (var m = 0; m <= maxT; m += 120) {
    var hh = Math.floor(m / 60), mm = m % 60;
    ctx.fillText((hh < 10 ? '0' : '') + hh + ':' + (mm < 10 ? '0' : '') + mm, x(m), pad.t + gh + 6);
  }

  ctx.strokeStyle = '#2266cc'; ctx.lineWidth = 2; ctx.lineJoin = 'round';
  ctx.beginPath();
  data.forEach(function(d, i) {
    if (i === 0) ctx.moveTo(x(d.min), y(d.v)); else ctx.lineTo(x(d.min), y(d.v));
  });
  ctx.stroke();

  ctx.fillStyle = '#2266cc';
  data.forEach(function(d) {
    ctx.beginPath(); ctx.arc(x(d.min), y(d.v), 3.5, 0, Math.PI*2); ctx.fill();
  });
}

(function() {
  var modal = document.getElementById('hba1cModal');
  var cv = document.getElementById('hba1cCanvas');
  var cx = cv.getContext('2d');
  applyDPR(cv, cx);
  var list = document.getElementById('hba1cList');
  var tabRanges = document.getElementById('tabRanges');
  var tabGoal = document.getElementById('tabGoal');
  var slots = [8*60, 10*60, 12*60, 14*60, 16*60, 18*60, 21*60];
  var weekHba1c = WEEK_HBA1C;
  var goalHba1c = GOAL_HBA1C;

  function damageIndex(h) { return Math.pow(h / 6, 5) * 10; }
  function fmtDmg(h) { var d = damageIndex(h); return d < 100 ? d.toFixed(0) : Math.round(d) + ''; }

  var mode = 'ranges';
  var currentBrowse = 6.5;

  function hba1cToRange(h) {
    var eag = (28.7 * h - 46.7) / 18.0182;
    var lo = Math.min(5, eag * 0.85);
    var hi = Math.min(20, eag * 2 - lo);
    if (hi < lo + 0.5) hi = lo + 0.5;
    return {eag: eag, lo: lo, hi: hi};
  }

  function mulberry32(a) {
    return function() {
      a |= 0; a = a + 0x6D2B79F5 | 0;
      var t = Math.imul(a ^ a >>> 15, 1 | a);
      t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t;
      return ((t ^ t >>> 14) >>> 0) / 4294967296;
    };
  }

  function drawAxes() {
    var W = cv._logicalW || cv.width, H = cv._logicalH || cv.height;
    var pad = {t: 20, r: 20, b: 35, l: 45};
    var gw = W - pad.l - pad.r, gh = H - pad.t - pad.b;
    var minT = 0, maxT = 24*60, minV = 2, maxV = 20;
    function x(m) { return pad.l + (m - minT) / (maxT - minT) * gw; }
    function y(v) { return pad.t + (1 - (v - minV) / (maxV - minV)) * gh; }
    cx.clearRect(0, 0, W, H);

    cx.fillStyle = 'rgba(200,236,200,0.3)';
    var y8 = Math.max(pad.t, y(8)), y4 = Math.min(pad.t + gh, y(4));
    cx.fillRect(pad.l, y8, gw, y4 - y8);
    cx.fillStyle = 'rgba(255,224,168,0.3)';
    var y10 = Math.max(pad.t, y(10));
    cx.fillRect(pad.l, y10, gw, y8 - y10);

    cx.strokeStyle = '#e0e0e0'; cx.lineWidth = 0.5;
    for (var v = Math.ceil(minV); v <= Math.floor(maxV); v++) {
      cx.beginPath(); cx.moveTo(pad.l, y(v)); cx.lineTo(pad.l + gw, y(v)); cx.stroke();
    }
    cx.fillStyle = '#888'; cx.font = '11px sans-serif'; cx.textAlign = 'right'; cx.textBaseline = 'middle';
    for (var v = Math.ceil(minV); v <= Math.floor(maxV); v++) {
      cx.fillText(v, pad.l - 6, y(v));
    }
    cx.textAlign = 'center'; cx.textBaseline = 'top';
    for (var m = 0; m <= maxT; m += 120) {
      var hh = Math.floor(m / 60), mm = m % 60;
      cx.fillText((hh < 10 ? '0' : '') + hh + ':' + (mm < 10 ? '0' : '') + mm, x(m), pad.t + gh + 6);
    }
    return {x: x, y: y, pad: pad, gw: gw, gh: gh};
  }

  function drawBlock(ax, h, color, seed, showDots) {
    var r = hba1cToRange(h);
    var xMin = ax.x(slots[0]) - 8, xMax = ax.x(slots[slots.length - 1]) + 8;
    cx.fillStyle = color.fill;
    cx.strokeStyle = color.stroke;
    cx.lineWidth = 1.5;
    cx.fillRect(xMin, ax.y(r.hi), xMax - xMin, ax.y(r.lo) - ax.y(r.hi));
    cx.strokeRect(xMin, ax.y(r.hi), xMax - xMin, ax.y(r.lo) - ax.y(r.hi));

    cx.strokeStyle = color.stroke; cx.lineWidth = 1;
    cx.setLineDash([4, 3]);
    cx.beginPath(); cx.moveTo(xMin, ax.y(r.eag)); cx.lineTo(xMax, ax.y(r.eag)); cx.stroke();
    cx.setLineDash([]);

    if (showDots) {
      var rng = mulberry32(seed);
      cx.fillStyle = color.dot;
      slots.forEach(function(m) {
        var v = r.lo + ((rng() + rng() + rng()) / 3) * (r.hi - r.lo);
        cx.beginPath(); cx.arc(ax.x(m), ax.y(v), 4, 0, Math.PI * 2); cx.fill();
      });
    }
    return r;
  }

  function drawRangesMode(h) {
    var ax = drawAxes();
    var r = drawBlock(ax, h,
      {fill: 'rgba(34,102,204,0.15)', stroke: 'rgba(34,102,204,0.4)', dot: '#2266cc'},
      Math.round(h * 100), true);
    document.getElementById('browseHba1c').textContent = h.toFixed(1) + '%';
    document.getElementById('browseRange').textContent = r.lo.toFixed(1) + ' – ' + r.hi.toFixed(1) + ' mmol/L';
    document.getElementById('browseDmg').textContent = fmtDmg(h);
  }

  var bright = {fill: 'rgba(34,102,204,0.18)', stroke: 'rgba(34,102,204,0.5)', dot: '#2266cc'};
  var dim    = {fill: 'rgba(34,102,204,0.05)', stroke: 'rgba(34,102,204,0.12)', dot: 'rgba(34,102,204,0.2)'};
  var cardGoal = document.getElementById('cardGoal');
  var cardWeek = document.getElementById('cardWeek');
  var surfaced = null;

  function drawGoalMode() {
    var ax = drawAxes();
    var rg = hba1cToRange(goalHba1c);
    var rw = hba1cToRange(weekHba1c);

    if (surfaced === 'goal') {
      drawBlock(ax, weekHba1c, dim, Math.round(weekHba1c * 100) + 7777, false);
      drawBlock(ax, goalHba1c, bright, Math.round(goalHba1c * 100) + 5555, true);
    } else if (surfaced === 'week') {
      drawBlock(ax, goalHba1c, dim, Math.round(goalHba1c * 100) + 5555, false);
      drawBlock(ax, weekHba1c, bright, Math.round(weekHba1c * 100) + 7777, true);
    } else {
      drawBlock(ax, goalHba1c, dim, Math.round(goalHba1c * 100) + 5555, false);
      drawBlock(ax, weekHba1c, dim, Math.round(weekHba1c * 100) + 7777, false);
    }

    document.getElementById('cardGoalHba1c').textContent = goalHba1c.toFixed(1) + '%';
    document.getElementById('cardGoalRange').textContent = rg.lo.toFixed(1) + ' – ' + rg.hi.toFixed(1) + ' mmol/L';
    document.getElementById('goalDmg').textContent = fmtDmg(goalHba1c);
    document.getElementById('cardWeekHba1c').textContent = weekHba1c.toFixed(1) + '%';
    document.getElementById('cardWeekRange').textContent = rw.lo.toFixed(1) + ' – ' + rw.hi.toFixed(1) + ' mmol/L';
    document.getElementById('weekDmg').textContent = fmtDmg(weekHba1c);

    cardGoal.classList.toggle('dim', surfaced === 'week');
    cardGoal.classList.toggle('active', surfaced === 'goal');
    cardWeek.classList.toggle('dim', surfaced === 'goal');
    cardWeek.classList.toggle('active', surfaced === 'week');
  }

  cardGoal.onmouseenter = function() { surfaced = 'goal'; drawGoalMode(); };
  cardWeek.onmouseenter = function() { surfaced = 'week'; drawGoalMode(); };
  cardGoal.onmouseleave = cardWeek.onmouseleave = function() { surfaced = null; drawGoalMode(); };

  var browseCard = document.getElementById('hba1cBrowseCard');
  var goalSide = document.getElementById('hba1cGoalSide');
  function refresh() {
    if (mode === 'goal') {
      drawGoalMode();
      list.style.display = 'none'; browseCard.style.display = 'none'; goalSide.style.display = 'flex';
    } else {
      drawRangesMode(currentBrowse);
      list.style.display = ''; browseCard.style.display = 'flex'; goalSide.style.display = 'none';
    }
  }

  tabRanges.onclick = function() { mode = 'ranges'; tabRanges.classList.add('active'); tabGoal.classList.remove('active'); refresh(); };
  tabGoal.onclick = function() { mode = 'goal'; tabGoal.classList.add('active'); tabRanges.classList.remove('active'); refresh(); };

  document.getElementById('hba1cBtn').onclick = function() {
    modal.classList.add('open');
    refresh();
    var activeLi = null;
    list.querySelectorAll('li').forEach(function(li) {
      var isActive = parseFloat(li.dataset.hba1c) === currentBrowse;
      li.classList.toggle('active', isActive);
      if (isActive) activeLi = li;
    });
    if (activeLi) list.scrollTop = activeLi.offsetTop - list.offsetTop;
  };
  document.getElementById('hba1cClose').onclick = function() { modal.classList.remove('open'); };
  modal.onclick = function(e) { if (e.target === modal) modal.classList.remove('open'); };

  var zones = [
    {at: 46, label: 'stellar'},
    {at: 52, label: 'healthy'},
    {at: 57, label: 'prediabetes'},
    {at: 65, label: 'diabetes'}
  ];
  var zi = 0;
  for (var h = 46; h <= 100; h++) {
    if (zi < zones.length && h === zones[zi].at) {
      var hdr = document.createElement('li');
      hdr.className = 'zone-header';
      hdr.textContent = zones[zi].label;
      list.appendChild(hdr);
      zi++;
    }
    var li = document.createElement('li');
    var val = h / 10;
    li.textContent = val.toFixed(1) + '%';
    li.dataset.hba1c = val;
    li.onmouseenter = li.onclick = function() {
      var v = parseFloat(this.dataset.hba1c);
      currentBrowse = v;
      list.querySelectorAll('li').forEach(function(l) { l.classList.remove('active'); });
      this.classList.add('active');
      drawRangesMode(v);
    };
    list.appendChild(li);
  }
})();

(function() {
  var el = document.getElementById('scratch');
  var timer;
  function autosize() { el.style.height = 'auto'; el.style.height = el.scrollHeight + 'px'; }
  el.addEventListener('input', function() {
    autosize();
    clearTimeout(timer);
    timer = setTimeout(function() {
      fetch('/scratch', {
        method: 'POST',
        headers: {'Content-Type': 'application/x-www-form-urlencoded'},
        body: 'content=' + encodeURIComponent(el.value)
      }).then(function(r) {
        if (r.ok) { el.classList.add('saved'); setTimeout(function(){ el.classList.remove('saved'); }, 800); }
      });
    }, 400);
  });
  autosize();
})();

document.querySelectorAll('td.note').forEach(function(el) {
  var timer;
  el.addEventListener('input', function() {
    var td = this;
    clearTimeout(timer);
    timer = setTimeout(function() {
      fetch('/notes', {
        method: 'POST',
        headers: {'Content-Type': 'application/x-www-form-urlencoded'},
        body: 'date=' + encodeURIComponent(td.dataset.date) + '&note=' + encodeURIComponent(td.textContent.trim())
      }).then(function(r) {
        if (r.ok) { td.classList.add('saved'); setTimeout(function(){ td.classList.remove('saved'); }, 800); }
      });
    }, 400);
  });
  el.addEventListener('keydown', function(e) {
    if (e.key === 'Enter') { e.preventDefault(); this.blur(); }
  });
});
</script>
</body>
</html>
`

const port = 10943

var (
	notesMu   sync.Mutex
	scratchMu sync.Mutex
)

func main() {
	loc, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		log.Fatalf("load tz: %v", err)
	}

	tsvPath, err := findFile("records.tsv")
	if err != nil {
		log.Fatalf("%v", err)
	}
	notesPath, err := findFile("notes.tsv")
	if err != nil {
		// notes.tsv may not exist yet — place it alongside records.tsv
		notesPath = filepath.Join(filepath.Dir(tsvPath), "notes.tsv")
	}
	scratchPath, err := findFile("scratch.txt")
	if err != nil {
		scratchPath = filepath.Join(filepath.Dir(tsvPath), "scratch.txt")
	}
	log.Printf("records=%s notes=%s scratch=%s", tsvPath, notesPath, scratchPath)

	t := template.Must(template.New("r").Parse(tpl))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		readings, err := loadReadings(tsvPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		notes, err := loadNotes(notesPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		scratch := loadScratch(scratchPath)

		weeks := buildWeeks(readings, loc, notes)
		sort.Slice(weeks, func(i, j int) bool {
			if weeks[i].year != weeks[j].year {
				return weeks[i].year > weeks[j].year
			}
			return weeks[i].weekNum > weeks[j].weekNum
		})

		weekHbA1c := 6.5
		if len(weeks) > 0 && weeks[0].HbA1c != "" {
			if v, err := strconv.ParseFloat(weeks[0].HbA1c, 64); err == nil {
				weekHbA1c = v
			}
		}

		goal := parseGoalHbA1c(scratch, 5.5)

		data := pagePayload{
			Count:     len(readings),
			Labels:    slotLabels,
			Weeks:     weeks,
			ColCount:  2 + len(slotLabels) + 2,
			Scratch:   scratch,
			WeekHbA1c: template.JS(strconv.FormatFloat(weekHbA1c, 'f', 1, 64)),
			GoalHbA1c: template.JS(strconv.FormatFloat(goal, 'f', 1, 64)),
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := t.Execute(w, data); err != nil {
			log.Printf("template: %v", err)
		}
	})

	http.HandleFunc("/api/readings", func(w http.ResponseWriter, r *http.Request) {
		dateStr := r.URL.Query().Get("date")
		day, err := time.ParseInLocation("2006-01-02", dateStr, loc)
		if err != nil {
			http.Error(w, "bad date", 400)
			return
		}
		readings, err := loadReadings(tsvPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		type outPoint struct {
			Time  string  `json:"time"`
			MmolL float64 `json:"mmol_l"`
		}
		next := day.AddDate(0, 0, 1)
		var out []outPoint
		for _, rd := range readings {
			t := rd.ts.In(loc)
			if !t.Before(day) && t.Before(next) {
				out = append(out, outPoint{
					Time:  t.Format("15:04"),
					MmolL: rd.mmolL,
				})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	})

	http.HandleFunc("/notes", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		date := r.FormValue("date")
		note := strings.TrimSpace(r.FormValue("note"))
		if date == "" {
			http.Error(w, "missing date", 400)
			return
		}

		notesMu.Lock()
		defer notesMu.Unlock()
		notes, err := loadNotes(notesPath)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if note == "" {
			delete(notes, date)
		} else {
			notes[date] = note
		}
		if err := saveNotes(notesPath, notes); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	})

	http.HandleFunc("/scratch", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var content string
		if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			content = r.FormValue("content")
		} else {
			b, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			content = string(b)
		}

		scratchMu.Lock()
		defer scratchMu.Unlock()
		if err := saveScratch(scratchPath, content); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(204)
	})

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	log.Printf("http://localhost:%d", port)
	log.Fatal(http.ListenAndServe(addr, nil))
}
