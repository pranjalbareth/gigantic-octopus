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
	"time"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

// ExpiresAt is the one place expiry is defined: creation time plus spec.ttl.
//
// It is a pure function of the object as stored in the API server. Nothing is
// remembered between reconciles, so a controller restart cannot lose or skip
// an expiry. Extending an environment means raising spec.ttl.
func ExpiresAt(env *runwayv1alpha1.Environment) time.Time {
	return env.CreationTimestamp.Add(env.Spec.TTL.Duration)
}

// Expired reports whether the environment's TTL has run out at the given time.
func Expired(env *runwayv1alpha1.Environment, now time.Time) bool {
	return !now.Before(ExpiresAt(env))
}

// Remaining is how long until expiry. It is never negative.
func Remaining(env *runwayv1alpha1.Environment, now time.Time) time.Duration {
	if d := ExpiresAt(env).Sub(now); d > 0 {
		return d
	}
	return 0
}
