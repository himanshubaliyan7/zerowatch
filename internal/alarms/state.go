// Package alarms implements the CloudWatch-compatible alarm engine: an M-of-N
// sliding window held in a u64 bitmask, TreatMissingData policies, the
// OK / ALARM / INSUFFICIENT_DATA state machine, a period-driven scheduler and
// notification dispatchers.
package alarms

import (
	"fmt"
	"math"
	"math/bits"
	"time"
)

// State is an alarm state.
type State string

const (
	StateOK               State = "OK"
	StateAlarm            State = "ALARM"
	StateInsufficientData State = "INSUFFICIENT_DATA"
)

// Comparison operators (CloudWatch names).
const (
	GreaterThanThreshold          = "GreaterThanThreshold"
	GreaterThanOrEqualToThreshold = "GreaterThanOrEqualToThreshold"
	LessThanThreshold             = "LessThanThreshold"
	LessThanOrEqualToThreshold    = "LessThanOrEqualToThreshold"
)

// TreatMissingData policies.
const (
	MissingBreaching    = "breaching"
	MissingNotBreaching = "notBreaching"
	MissingIgnore       = "ignore"
	MissingMissing      = "missing"
)

// SlidingAlarmState is the blueprint's M-of-N evaluator: the last N period
// outcomes are kept as bit flags in a u64 (bit 0 = most recent period).
// A parallel presence mask distinguishes "evaluated healthy" from "no data"
// so TreatMissingData=missing can yield INSUFFICIENT_DATA.
type SlidingAlarmState struct {
	HistoryBitmask    uint64 `json:"historyBitmask"` // 1 = breached
	PresenceBitmask   uint64 `json:"presenceBitmask"`
	EvaluationPeriods uint8  `json:"evaluationPeriods"` // N (1..64)
	DatapointsToAlarm uint8  `json:"datapointsToAlarm"` // M (1..N)
}

func (s *SlidingAlarmState) mask() uint64 {
	if s.EvaluationPeriods >= 64 {
		return math.MaxUint64
	}
	return (uint64(1) << s.EvaluationPeriods) - 1
}

// RecordEvaluation shifts in one period outcome and reports whether the
// M-of-N breach condition is met (exactly the blueprint algorithm, plus the
// presence mask).
func (s *SlidingAlarmState) RecordEvaluation(isBreaching bool) bool {
	return s.record(isBreaching, true)
}

// RecordMissing shifts in a period with no data (counts neither as breach nor
// as present).
func (s *SlidingAlarmState) RecordMissing() bool { return s.record(false, false) }

func (s *SlidingAlarmState) record(breach, present bool) bool {
	var b, p uint64
	if breach {
		b = 1
	}
	if present {
		p = 1
	}
	s.HistoryBitmask = s.HistoryBitmask<<1 | b
	s.PresenceBitmask = s.PresenceBitmask<<1 | p
	return s.Breaches() >= int(s.DatapointsToAlarm)
}

// Breaches returns the number of breaching periods within the window.
func (s *SlidingAlarmState) Breaches() int { return bits.OnesCount64(s.HistoryBitmask & s.mask()) }

// Present returns the number of periods with data within the window.
func (s *SlidingAlarmState) Present() int { return bits.OnesCount64(s.PresenceBitmask & s.mask()) }

// Config is the evaluation-relevant subset of an alarm rule.
type Config struct {
	Threshold         float64 `json:"threshold"`
	Comparison        string  `json:"comparison"`
	EvaluationPeriods int     `json:"evaluationPeriods"`
	DatapointsToAlarm int     `json:"datapointsToAlarm"`
	TreatMissingData  string  `json:"treatMissingData"`
}

// Validate checks the config.
func (c *Config) Validate() error {
	if c.EvaluationPeriods < 1 || c.EvaluationPeriods > 64 {
		return fmt.Errorf("evaluationPeriods must be in [1,64]")
	}
	if c.DatapointsToAlarm == 0 {
		c.DatapointsToAlarm = c.EvaluationPeriods
	}
	if c.DatapointsToAlarm < 1 || c.DatapointsToAlarm > c.EvaluationPeriods {
		return fmt.Errorf("datapointsToAlarm must be in [1,evaluationPeriods]")
	}
	switch c.Comparison {
	case GreaterThanThreshold, GreaterThanOrEqualToThreshold, LessThanThreshold, LessThanOrEqualToThreshold:
	default:
		return fmt.Errorf("unknown comparison %q", c.Comparison)
	}
	if c.TreatMissingData == "" {
		c.TreatMissingData = MissingMissing
	}
	switch c.TreatMissingData {
	case MissingBreaching, MissingNotBreaching, MissingIgnore, MissingMissing:
	default:
		return fmt.Errorf("unknown treatMissingData %q", c.TreatMissingData)
	}
	return nil
}

