package duration_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jahrulnr/fakeagents/internal/duration"
)

func TestDurationUnmarshal(t *testing.T) {
	var value duration.Duration
	if err := json.Unmarshal([]byte(`"250ms"`), &value); err != nil {
		t.Fatalf("string: %v", err)
	}
	if value.D() != 250*time.Millisecond {
		t.Fatalf("value = %s", value)
	}
	if err := json.Unmarshal([]byte(`1500`), &value); err != nil {
		t.Fatalf("number: %v", err)
	}
	if value.D() != 1500*time.Millisecond {
		t.Fatalf("value = %s", value)
	}
	if err := json.Unmarshal([]byte(`"nope"`), &value); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestRangeUnmarshal(t *testing.T) {
	tests := []struct {
		input string
		min   time.Duration
		max   time.Duration
	}{
		{`"100ms"`, 100 * time.Millisecond, 100 * time.Millisecond},
		{`{"min":"10ms","max":"20ms"}`, 10 * time.Millisecond, 20 * time.Millisecond},
	}
	for _, test := range tests {
		var value duration.Range
		if err := json.Unmarshal([]byte(test.input), &value); err != nil {
			t.Fatalf("%s: %v", test.input, err)
		}
		if value.Min.D() != test.min || value.Max.D() != test.max {
			t.Fatalf("%s = %+v", test.input, value)
		}
	}
}

func TestNumberRangeUnmarshal(t *testing.T) {
	var value duration.NumberRange
	if err := json.Unmarshal([]byte(`50`), &value); err != nil {
		t.Fatalf("number: %v", err)
	}
	if value.Min != 50 || value.Max != 50 {
		t.Fatalf("value = %+v", value)
	}
	if err := json.Unmarshal([]byte(`{"min":10,"max":20}`), &value); err != nil {
		t.Fatalf("object: %v", err)
	}
	if value.Min != 10 || value.Max != 20 {
		t.Fatalf("value = %+v", value)
	}
}
