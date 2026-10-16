package reports

import (
	"fmt"
	"html/template"
	"io"
	"time"
)

var funcs = template.FuncMap{
	"count": humanCount,
	"pct": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprintf("%.2f%%", *v)
	},
	"ms": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprintf("%.0f ms", *v)
	},
	"num": func(v float64) string { return fmt.Sprintf("%.4g", v) },
	"fms": func(v float64) string { return fmt.Sprintf("%.0f ms", v) },
	"change": func(v *float64) string {
		if v == nil {
			return "—"
		}
		return fmt.Sprintf("%+.0f%%", *v)
	},
	// tone classes are data colours: worse p99, error rate, alarm state
	"changeTone": func(v *float64) string {
		switch {
		case v == nil:
			return ""
		case *v > 20:
			return "crit"
		case *v < -20:
			return "pos"
		}
		return ""
	},
	"errTone": func(v *float64) string {
		switch {
		case v == nil:
			return ""
		case *v >= 5:
			return "crit"
		case *v >= 1:
			return "warn"
		}
		return ""
	},
	"stateTone": func(s any) string {
		switch fmt.Sprint(s) {
		case "ALARM":
			return "crit"
		case "OK":
			return "pos"
		}
		return "none"
	},
	"when":  func(ms int64) string { return time.UnixMilli(ms).UTC().Format("Jan 2 15:04 UTC") },
	"whenf": func(ms float64) string { return time.UnixMilli(int64(ms)).UTC().Format("Jan 2 15:04 UTC") },
	"whenp": func(ms *int64) string {
		if ms == nil {
			return "not in horizon"
		}
		return time.UnixMilli(*ms).UTC().Format("Jan 2 15:04 UTC")
	},
	"stamp": func(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") },
}

var page = template.Must(template.New("report").Funcs(funcs).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>ZeroWatch report · {{.Range.ID}}</title>
<style>
:root{--bg:oklch(0.95 0.014 85);--fg:oklch(0.17 0 0);--muted:oklch(0.42 0.01 85);--surface:oklch(0.915 0.018 85);
--border:oklch(0.17 0 0 / .3);--hair:oklch(0.17 0 0 / .11);--pos:oklch(0.56 0.14 152);--warn:oklch(0.66 0.16 62);--crit:oklch(0.54 0.2 27)}
@media (prefers-color-scheme:dark){:root{--bg:oklch(0.145 0 0);--fg:oklch(0.95 0.014 85);--muted:oklch(0.74 0.008 85);--surface:oklch(0.195 0 0);
--border:oklch(0.95 0.014 85 / .24);--hair:oklch(0.95 0.014 85 / .09);--pos:oklch(0.68 0.15 152);--warn:oklch(0.75 0.16 62);--crit:oklch(0.66 0.19 25)}}
*{box-sizing:border-box;border-radius:0}
html,body{overflow-x:clip}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.6 "DM Sans",system-ui,sans-serif}
main{max-width:72rem;margin:0 auto;padding:2rem 1rem 4rem}
.micro,th,.brand{font:500 .66rem/1.5 "JetBrains Mono",ui-monospace,monospace;text-transform:uppercase;letter-spacing:.12em}
.brand{font-family:"Space Grotesk",system-ui,sans-serif;font-weight:600;letter-spacing:.34em;font-size:.8rem}
.muted{color:var(--muted)}
h1{font:600 clamp(2.4rem,8vw,5.5rem)/.88 "Space Grotesk",system-ui,sans-serif;text-transform:uppercase;letter-spacing:-.02em;margin:1.5rem 0 1rem;overflow-wrap:anywhere}
.lead{max-width:54ch;font-size:1.1rem;line-height:1.4;margin:0 0 2rem}
header{display:flex;justify-content:space-between;gap:1rem;flex-wrap:wrap;border-bottom:1px solid var(--hair);padding-bottom:.75rem}
.stats{display:grid;grid-template-columns:repeat(2,1fr);border-top:1px solid var(--border);border-left:1px solid var(--border);margin-bottom:2.5rem}
@media(min-width:48rem){.stats{grid-template-columns:repeat(5,1fr)}}
.stat{border-right:1px solid var(--border);border-bottom:1px solid var(--border);padding:1rem}
.big{font:600 clamp(1.8rem,4vw,3rem)/1 "Space Grotesk",system-ui,sans-serif;letter-spacing:-.02em;font-variant-numeric:tabular-nums;margin-top:.5rem}
section{margin-bottom:2.5rem}
.sh{display:flex;justify-content:space-between;gap:1rem;border-bottom:1px solid var(--border);padding-bottom:.6rem;margin-bottom:.25rem}
.scroll{overflow-x:auto;position:relative}
table{width:100%;border-collapse:collapse;font-variant-numeric:tabular-nums}
th{text-align:left;color:var(--muted);padding:.6rem .75rem .6rem 0;font-weight:500}
td{border-top:1px solid var(--hair);padding:.6rem .75rem .6rem 0;white-space:nowrap}
td.r,th.r{text-align:right}
.sq{display:inline-block;width:.6rem;height:.6rem;margin-right:.45rem;vertical-align:baseline}
.crit{color:var(--crit)}.warn{color:var(--warn)}.pos{color:var(--pos)}
.sq.crit{background:var(--crit)}.sq.pos{background:var(--pos)}.sq.none{border:1px dashed var(--muted)}
.empty{border:1px dashed var(--border);padding:1rem;color:var(--muted)}
footer{border-top:1px solid var(--hair);padding-top:.75rem}
</style></head><body><main>
<header><span class="brand">ZeroWatch</span><span class="micro muted">Report · last {{.Range.ID}} · {{when .Range.Start}} → {{when .Range.End}}</span></header>
<h1>Last {{.Range.ID}}.</h1>
{{with .Narrative}}<p class="lead">{{.}}</p>{{end}}
<div class="stats">
<div class="stat"><div class="micro muted">Requests</div><div class="big">{{count .Totals.Requests}}</div></div>
<div class="stat"><div class="micro muted">Error rate</div><div class="big {{errTone .Totals.ErrorPct}}">{{pct .Totals.ErrorPct}}</div></div>
<div class="stat"><div class="micro muted">p50</div><div class="big">{{ms .Totals.P50}}</div></div>
<div class="stat"><div class="micro muted">p99</div><div class="big">{{ms .Totals.P99}}</div></div>
<div class="stat"><div class="micro muted">Good periods</div><div class="big">{{pct .Totals.AvailabilityPct}}</div></div>
</div>

