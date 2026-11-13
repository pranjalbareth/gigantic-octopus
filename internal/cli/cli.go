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

// Package cli holds the logic of the kubectl-runway plugin, kept apart from
// the cluster calls so it can be tested without a cluster.
package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

// Left prints how long an environment has, in the largest two units.
func Left(expires, now time.Time) string {
	d := expires.Sub(now)
	if d <= 0 {
		return "expired"
	}
	d = d.Round(time.Second)
	days, hours, mins, secs := int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60, int(d.Seconds())%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm%ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// Mine keeps the environments owned by user, soonest to expire first. An
// empty user keeps everything.
func Mine(envs []runwayv1alpha1.Environment, user string) []runwayv1alpha1.Environment {
	var out []runwayv1alpha1.Environment
	for _, e := range envs {
		if user == "" || e.Spec.Owner == user {
			out = append(out, e)
		}
	}
	slices.SortFunc(out, func(x, y runwayv1alpha1.Environment) int {
		a, b := x.Status.ExpiresAt, y.Status.ExpiresAt
		switch {
		case a == nil && b == nil:
			return strings.Compare(x.Name, y.Name)
		case a == nil:
			return 1
		case b == nil:
			return -1
		case !a.Equal(b):
			return a.Compare(b.Time)
		}
		return strings.Compare(x.Name, y.Name)
	})
	return out
}

// PrintList writes the table for `kubectl runway list`.
func PrintList(w io.Writer, envs []runwayv1alpha1.Environment, showOwner bool, now time.Time) error {
	if len(envs) == 0 {
		_, err := fmt.Fprintln(w, "No environments. Start one with: kubectl apply -f environment.yaml")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	header := []string{"NAME", "TEMPLATE", "PHASE", "LEFT"}
	if showOwner {
		header = append(header, "OWNER")
	}
	_, _ = fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, e := range envs {
		left := "-"
		if e.Status.ExpiresAt != nil {
			left = Left(e.Status.ExpiresAt.Time, now)
		}
		phase := string(e.Status.Phase)
		if phase == "" {
			phase = string(runwayv1alpha1.PhasePending)
		}
		row := []string{e.Name, e.Spec.Template, phase, left}
		if showOwner {
			row = append(row, e.Spec.Owner)
		}
		_, _ = fmt.Fprintln(tw, strings.Join(row, "\t"))
	}
	return tw.Flush()
}

// Extended returns the TTL after adding by. Extending never shortens.
func Extended(ttl time.Duration, by string) (time.Duration, error) {
	d, err := time.ParseDuration(by)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration; use something like 1h or 30m", by)
	}
	if d <= 0 {
		return 0, fmt.Errorf("extend by a positive amount; to end an environment early, delete it")
	}
	return ttl + d, nil
}
