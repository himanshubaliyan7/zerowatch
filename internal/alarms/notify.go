package alarms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// Target is a notification destination attached to an alarm rule.
type Target struct {
	Type       string `json:"type"` // webhook | slack | pagerduty | sns (URL = topic ARN) | log
	URL        string `json:"url,omitempty"`
	RoutingKey string `json:"routingKey,omitempty"` // PagerDuty integration key
}

// Notifier delivers a transition to a destination.
type Notifier interface {
	Notify(ctx context.Context, t Target, tr Transition) error
}

// NotifierFunc adapts a function.
type NotifierFunc func(ctx context.Context, t Target, tr Transition) error

// Notify implements Notifier.
func (f NotifierFunc) Notify(ctx context.Context, t Target, tr Transition) error {
	return f(ctx, t, tr)
}

func postJSON(ctx context.Context, c *http.Client, url string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "zerowatch-alarms/1.0")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s returned HTTP %d", url, resp.StatusCode)
	}
	return nil
}

// WebhookNotifier POSTs the raw transition JSON.
type WebhookNotifier struct{ Client *http.Client }

// Notify implements Notifier.
func (n WebhookNotifier) Notify(ctx context.Context, t Target, tr Transition) error {
	return postJSON(ctx, n.Client, t.URL, tr)
}

// SlackNotifier posts to a Slack incoming webhook.
type SlackNotifier struct{ Client *http.Client }

// Notify implements Notifier.
func (n SlackNotifier) Notify(ctx context.Context, t Target, tr Transition) error {
	emoji := map[State]string{StateAlarm: ":rotating_light:", StateOK: ":white_check_mark:", StateInsufficientData: ":grey_question:"}[tr.To]
	return postJSON(ctx, n.Client, t.URL, map[string]any{
		"text": fmt.Sprintf("%s *%s* %s → %s\n%s", emoji, tr.RuleName, tr.From, tr.To, tr.Reason),
	})
}

// PagerDutyNotifier uses the Events API v2 (trigger on ALARM, resolve on OK).
type PagerDutyNotifier struct {
	Client   *http.Client
	Endpoint string // default https://events.pagerduty.com/v2/enqueue
}

// Notify implements Notifier.
func (n PagerDutyNotifier) Notify(ctx context.Context, t Target, tr Transition) error {
	action := "trigger"
	if tr.To == StateOK {
		action = "resolve"
	} else if tr.To != StateAlarm {
		return nil
	}
	ep := n.Endpoint
	if ep == "" {
		ep = "https://events.pagerduty.com/v2/enqueue"
	}
	if t.URL != "" {
		ep = t.URL
	}
	return postJSON(ctx, n.Client, ep, map[string]any{
		"routing_key":  t.RoutingKey,
		"event_action": action,
		"dedup_key":    "zerowatch-" + tr.RuleID,
		"payload": map[string]any{
			"summary":   fmt.Sprintf("%s is %s: %s", tr.RuleName, tr.To, tr.Reason),
			"source":    "zerowatch",
			"severity":  "error",
			"timestamp": tr.Timestamp.Format(time.RFC3339),
		},
	})
}

// LogNotifier writes transitions to the structured log.
type LogNotifier struct{ Log *slog.Logger }

// Notify implements Notifier.
func (n LogNotifier) Notify(_ context.Context, _ Target, tr Transition) error {
	n.Log.Info("alarm transition", "rule", tr.RuleName, "from", tr.From, "to", tr.To, "reason", tr.Reason)
	return nil
}

// Dispatcher routes transitions to notifiers with bounded retries and
// exponential backoff. Delivery is asynchronous so a slow webhook never
// blocks evaluation.
type Dispatcher struct {
	notifiers map[string]Notifier
	log       *slog.Logger
	attempts  int
	backoff   time.Duration
	sem       chan struct{}
}

// NewDispatcher builds a dispatcher with the built-in notifiers.
func NewDispatcher(log *slog.Logger) *Dispatcher {
	c := &http.Client{Timeout: 10 * time.Second}
	return &Dispatcher{
		notifiers: map[string]Notifier{
			"webhook":   WebhookNotifier{Client: c},
			"slack":     SlackNotifier{Client: c},
			"pagerduty": PagerDutyNotifier{Client: c},
			"sns":       SNSNotifier{Client: c},
			"log":       LogNotifier{Log: log},
		},
		log:      log,
		attempts: 3,
		backoff:  500 * time.Millisecond,
		sem:      make(chan struct{}, 32),
	}
}

// Register overrides or adds a notifier type.
func (d *Dispatcher) Register(kind string, n Notifier) { d.notifiers[kind] = n }

// DeliverSync delivers to one target with retries (exposed for tests).
func (d *Dispatcher) DeliverSync(ctx context.Context, t Target, tr Transition) error {
	n, ok := d.notifiers[t.Type]
	if !ok {
		return fmt.Errorf("unknown notifier type %q", t.Type)
	}
	var err error
	delay := d.backoff
	for i := 0; i < d.attempts; i++ {
		if err = n.Notify(ctx, t, tr); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
	return err
}

// Dispatch fans out asynchronously to every target.
func (d *Dispatcher) Dispatch(ctx context.Context, targets []Target, tr Transition) {
	for _, t := range targets {
		t := t
		go func() {
			d.sem <- struct{}{}
			defer func() { <-d.sem }()
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if err := d.DeliverSync(cctx, t, tr); err != nil {
				d.log.Warn("alarm notification failed", "type", t.Type, "rule", tr.RuleName, "err", err)
			}
		}()
	}
}