// Breaching applies the comparison operator.
func (c *Config) Breaching(v float64) bool {
	switch c.Comparison {
	case GreaterThanThreshold:
		return v > c.Threshold
	case GreaterThanOrEqualToThreshold:
		return v >= c.Threshold
	case LessThanThreshold:
		return v < c.Threshold
	case LessThanOrEqualToThreshold:
		return v <= c.Threshold
	}
	return false
}

// Transition is emitted whenever an alarm changes state.
type Transition struct {
	RuleID    string    `json:"ruleId"`
	RuleName  string    `json:"ruleName"`
	TenantID  uint32    `json:"tenantId"`
	From      State     `json:"from"`
	To        State     `json:"to"`
	Reason    string    `json:"reason"`
	Value     *float64  `json:"value,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// Machine is the per-alarm state machine.
type Machine struct {
	Cfg    Config
	Window SlidingAlarmState
	State  State
}

// NewMachine returns a machine in INSUFFICIENT_DATA (the CloudWatch initial state).
func NewMachine(cfg Config) (*Machine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Machine{
		Cfg:    cfg,
		Window: SlidingAlarmState{EvaluationPeriods: uint8(cfg.EvaluationPeriods), DatapointsToAlarm: uint8(cfg.DatapointsToAlarm)},
		State:  StateInsufficientData,
	}, nil
}

// Step evaluates one closed period. value == nil means no datapoint.
// Returns the new state, whether it changed, and a human readable reason.
func (m *Machine) Step(value *float64) (State, bool, string) {
	var reason string
	if value != nil && !math.IsNaN(*value) {
		br := m.Cfg.Breaching(*value)
		m.Window.RecordEvaluation(br)
		reason = fmt.Sprintf("datapoint %.4g %s %.4g: breaching=%v", *value, m.Cfg.Comparison, m.Cfg.Threshold, br)
	} else {
		switch m.Cfg.TreatMissingData {
		case MissingBreaching:
			m.Window.RecordEvaluation(true)
			reason = "missing datapoint treated as breaching"
		case MissingNotBreaching:
			m.Window.RecordEvaluation(false)
			reason = "missing datapoint treated as not breaching"
		case MissingIgnore:
			// Preserve state; the window is not advanced (CloudWatch "ignore").
			return m.State, false, "missing datapoint ignored"
		default: // missing
			m.Window.RecordMissing()
			reason = "missing datapoint"
		}
	}
	next := StateOK
	switch {
	case m.Window.Breaches() >= m.Cfg.DatapointsToAlarm:
		next = StateAlarm
		reason = fmt.Sprintf("%d of last %d datapoints breaching (>= %d); %s", m.Window.Breaches(), m.Cfg.EvaluationPeriods, m.Cfg.DatapointsToAlarm, reason)
	case m.Window.Present() == 0:
		next = StateInsufficientData
		reason = fmt.Sprintf("no datapoints in last %d periods", m.Cfg.EvaluationPeriods)
	default:
		reason = fmt.Sprintf("%d of last %d datapoints breaching (< %d); %s", m.Window.Breaches(), m.Cfg.EvaluationPeriods, m.Cfg.DatapointsToAlarm, reason)
	}
	changed := next != m.State
	m.State = next
	return next, changed, reason
}

// SimulationResult summarises a backtest of a rule over historical data.
type SimulationResult struct {
	States      []State `json:"states"`
	AlarmCount  int     `json:"alarmCount"`  // transitions into ALARM
	BreachCount int     `json:"breachCount"` // individual breaching datapoints
}

// Simulate replays a historical per-period series through a fresh machine —
// the backend for the Alarm Studio "threshold simulator".
func Simulate(cfg Config, series []*float64) (SimulationResult, error) {
	m, err := NewMachine(cfg)
	if err != nil {
		return SimulationResult{}, err
	}
	res := SimulationResult{States: make([]State, 0, len(series))}
	for _, v := range series {
		if v != nil && !math.IsNaN(*v) && m.Cfg.Breaching(*v) {
			res.BreachCount++
		}
		st, changed, _ := m.Step(v)
		if changed && st == StateAlarm {
			res.AlarmCount++
		}
		res.States = append(res.States, st)
	}
	return res, nil
}
