// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connectionpolicy

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestPolicy(t *testing.T) {
	resolved, err := (Policy{}).Resolve()
	if err != nil || resolved.DefaultSeconds != DefaultSeconds || resolved.MaxSeconds != MaximumSeconds {
		t.Fatalf("unexpected defaults: %+v, %v", resolved, err)
	}
	for _, policy := range []Policy{{DefaultSeconds: -1}, {DefaultSeconds: 599}, {MaxSeconds: -1}, {DefaultSeconds: 3600, MaxSeconds: 600}, {MaxSeconds: LimitSeconds + 1}, {DefaultSeconds: math.MaxInt64}} {
		if _, err := policy.Resolve(); err == nil {
			t.Fatalf("invalid policy accepted: %+v", policy)
		}
	}
	policy := Policy{DefaultSeconds: 3600, MaxSeconds: LimitSeconds}
	for _, seconds := range []int64{0, 600, 3600, 86400, 604800, 2592000} {
		duration, err := policy.Duration(seconds)
		if seconds == 0 {
			seconds = 3600
		}
		if err != nil || duration != time.Duration(seconds)*time.Second {
			t.Fatalf("duration %d: %v, %v", seconds, duration, err)
		}
	}
	for _, seconds := range []int64{-1, 599, LimitSeconds + 1, math.MaxInt64} {
		if _, err := policy.Duration(seconds); !errors.Is(err, ErrDuration) {
			t.Fatalf("invalid duration accepted: %d, %v", seconds, err)
		}
	}
}
