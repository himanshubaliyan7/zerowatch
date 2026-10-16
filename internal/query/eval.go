package query

import (
	"fmt"
	"math"
	"strings"
)

// Value is either a scalar or a vector aligned to the request grid.
type Value struct {
	Scalar   float64
	Vec      []float64
	IsScalar bool
}

// ScalarV constructs a scalar value.
func ScalarV(f float64) Value { return Value{Scalar: f, IsScalar: true} }

// VecV constructs a vector value.
func VecV(v []float64) Value { return Value{Vec: v} }

// Materialize broadcasts a scalar to n points.
func (v Value) Materialize(n int) []float64 {
	if !v.IsScalar {
		return v.Vec
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = v.Scalar
	}
	return out
}

// EvalError is a semantic evaluation failure.
type EvalError struct{ Msg string }

func (e *EvalError) Error() string { return e.Msg }

func evalErr(format string, a ...any) error { return &EvalError{fmt.Sprintf(format, a...)} }

// Env holds already-computed query results plus the grid.
type Env struct {
	Grid   Grid
	Values map[string]Value
}

// Eval evaluates n against env. All vector kernels are tight loops over
// contiguous []float64 so the Go compiler can bounds-check-eliminate and the
// CPU can pipeline; NaN represents a missing datapoint and propagates.
func Eval(n Node, env *Env) (Value, error) {
	switch x := n.(type) {
	case *Num:
		return ScalarV(x.Value), nil
	case *Ref:
		v, ok := env.Values[x.ID]
		if !ok {
			return Value{}, evalErr("unknown reference %q", x.ID)
		}
		return v, nil
	case *Unary:
		v, err := Eval(x.X, env)
		if err != nil {
			return Value{}, err
		}
		return mapV(v, func(f float64) float64 { return -f }), nil
	case *Binary:
		l, err := Eval(x.L, env)
		if err != nil {
			return Value{}, err
		}
		r, err := Eval(x.R, env)
		if err != nil {
			return Value{}, err
		}
		return binary(x.Op, l, r, env.Grid.N)
	case *Call:
		return call(x, env)
	}
	return Value{}, evalErr("unsupported node %T", n)
}

func opFn(op string) func(a, b float64) float64 {
	switch op {
	case "+":
		return func(a, b float64) float64 { return a + b }
	case "-":
		return func(a, b float64) float64 { return a - b }
	case "*":
		return func(a, b float64) float64 { return a * b }
	case "/":
		// CloudWatch returns no datapoint for division by zero.
		return func(a, b float64) float64 {
			if b == 0 {
				return math.NaN()
			}
			return a / b
		}
	case "^":
		return math.Pow
	}
	return nil
}

func binary(op string, l, r Value, n int) (Value, error) {
	f := opFn(op)
	if f == nil {
		return Value{}, evalErr("unknown operator %q", op)
	}
	if l.IsScalar && r.IsScalar {
		return ScalarV(f(l.Scalar, r.Scalar)), nil
	}
	a, b := l.Materialize(n), r.Materialize(n)
	if len(a) != len(b) {
		return Value{}, evalErr("vector length mismatch %d vs %d", len(a), len(b))
	}
	out := make([]float64, len(a))
	for i := range out {
		out[i] = f(a[i], b[i])
	}
	return VecV(out), nil
}

func mapV(v Value, f func(float64) float64) Value {
	if v.IsScalar {
		return ScalarV(f(v.Scalar))
	}
	out := make([]float64, len(v.Vec))
	for i, x := range v.Vec {
		out[i] = f(x)
	}
	return VecV(out)
}

func reduce(vals []float64, kind string) float64 {
	acc, n := 0.0, 0
	for _, v := range vals {
		if math.IsNaN(v) {
			continue
		}
		switch {
		case n == 0:
			acc = v
		case kind == "MIN":
			acc = math.Min(acc, v)
		case kind == "MAX":
			acc = math.Max(acc, v)
		default:
			acc += v
		}
		n++
	}
	if n == 0 {
		return math.NaN()
	}
	if kind == "AVG" {
		return acc / float64(n)
	}
	return acc
}

