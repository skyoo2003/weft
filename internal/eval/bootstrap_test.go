// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"errors"
	"fmt"
	"math"
	"testing"
)

// TestBootstrapIsReproducible is the property the published numbers depend on:
// `make eval` has to reprint the interval that was written down, and a bootstrap
// that drew from the clock or from map order would not.
func TestBootstrapIsReproducible(t *testing.T) {
	base, arm := make(map[string]float64), make(map[string]float64)
	for i := 0; i < 40; i++ {
		id := fmt.Sprintf("q%02d", i)
		base[id] = float64(i%7) / 10
		arm[id] = float64(i%7)/10 + float64(i%3)/100
	}

	first, err := BootstrapCI(base, arm, 2000, 42)
	if err != nil {
		t.Fatalf("BootstrapCI: %v", err)
	}
	// Repeated ten times: map iteration order is randomised per range statement,
	// so an order dependency shows up as an occasional mismatch rather than a
	// reliable one.
	for i := 0; i < 10; i++ {
		again, err := BootstrapCI(base, arm, 2000, 42)
		if err != nil {
			t.Fatalf("BootstrapCI: %v", err)
		}
		if again != first {
			t.Fatalf("run %d gave %+v, first run gave %+v — the estimate depends on something other than the seed",
				i, again, first)
		}
	}
}

func TestBootstrapZeroVariance(t *testing.T) {
	// Every query improves by exactly 0.1, so every resample has the same mean
	// and the interval collapses onto the point estimate.
	base, arm := make(map[string]float64), make(map[string]float64)
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("q%02d", i)
		base[id] = 0.4
		arm[id] = 0.5
	}

	iv, err := BootstrapCI(base, arm, 1000, 7)
	if err != nil {
		t.Fatalf("BootstrapCI: %v", err)
	}
	for _, c := range []struct {
		name string
		got  float64
	}{{"Delta", iv.Delta}, {"Lo", iv.Lo}, {"Hi", iv.Hi}} {
		if math.Abs(c.got-0.1) > 1e-12 {
			t.Errorf("%s = %v, want 0.1", c.name, c.got)
		}
	}
	if iv.ContainsZero() {
		t.Error("ContainsZero() = true for a uniform 0.1 improvement")
	}
}

// TestBootstrapNoiseContainsZero is the case the judgment rule turns on: deltas
// that cancel out must produce an interval straddling zero, so the milestone
// reads "undetermined" instead of reporting the sign of the mean.
func TestBootstrapNoiseContainsZero(t *testing.T) {
	base, arm := make(map[string]float64), make(map[string]float64)
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("q%02d", i)
		base[id] = 0.5
		if i%2 == 0 {
			arm[id] = 0.6
		} else {
			arm[id] = 0.4
		}
	}

	iv, err := BootstrapCI(base, arm, 5000, 1)
	if err != nil {
		t.Fatalf("BootstrapCI: %v", err)
	}
	if math.Abs(iv.Delta) > 1e-12 {
		t.Errorf("Delta = %v, want 0", iv.Delta)
	}
	if !iv.ContainsZero() {
		t.Errorf("ContainsZero() = false for [%v, %v] around a zero mean", iv.Lo, iv.Hi)
	}
	if iv.Lo >= iv.Hi {
		t.Errorf("Lo %v is not below Hi %v — the resampling produced no spread", iv.Lo, iv.Hi)
	}
}

func TestBootstrapIntervalBracketsDelta(t *testing.T) {
	base, arm := make(map[string]float64), make(map[string]float64)
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("q%02d", i)
		base[id] = float64(i) / 40
		arm[id] = float64(i)/40 + float64(i%5)/50
	}

	iv, err := BootstrapCI(base, arm, 4000, 99)
	if err != nil {
		t.Fatalf("BootstrapCI: %v", err)
	}
	if iv.Lo > iv.Delta || iv.Delta > iv.Hi {
		t.Errorf("Delta %v is outside [%v, %v]", iv.Delta, iv.Lo, iv.Hi)
	}
	if iv.Queries != 30 || iv.Iters != 4000 {
		t.Errorf("Queries=%d Iters=%d, want 30 and 4000", iv.Queries, iv.Iters)
	}
}

func TestBootstrapRejectsUnpairedRuns(t *testing.T) {
	tests := []struct {
		name      string
		base, arm map[string]float64
		iters     int
		wantErr   error
	}{
		{
			name:    "different query counts",
			base:    map[string]float64{"a": 1, "b": 1},
			arm:     map[string]float64{"a": 1},
			iters:   100,
			wantErr: ErrUnpaired,
		},
		{
			name:    "same count, different ids",
			base:    map[string]float64{"a": 1, "b": 1},
			arm:     map[string]float64{"a": 1, "c": 1},
			iters:   100,
			wantErr: ErrUnpaired,
		},
		{
			name:    "empty",
			base:    nil,
			arm:     nil,
			iters:   100,
			wantErr: ErrUnpaired,
		},
		{
			name:    "no iterations",
			base:    map[string]float64{"a": 1},
			arm:     map[string]float64{"a": 1},
			iters:   0,
			wantErr: ErrNoIters,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BootstrapCI(tc.base, tc.arm, tc.iters, 1)
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestPercentileIndexIsNearestRank pins the definition rather than the values it
// happens to produce. Nearest rank is the ⌈p·n⌉-th value counting from 1, so the
// zero-based index is one less; the n = 10000 rows are the two percentiles the
// harness actually runs, and are where truncating p·n silently picked the next order
// statistic.
func TestPercentileIndexIsNearestRank(t *testing.T) {
	for _, tc := range []struct {
		n    int
		p    float64
		want int
	}{
		{10000, 0.025, 249},  // 250th value, not the 251st.
		{10000, 0.975, 9749}, // 9750th value, not the 9751st.
		{2000, 0.025, 49},
		{2000, 0.975, 1949},
		{100, 0.5, 49},
		{40, 0.025, 0}, // ⌈1⌉ − 1 = 0, in range without clamping.
		{20, 0.025, 0}, // ⌈0.5⌉ − 1 = 0, and the clamp is not what produced it.
		{3, 0.999, 2},  // ⌈2.997⌉ − 1 = 2.
		{3, 1.0, 2},    // The maximum, exactly.
		{1, 0.025, 0},  // Degenerate, held by the low clamp.
		{1, 0.975, 0},  // Degenerate, held by the high clamp.
	} {
		if got := percentileIndex(tc.n, tc.p); got != tc.want {
			t.Errorf("percentileIndex(%d, %v) = %d, want %d", tc.n, tc.p, got, tc.want)
		}
	}
}

// TestBootstrapSingleQueryDoesNotIndexOutOfRange covers the clamp in
// percentileIndex: one query and one iteration is a degenerate call, and it has
// to return the degenerate answer rather than panic.
func TestBootstrapSingleQueryDoesNotIndexOutOfRange(t *testing.T) {
	iv, err := BootstrapCI(map[string]float64{"a": 0.2}, map[string]float64{"a": 0.5}, 1, 3)
	if err != nil {
		t.Fatalf("BootstrapCI: %v", err)
	}
	if math.Abs(iv.Delta-0.3) > 1e-12 || math.Abs(iv.Lo-0.3) > 1e-12 || math.Abs(iv.Hi-0.3) > 1e-12 {
		t.Errorf("got %+v, want Delta/Lo/Hi all 0.3", iv)
	}
}
