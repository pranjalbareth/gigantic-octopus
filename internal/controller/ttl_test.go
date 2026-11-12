/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

func envCreatedAt(created time.Time, ttl time.Duration) *runwayv1alpha1.Environment {
	return &runwayv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "t", CreationTimestamp: metav1.NewTime(created)},
		Spec:       runwayv1alpha1.EnvironmentSpec{TTL: metav1.Duration{Duration: ttl}},
	}
}

func TestExpiry(t *testing.T) {
	created := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	env := envCreatedAt(created, 2*time.Hour)

	if got, want := ExpiresAt(env), created.Add(2*time.Hour); !got.Equal(want) {
		t.Fatalf("ExpiresAt = %s, want %s", got, want)
	}

	cases := []struct {
		name      string
		now       time.Time
		expired   bool
		remaining time.Duration
	}{
		{"at creation", created, false, 2 * time.Hour},
		{"halfway", created.Add(time.Hour), false, time.Hour},
		{"one second before", created.Add(2*time.Hour - time.Second), false, time.Second},
		{"exactly at expiry", created.Add(2 * time.Hour), true, 0},
		{"long after (controller was down)", created.Add(50 * time.Hour), true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Expired(env, tc.now); got != tc.expired {
				t.Errorf("Expired = %v, want %v", got, tc.expired)
			}
			if got := Remaining(env, tc.now); got != tc.remaining {
				t.Errorf("Remaining = %s, want %s", got, tc.remaining)
			}
		})
	}
}

func TestExtendingTTLMovesExpiry(t *testing.T) {
	created := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	env := envCreatedAt(created, time.Hour)
	now := created.Add(90 * time.Minute)

	if !Expired(env, now) {
		t.Fatal("expected a 1h environment to be expired after 90m")
	}
	env.Spec.TTL.Duration = 2 * time.Hour
	if Expired(env, now) {
		t.Fatal("expected raising spec.ttl to 2h to un-expire the environment at 90m")
	}
	if got := Remaining(env, now); got != 30*time.Minute {
		t.Fatalf("Remaining = %s, want 30m", got)
	}
}