func call(c *Call, env *Env) (Value, error) {
	n := env.Grid.N
	argc := func(min, max int) error {
		if len(c.Args) < min || (max >= 0 && len(c.Args) > max) {
			return evalErr("%s: wrong number of arguments (%d)", c.Name, len(c.Args))
		}
		return nil
	}
	switch c.Name {
	case "AVG", "SUM", "MIN", "MAX":
		if err := argc(1, -1); err != nil {
			return Value{}, err
		}
		args := make([]Value, len(c.Args))
		for i, a := range c.Args {
			v, err := Eval(a, env)
			if err != nil {
				return Value{}, err
			}
			args[i] = v
		}
		if len(args) == 1 {
			// single series: reduce over time -> scalar (CloudWatch semantics)
			if args[0].IsScalar {
				return args[0], nil
			}
			return ScalarV(reduce(args[0].Vec, c.Name)), nil
		}
		// multiple series: element-wise across series
		mats := make([][]float64, len(args))
		for i, a := range args {
			mats[i] = a.Materialize(n)
		}
		out := make([]float64, n)
		col := make([]float64, len(mats))
		for t := 0; t < n; t++ {
			for s := range mats {
				col[s] = mats[s][t]
			}
			out[t] = reduce(col, c.Name)
		}
		return VecV(out), nil
	case "RATE", "DIFF":
		if err := argc(1, 1); err != nil {
			return Value{}, err
		}
		v, err := Eval(c.Args[0], env)
		if err != nil {
			return Value{}, err
		}
		if v.IsScalar {
			return ScalarV(0), nil
		}
		out := make([]float64, len(v.Vec))
		prevI := -1
		for i, x := range v.Vec {
			out[i] = math.NaN()
			if math.IsNaN(x) {
				continue
			}
			if prevI >= 0 {
				d := x - v.Vec[prevI]
				if c.Name == "RATE" {
					secs := float64(int64(i-prevI)*env.Grid.PeriodMs) / 1000
					d /= secs
				}
				out[i] = d
			}
			prevI = i
		}
		return VecV(out), nil
	case "FILL":
		if err := argc(2, 2); err != nil {
			return Value{}, err
		}
		v, err := Eval(c.Args[0], env)
		if err != nil {
			return Value{}, err
		}
		var pol FillPolicy
		if r, ok := c.Args[1].(*Ref); ok && fillKeywords[strings.ToUpper(r.ID)] {
			pol, _ = ParseFillPolicy(r.ID)
		} else {
			fv, err := Eval(c.Args[1], env)
			if err != nil {
				return Value{}, err
			}
			if !fv.IsScalar {
				// fill from another series point-wise
				src := v.Materialize(n)
				out := make([]float64, n)
				for i := range out {
					out[i] = src[i]
					if math.IsNaN(out[i]) {
						out[i] = fv.Vec[i]
					}
				}
				return VecV(out), nil
			}
			pol = FillPolicy{Kind: "VALUE", Value: fv.Scalar}
		}
		if v.IsScalar {
			return v, nil
		}
		return VecV(Fill(v.Vec, pol)), nil
	case "PERIOD":
		if err := argc(1, 1); err != nil {
			return Value{}, err
		}
		if _, err := Eval(c.Args[0], env); err != nil {
			return Value{}, err
		}
		return ScalarV(float64(env.Grid.PeriodMs) / 1000), nil
	case "ABS", "CEIL", "FLOOR", "SQRT", "LOG", "LOG10":
		if err := argc(1, 1); err != nil {
			return Value{}, err
		}
		v, err := Eval(c.Args[0], env)
		if err != nil {
			return Value{}, err
		}
		fns := map[string]func(float64) float64{"ABS": math.Abs, "CEIL": math.Ceil, "FLOOR": math.Floor, "SQRT": math.Sqrt, "LOG": math.Log, "LOG10": math.Log10}
		return mapV(v, fns[c.Name]), nil
	}
	return Value{}, evalErr("unknown function %s", c.Name)
}
