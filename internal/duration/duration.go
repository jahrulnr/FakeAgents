// Package duration provides JSON/YAML friendly duration and numeric range
// types used throughout FakeAgents configuration.
package duration

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Duration is a time.Duration that marshals as a human readable string such as
// "250ms" or "2s". When unmarshalling, a bare number is interpreted as
// milliseconds.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON implements json.Marshaler.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON implements json.Unmarshaler.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	parsed, err := parseDuration(v)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

// MarshalYAML implements yaml.Marshaler.
func (d Duration) MarshalYAML() (any, error) { return time.Duration(d).String(), nil }

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	parsed, err := parseDuration(v)
	if err != nil {
		return err
	}
	*d = parsed
	return nil
}

func parseDuration(v any) (Duration, error) {
	switch value := v.(type) {
	case string:
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return 0, nil
		}
		parsed, err := time.ParseDuration(trimmed)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", value, err)
		}
		return Duration(parsed), nil
	case float64:
		return Duration(time.Duration(value * float64(time.Millisecond))), nil
	case int:
		return Duration(time.Duration(value) * time.Millisecond), nil
	case int64:
		return Duration(time.Duration(value) * time.Millisecond), nil
	default:
		return 0, fmt.Errorf("invalid duration value %v (%T)", v, v)
	}
}

// Range is a min/max duration range. A single duration string is accepted as a
// fixed value.
type Range struct {
	Min Duration `json:"min"`
	Max Duration `json:"max"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (r *Range) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	return r.fromAny(v)
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (r *Range) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	return r.fromAny(v)
}

func (r *Range) fromAny(v any) error {
	if m, ok := v.(map[string]any); ok {
		minV, hasMin := m["min"]
		maxV, hasMax := m["max"]
		if !hasMin && !hasMax {
			return fmt.Errorf("duration range requires min and/or max")
		}
		if hasMin {
			parsed, err := parseDuration(minV)
			if err != nil {
				return err
			}
			r.Min = parsed
		}
		if hasMax {
			parsed, err := parseDuration(maxV)
			if err != nil {
				return err
			}
			r.Max = parsed
		}
		if r.Max < r.Min {
			return fmt.Errorf("duration range max %s is smaller than min %s", r.Max, r.Min)
		}
		return nil
	}
	parsed, err := parseDuration(v)
	if err != nil {
		return err
	}
	r.Min = parsed
	r.Max = parsed
	return nil
}

// Pick returns a random duration within the range. A nil RNG is allowed.
func (r Range) Pick(rng *rand.Rand) time.Duration {
	if r.Max <= r.Min {
		return r.Min.D()
	}
	span := int64(r.Max.D() - r.Min.D())
	if rng == nil {
		return r.Min.D() + time.Duration(span/2)
	}
	return r.Min.D() + time.Duration(rng.Int64N(span+1))
}

// MarshalJSON implements json.Marshaler.
func (r Range) MarshalJSON() ([]byte, error) {
	if r.Min == r.Max {
		return json.Marshal(r.Min)
	}
	type alias Range
	return json.Marshal(alias(r))
}

// NumberRange is a min/max float range. A bare number is accepted as a fixed
// value.
type NumberRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// UnmarshalJSON implements json.Unmarshaler.
func (n *NumberRange) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	return n.fromAny(v)
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (n *NumberRange) UnmarshalYAML(unmarshal func(any) error) error {
	var v any
	if err := unmarshal(&v); err != nil {
		return err
	}
	return n.fromAny(v)
}

func (n *NumberRange) fromAny(v any) error {
	if m, ok := v.(map[string]any); ok {
		minV, hasMin := m["min"]
		maxV, hasMax := m["max"]
		if !hasMin && !hasMax {
			return fmt.Errorf("number range requires min and/or max")
		}
		if hasMin {
			f, err := toFloat(minV)
			if err != nil {
				return err
			}
			n.Min = f
		}
		if hasMax {
			f, err := toFloat(maxV)
			if err != nil {
				return err
			}
			n.Max = f
		}
		if n.Max < n.Min {
			return fmt.Errorf("number range max %v is smaller than min %v", n.Max, n.Min)
		}
		return nil
	}
	f, err := toFloat(v)
	if err != nil {
		return err
	}
	n.Min = f
	n.Max = f
	return nil
}

func toFloat(v any) (float64, error) {
	switch value := v.(type) {
	case float64:
		return value, nil
	case int:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case string:
		return strconv.ParseFloat(strings.TrimSpace(value), 64)
	default:
		return 0, fmt.Errorf("invalid number %v (%T)", v, v)
	}
}

// Pick returns a random value within the range. A nil RNG is allowed.
func (n NumberRange) Pick(rng *rand.Rand) float64 {
	if n.Max <= n.Min {
		return n.Min
	}
	if rng == nil {
		return n.Min + (n.Max-n.Min)/2
	}
	return n.Min + rng.Float64()*(n.Max-n.Min)
}

// MarshalJSON implements json.Marshaler.
func (n NumberRange) MarshalJSON() ([]byte, error) {
	if n.Min == n.Max {
		return json.Marshal(n.Min)
	}
	type alias NumberRange
	return json.Marshal(alias(n))
}
