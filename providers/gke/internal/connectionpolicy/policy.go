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
	"fmt"
	"time"
)

const (
	MinimumSeconds = int64(600)
	DefaultSeconds = int64(24 * 60 * 60)
	MaximumSeconds = int64(7 * 24 * 60 * 60)
	LimitSeconds   = int64(30 * 24 * 60 * 60)
)

var ErrDuration = errors.New("invalid connection duration")

type Policy struct {
	DefaultSeconds int64 `json:"defaultSeconds"`
	MaxSeconds     int64 `json:"maxSeconds"`
}

func (policy Policy) Resolve() (Policy, error) {
	if policy.DefaultSeconds == 0 {
		policy.DefaultSeconds = DefaultSeconds
	}
	if policy.MaxSeconds == 0 {
		policy.MaxSeconds = MaximumSeconds
	}
	if policy.DefaultSeconds < MinimumSeconds || policy.MaxSeconds > LimitSeconds || policy.DefaultSeconds > policy.MaxSeconds {
		return Policy{}, fmt.Errorf("connection lifetimes must satisfy %d <= default <= maximum <= %d seconds", MinimumSeconds, LimitSeconds)
	}
	return policy, nil
}

func (policy Policy) Duration(seconds int64) (time.Duration, error) {
	resolved, err := policy.Resolve()
	if err != nil {
		return 0, err
	}
	if seconds == 0 {
		seconds = resolved.DefaultSeconds
	}
	if seconds < MinimumSeconds || seconds > resolved.MaxSeconds {
		return 0, fmt.Errorf("%w: must be between %d and %d seconds", ErrDuration, MinimumSeconds, resolved.MaxSeconds)
	}
	return time.Duration(seconds) * time.Second, nil
}
