package reports

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zerowatch/zerowatch/internal/alarms"
	"github.com/zerowatch/zerowatch/internal/httpx"
)

// ParseTargets parses "slack:<url>", "webhook:<url>" and "log" entries.
func ParseTargets(specs []string) ([]alarms.Target, error) {
	var out []alarms.Target
	for i, s := range specs {
		typ, u, _ := strings.Cut(s, ":")
		switch typ {
		case "log":
			out = append(out, alarms.Target{Type: "log"})
		case "slack", "webhook":
			if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
				return nil, fmt.Errorf("report target %d: %s needs an http(s) url", i+1, typ)
			}
			out = append(out, alarms.Target{Type: typ, URL: u})
		default:
			return nil, fmt.Errorf("report target %d: unknown type %q (slack|webhook|log)", i+1, typ)
		}
	}
	return out, nil
}

// Register adds the report routes.
func (b *Builder) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/reports/summary", b.summary)
	mux.HandleFunc("GET /api/v1/reports/summary.html", b.summaryHTML)
	mux.HandleFunc("POST /api/v1/reports/send", b.send)
}

func rangeParam(r *http.Request) string {
	if v := r.URL.Query().Get("range"); v != "" {
		return v
	}
	return "24h"
}

func (b *Builder) summary(w http.ResponseWriter, r *http.Request) {
	rep, err := b.Build(r.Context(), httpx.Tenant(r.Context()), rangeParam(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	httpx.WriteJSON(w, 200, rep)
}

func (b *Builder) summaryHTML(w http.ResponseWriter, r *http.Request) {
	rep, err := b.Build(r.Context(), httpx.Tenant(r.Context()), rangeParam(r))
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	var buf bytes.Buffer
	if err := RenderHTML(&buf, rep); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (b *Builder) send(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Range string `json:"range"`
	}
	if err := httpx.DecodeJSON(w, r, &body, 4096, true); err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	if body.Range == "" {
		body.Range = "24h"
	}
	if len(b.Targets) == 0 {
		httpx.WriteError(w, r, httpx.BadRequest("no report targets configured (--report-targets)"))
		return
	}
	rep, err := b.Build(r.Context(), httpx.Tenant(r.Context()), body.Range)
	if err != nil {
		httpx.WriteError(w, r, err)
		return
	}
	sent, err := b.Send(r.Context(), rep)
	if err != nil && len(sent) == 0 {
		httpx.WriteError(w, r, httpx.Unavailable(err.Error()))
		return
	}
	httpx.WriteJSON(w, 200, map[string]any{"sent": sent})
}

// targetName identifies a target without its URL path (Slack and webhook
// URLs carry secrets).
func targetName(t alarms.Target) string {
	if u, err := url.Parse(t.URL); err == nil && u.Host != "" {
		return t.Type + ":" + u.Host
	}
	return t.Type
}

// Send delivers rep to every target and returns those that accepted it; err
// joins the failures.
func (b *Builder) Send(ctx context.Context, rep *Report) ([]string, error) {
	c := b.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	sent := []string{}
	var fails []string
	for _, t := range b.Targets {
		var err error
		switch t.Type {
		case "log":
			if b.Log != nil {
				b.Log.Info("report", "range", rep.Range.ID, "summary", rep.Narrative)
			}
		case "slack":
			err = postJSON(ctx, c, t.URL, map[string]string{"text": fmt.Sprintf("*ZeroWatch report, last %s*\n%s", rep.Range.ID, rep.Narrative)})
		case "webhook":
			err = postJSON(ctx, c, t.URL, rep)
		}
		if err != nil {
			fails = append(fails, targetName(t)+": "+err.Error())
			continue
		}
		sent = append(sent, targetName(t))
	}
	if len(fails) > 0 {
		return sent, fmt.Errorf("report delivery failed: %s", strings.Join(fails, "; "))
	}
	return sent, nil
}

func postJSON(ctx context.Context, c *http.Client, u string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zerowatch-reports/1.0")
	resp, err := c.Do(req)
	if err != nil {
		// url.Error repeats the full URL; keep only the cause.
		if ue, ok := err.(*url.Error); ok {
			return ue.Err
		}
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// Run builds and sends the rangeID report every interval until ctx ends.
func (b *Builder) Run(ctx context.Context, every time.Duration, tenant uint32, rangeID string) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rep, err := b.Build(ctx, tenant, rangeID)
			if err == nil {
				var sent []string
				sent, err = b.Send(ctx, rep)
				if b.Log != nil && len(sent) > 0 {
					b.Log.Info("scheduled report sent", "range", rangeID, "targets", sent)
				}
			}
			if err != nil && b.Log != nil && ctx.Err() == nil {
				b.Log.Warn("scheduled report failed", "err", err)
			}
		}
	}
}
