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

// Command portal serves Runway's self-service web page.
package main

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
	"github.com/pranjalbareth/runway/internal/portal"
)

func main() {
	var (
		listen  string
		auth    string
		prefix  string
		headers string
	)
	flag.StringVar(&listen, "listen", ":8080",
		"Address to serve on. With --auth=proxy use 127.0.0.1:8080 so only the sidecar proxy can reach the portal.")
	flag.StringVar(&auth, "auth", "dev",
		`How users sign in: "proxy" (trust oauth2-proxy headers) or "dev" (anyone picks a name; demos only).`)
	flag.StringVar(&prefix, "user-prefix", "github:", "Prefix for Kubernetes usernames in proxy mode.")
	flag.StringVar(&headers, "user-headers", strings.Join(portal.DefaultProxyHeaders, ","),
		"Identity headers to trust in proxy mode, first match wins.")
	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	log := ctrl.Log.WithName("portal")

	scheme := runtime.NewScheme()
	utilruntime.Must(runwayv1alpha1.AddToScheme(scheme))

	clients, err := portal.NewClients(ctrl.GetConfigOrDie(), scheme)
	if err != nil {
		log.Error(err, "Connecting to the cluster")
		os.Exit(1)
	}

	var authn portal.Authenticator
	switch auth {
	case "proxy":
		if prefix == "" {
			log.Error(nil, "--user-prefix must not be empty: without it a login name could match a cluster user")
			os.Exit(1)
		}
		authn = portal.ProxyAuth{Headers: strings.Split(headers, ","), Prefix: prefix}
	case "dev":
		log.Info("DEV LOGIN IS ON: anyone who can reach this page can sign in as anyone. Use --auth=proxy outside demos.")
		authn = portal.DevAuth{}
	default:
		log.Error(nil, "Unknown --auth value", "auth", auth)
		os.Exit(1)
	}

	handler, err := (&portal.Server{Clients: clients, Auth: authn, Log: log}).Handler()
	if err != nil {
		log.Error(err, "Building routes")
		os.Exit(1)
	}

	// No WriteTimeout: the event stream is one long response.
	srv := &http.Server{Addr: listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("Serving", "address", listen, "auth", auth)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error(err, "Serving")
		os.Exit(1)
	}
}
