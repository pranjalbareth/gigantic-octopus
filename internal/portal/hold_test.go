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

package portal

import (
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
)

// seedAndHold starts a portal on a fixed address with a few environments in
// different states for the user "demo", and keeps it up for five minutes.
func seedAndHold(addr, tmpl string) {
	GinkgoHelper()
	demo := impersonate("dev:demo")
	for name, ttl := range map[string]time.Duration{"pr-1423": 8 * time.Hour, "checkout-fix": 90 * time.Minute, "load-test": 4 * time.Minute} {
		env := &runwayv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       runwayv1alpha1.EnvironmentSpec{Template: tmpl, TTL: metav1.Duration{Duration: ttl}},
		}
		Expect(demo.Create(ctx, env)).To(Succeed())
	}
	// envtest has no kubelet, so play its part for two of the three.
	for _, name := range []string{"pr-1423", "checkout-fix"} {
		key := types.NamespacedName{Namespace: "env-" + name, Name: name}
		d := &appsv1.Deployment{}
		Eventually(func() error { return k8sClient.Get(ctx, key, d) }, 10*time.Second, 100*time.Millisecond).Should(Succeed())
		d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue}}
		Expect(k8sClient.Status().Update(ctx, d)).To(Succeed())
	}

	clients, err := NewClients(portalCfg, scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())
	handler, err := (&Server{Clients: clients, Auth: DevAuth{}, Log: logr.Discard()}).Handler()
	Expect(err).NotTo(HaveOccurred())
	ln, err := net.Listen("tcp", addr)
	Expect(err).NotTo(HaveOccurred())
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()
	time.Sleep(5 * time.Minute)
}
