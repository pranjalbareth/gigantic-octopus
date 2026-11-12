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
	"context"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

var (
	// timeToReady measures creation to the workload being available.
	// A workload that drops out of Ready and comes back is observed again.
	timeToReady = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "runway_time_to_ready_seconds",
		Help:    "Seconds from Environment creation until its workload is available.",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})

	// expiryLag is how late an expiry was acted on. It stays near zero while
	// the controller is healthy and grows by the length of any outage.
	expiryLag = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "runway_expiry_lag_seconds",
		Help:    "Seconds between an Environment's expiresAt and the reconcile that deleted it.",
		Buckets: prometheus.ExponentialBuckets(1, 4, 8),
	})

	phaseDesc = prometheus.NewDesc(
		"runway_environments",
		"Number of Environments by phase.",
		[]string{"phase"}, nil,
	)
)

// phaseCollector counts Environments by phase at scrape time, straight from
// the informer cache, so the gauge can never drift from the real objects.
type phaseCollector struct {
	reader client.Reader
}

func (c *phaseCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- phaseDesc
}

func (c *phaseCollector) Collect(ch chan<- prometheus.Metric) {
	var list runwayv1alpha1.EnvironmentList
	if err := c.reader.List(context.Background(), &list); err != nil {
		return
	}
	counts := map[runwayv1alpha1.EnvironmentPhase]float64{
		runwayv1alpha1.PhasePending:      0,
		runwayv1alpha1.PhaseProvisioning: 0,
		runwayv1alpha1.PhaseReady:        0,
		runwayv1alpha1.PhaseExpiring:     0,
		runwayv1alpha1.PhaseDeleting:     0,
	}
	for i := range list.Items {
		if p := list.Items[i].Status.Phase; p != "" {
			counts[p]++
		}
	}
	for phase, n := range counts {
		ch <- prometheus.MustNewConstMetric(phaseDesc, prometheus.GaugeValue, n, string(phase))
	}
}

// registerMetrics adds the Runway metrics to a registry. Registering twice is
// harmless, which keeps tests that build several managers simple.
func registerMetrics(reg prometheus.Registerer, reader client.Reader) {
	for _, c := range []prometheus.Collector{timeToReady, expiryLag, &phaseCollector{reader: reader}} {
		_ = reg.Register(c)
	}
}
