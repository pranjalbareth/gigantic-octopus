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
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
	"github.com/pranjalbareth/runway/internal/controller"
	webhookv1alpha1 "github.com/pranjalbareth/runway/internal/webhook/v1alpha1"
)

// This suite runs the whole product in one process: a real API server, the
// real controller, the real webhooks, the shipped admission policies and
// RBAC, and the portal on top, signed in as a user that holds nothing but
// the portal's shipped ClusterRole.

var (
	ctx       context.Context
	cancel    context.CancelFunc
	adminCfg  *rest.Config
	portalCfg *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
)

func TestPortal(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Portal Suite")
}

func configPath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "config"}, parts...)...)
}

func applyFile(parts ...string) {
	GinkgoHelper()
	f, err := os.Open(configPath(parts...))
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = f.Close() }()
	decoder := yaml.NewYAMLOrJSONDecoder(f, 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			Expect(err).To(Equal(io.EOF))
			return
		}
		if len(obj.Object) == 0 {
			continue
		}
		if obj.GetNamespace() != "" {
			obj.SetNamespace("default")
		}
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
	}
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.Background())

	Expect(runwayv1alpha1.AddToScheme(scheme.Scheme)).To(Succeed())

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{configPath("crd", "bases")},
		ErrorIfCRDPathMissing: true,
		WebhookInstallOptions: envtest.WebhookInstallOptions{Paths: []string{configPath("webhook")}},
	}
	if dir := firstEnvTestBinaryDir(); dir != "" {
		testEnv.BinaryAssetsDirectory = dir
	}

	var err error
	adminCfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	k8sClient, err = client.New(adminCfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())

	By("running the controller and the webhooks")
	opts := &testEnv.WebhookInstallOptions
	mgr, err := ctrl.NewManager(adminCfg, ctrl.Options{
		Scheme:     scheme.Scheme,
		Metrics:    metricsserver.Options{BindAddress: "0"},
		Controller: config.Controller{SkipNameValidation: new(true)},
		WebhookServer: webhook.NewServer(webhook.Options{
			Host: opts.LocalServingHost, Port: opts.LocalServingPort, CertDir: opts.LocalServingCertDir,
		}),
	})
	Expect(err).NotTo(HaveOccurred())
	Expect((&controller.EnvironmentReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager(mgr)).To(Succeed())
	Expect(webhookv1alpha1.SetupEnvironmentWebhookWithManager(mgr)).To(Succeed())
	Expect(webhookv1alpha1.SetupEnvironmentTemplateWebhookWithManager(mgr)).To(Succeed())
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(ctx)).To(Succeed())
	}()
	Eventually(func() error {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp",
			fmt.Sprintf("%s:%d", opts.LocalServingHost, opts.LocalServingPort), &tls.Config{InsecureSkipVerify: true})
		if err != nil {
			return err
		}
		return conn.Close()
	}).Should(Succeed())

	By("installing the shipped policies and RBAC")
	applyFile("admission", "environment_policy.yaml")
	applyFile("rbac", "environment_user_role.yaml")
	applyFile("portal", "rbac.yaml")

	By("giving the portal an identity that holds only its shipped ClusterRole")
	portalUser, err := testEnv.AddUser(envtest.User{Name: "portal"}, nil)
	Expect(err).NotTo(HaveOccurred())
	portalCfg = portalUser.Config()
	Expect(k8sClient.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "portal-test-identity"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "portal-impersonator"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: "portal"}},
	})).To(Succeed())

	By("waiting until both admission policies are enforced")
	Expect(k8sClient.Create(ctx, newTemplate("policy-probe"))).To(Succeed())
	probeOwner := impersonate("probe-owner")
	probe := &runwayv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "policy-probe"},
		Spec:       runwayv1alpha1.EnvironmentSpec{Template: "policy-probe", TTL: metav1.Duration{Duration: time.Hour}},
	}
	Eventually(func() error { return probeOwner.Create(ctx, probe) }, 30*time.Second, 250*time.Millisecond).Should(Succeed())
	stranger := impersonate("probe-stranger")
	Eventually(func() string {
		err := stranger.Delete(ctx, probe.DeepCopy(), client.DryRunAll)
		if err == nil {
			return ""
		}
		return err.Error()
	}, 30*time.Second, 250*time.Millisecond).Should(ContainSubstring("only the owner"))
	Eventually(func() string {
		bad := probe.DeepCopy()
		bad.ObjectMeta = metav1.ObjectMeta{Name: "x"}
		err := probeOwner.Create(ctx, bad, client.DryRunAll)
		if err == nil {
			return ""
		}
		return err.Error()
	}, 30*time.Second, 250*time.Millisecond).Should(ContainSubstring("metadata.name must be"))
})

var _ = AfterSuite(func() {
	cancel()
	Eventually(func() error { return testEnv.Stop() }, time.Minute, time.Second).Should(Succeed())
})

func impersonate(user string) client.Client {
	GinkgoHelper()
	cfg := rest.CopyConfig(adminCfg)
	cfg.Impersonate = rest.ImpersonationConfig{UserName: user}
	c, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return c
}

func firstEnvTestBinaryDir() string {
	base := filepath.Join("..", "..", "bin", "k8s")
	entries, err := os.ReadDir(base)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			return filepath.Join(base, e.Name())
		}
	}
	return ""
}