<section><div class="sh"><span class="micro">Services</span><span class="micro muted">p99 change vs previous {{.Range.ID}}</span></div>
{{if .Services}}<div class="scroll"><table><tr><th>Service</th><th class="r">Requests</th><th class="r">Errors</th><th class="r">p99</th><th class="r">Change</th><th class="r">Good periods</th></tr>
{{range .Services}}<tr><td>{{.Name}}</td><td class="r">{{count .Requests}}</td><td class="r {{errTone .ErrorPct}}">{{pct .ErrorPct}}</td><td class="r">{{ms .P99}}</td><td class="r {{changeTone .P99ChangePct}}">{{change .P99ChangePct}}</td><td class="r">{{pct .AvailabilityPct}}</td></tr>
{{end}}</table></div>{{else}}<p class="empty">No services reported RED metrics in this window.</p>{{end}}</section>

<section><div class="sh"><span class="micro">Alarms</span><span class="micro muted">times fired · minutes in alarm</span></div>
{{if .Alarms}}<div class="scroll"><table><tr><th>Alarm</th><th>State</th><th class="r">Fired</th><th class="r">Minutes in alarm</th></tr>
{{range .Alarms}}<tr><td>{{.Name}}</td><td><span class="sq {{stateTone .State}}" aria-hidden="true"></span>{{if .State}}{{.State}}{{else}}UNKNOWN{{end}}</td><td class="r">{{.Fired}}</td><td class="r">{{.MinutesInAlarm}}</td></tr>
{{end}}</table></div>{{else}}<p class="empty">No alarm rules.</p>{{end}}</section>

<section><div class="sh"><span class="micro">Capacity forecast</span><span class="micro muted">when each alarm threshold is forecast to be crossed</span></div>
{{if .Capacity}}<div class="scroll"><table><tr><th>Alarm</th><th>Metric</th><th class="r">Threshold</th><th>Crosses</th><th>Method</th></tr>
{{range .Capacity}}<tr><td>{{.Label}}</td><td>{{with .Service}}{{.}} · {{end}}{{.Metric}}</td><td class="r">{{num .Threshold}}</td><td class="{{if .CrossesAt}}warn{{else}}muted{{end}}">{{whenp .CrossesAt}}</td><td class="micro muted">{{.Method}}</td></tr>
{{end}}</table></div>{{else}}<p class="empty">No forecasts: no enabled alarm rules, or too little history.</p>{{end}}</section>

<section><div class="sh"><span class="micro">Anomalies</span><span class="micro muted">outside the robust STL band</span></div>
{{if .Anomalies}}<div class="scroll"><table><tr><th>When</th><th>Service</th><th>Metric</th><th class="r">Value</th><th class="r">Expected</th></tr>
{{range .Anomalies}}<tr><td>{{when .At}}</td><td>{{.Service}}</td><td>{{.Metric}}</td><td class="r crit">{{num .Value}}</td><td class="r muted">{{num .ExpectedLower}} – {{num .ExpectedUpper}}</td></tr>
{{end}}</table></div>{{else}}<p class="empty">No anomalies, or not enough history to judge (two seasons are needed).</p>{{end}}</section>

<section><div class="sh"><span class="micro">Slowest traces</span><span class="micro muted">kept in memory on this node</span></div>
{{if .SlowTraces}}<div class="scroll"><table><tr><th>Started</th><th>Root</th><th class="r">Duration</th><th class="r">Spans</th><th class="r">Errors</th><th>Trace</th></tr>
{{range .SlowTraces}}<tr><td>{{whenf .Start}}</td><td>{{.RootService}} · {{.RootName}}</td><td class="r">{{fms .DurationMs}}</td><td class="r">{{.Spans}}</td><td class="r {{if .Errors}}crit{{end}}">{{.Errors}}</td><td class="micro muted">{{.TraceID}}</td></tr>
{{end}}</table></div>{{else}}<p class="empty">No traces in this window.</p>{{end}}</section>

<footer class="micro muted">Generated {{stamp .GeneratedAt}}. p50 and p99 are request-weighted means of per-period percentiles; good periods have an error rate below 5 %.</footer>
</main></body></html>
`))

// RenderHTML writes rep as a self-contained HTML page (no external requests).
func RenderHTML(w io.Writer, rep *Report) error { return page.Execute(w, rep) }
