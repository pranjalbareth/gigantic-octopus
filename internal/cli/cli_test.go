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

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

var now = time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)

func env(name, owner string, left time.Duration) runwayv1alpha1.Environment {
	expires := metav1.NewTime(now.Add(left))
	return runwayv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       runwayv1alpha1.EnvironmentSpec{Template: "web-small", Owner: owner},
		Status:     runwayv1alpha1.EnvironmentStatus{Phase: runwayv1alpha1.PhaseReady, ExpiresAt: &expires},
	}
}

func TestLeft(t *testing.T) {
	for want, d := range map[string]time.Duration{
		"expired": -time.Second,
		"45s":     45 * time.Second,
		"2m5s":    125 * time.Second,
		"7h59m":   7*time.Hour + 59*time.Minute + 30*time.Second,
		"2d3h":    51 * time.Hour,
	} {
		if got := Left(now.Add(d), now); got != want {
			t.Errorf("Left(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestMineFiltersAndSortsBySoonestExpiry(t *testing.T) {
	all := []runwayv1alpha1.Environment{env("later", "alice", 5*time.Hour), env("bobs", "bob", time.Minute), env("sooner", "alice", time.Hour)}

	mine := Mine(all, "alice")
	if len(mine) != 2 || mine[0].Name != "sooner" || mine[1].Name != "later" {
		t.Fatalf("Mine(alice) = %v", names(mine))
	}
	if everyone := Mine(all, ""); len(everyone) != 3 || everyone[0].Name != "bobs" {
		t.Fatalf("Mine(\"\") = %v", names(everyone))
	}
}

func TestPrintList(t *testing.T) {
	var out bytes.Buffer
	if err := PrintList(&out, []runwayv1alpha1.Environment{env("pr-1423", "alice", 90*time.Minute)}, true, now); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || strings.Fields(lines[0])[3] != "LEFT" {
		t.Fatalf("unexpected table:\n%s", out.String())
	}
	if got := strings.Fields(lines[1]); strings.Join(got, " ") != "pr-1423 web-small Ready 1h30m alice" {
		t.Fatalf("row = %v", got)
	}

	out.Reset()
	_ = PrintList(&out, nil, false, now)
	if !strings.HasPrefix(out.String(), "No environments.") {
		t.Fatalf("empty list printed %q", out.String())
	}
}

func TestExtended(t *testing.T) {
	if got, err := Extended(8*time.Hour, "4h"); err != nil || got != 12*time.Hour {
		t.Fatalf("Extended = %s, %v", got, err)
	}
	for _, bad := range []string{"soon", "-1h", "0s"} {
		if _, err := Extended(time.Hour, bad); err == nil {
			t.Errorf("Extended(%q) should fail", bad)
		}
	}
}

func names(envs []runwayv1alpha1.Environment) []string {
	out := make([]string, 0, len(envs))
	for _, e := range envs {
		out = append(out, e.Name)
	}
	return out
}
