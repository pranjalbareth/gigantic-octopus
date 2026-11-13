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

// Command kubectl-runway is a kubectl plugin for Runway. Put the binary on
// your PATH and run `kubectl runway list`.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	runwayv1alpha1 "github.com/pranjalbareth/runway/api/v1alpha1"
	"github.com/pranjalbareth/runway/internal/cli"
)

const usage = `Manage Runway environments.

Usage:
  kubectl runway list [--all]            Your environments, soonest to expire first
  kubectl runway extend NAME DURATION    Add time, for example: extend pr-1423 4h
  kubectl runway describe NAME           Status, expiry and how to reach it
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Print(usage)
		return nil
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(runwayv1alpha1.AddToScheme(scheme))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return err
	}
	kube, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch args[0] {
	case "list":
		all := len(args) > 1 && args[1] == "--all"
		user := ""
		if !all {
			// Ask the API server who we are, the same identity the webhook
			// recorded as owner.
			review := &authenticationv1.SelfSubjectReview{}
			if err := kube.Create(ctx, review); err != nil {
				return fmt.Errorf("finding out who you are: %w", err)
			}
			user = review.Status.UserInfo.Username
		}
		var envs runwayv1alpha1.EnvironmentList
		if err := kube.List(ctx, &envs); err != nil {
			return err
		}
		return cli.PrintList(os.Stdout, cli.Mine(envs.Items, user), all, time.Now())

	case "extend":
		if len(args) != 3 {
			return fmt.Errorf("usage: kubectl runway extend NAME DURATION")
		}
		env := &runwayv1alpha1.Environment{}
		if err := kube.Get(ctx, types.NamespacedName{Name: args[1]}, env); err != nil {
			return err
		}
		ttl, err := cli.Extended(env.Spec.TTL.Duration, args[2])
		if err != nil {
			return err
		}
		patch := client.MergeFrom(env.DeepCopy())
		env.Spec.TTL.Duration = ttl
		if err := kube.Patch(ctx, env, patch); err != nil {
			return err
		}
		fmt.Printf("%s now lives %s from creation, until %s.\n", env.Name, ttl,
			env.CreationTimestamp.Add(ttl).Local().Format("Mon 15:04"))
		return nil

	case "describe":
		if len(args) != 2 {
			return fmt.Errorf("usage: kubectl runway describe NAME")
		}
		env := &runwayv1alpha1.Environment{}
		if err := kube.Get(ctx, types.NamespacedName{Name: args[1]}, env); err != nil {
			return err
		}
		fmt.Printf("Name:       %s\nOwner:      %s\nTemplate:   %s\nPhase:      %s\n",
			env.Name, env.Spec.Owner, env.Spec.Template, env.Status.Phase)
		for _, c := range env.Status.Conditions {
			fmt.Printf("Ready:      %s (%s)\n", c.Status, c.Message)
		}
		if env.Status.ExpiresAt != nil {
			expires := env.Status.ExpiresAt.Time
			fmt.Printf("Expires:    %s (%s left)\n", expires.Local().Format(time.RFC1123), cli.Left(expires, time.Now()))
		}
		if env.Status.Namespace != "" {
			fmt.Printf("Namespace:  %s\nURL:        %s\nReach it:   kubectl port-forward -n %s service/%s 8080:<port>\n",
				env.Status.Namespace, env.Status.URL, env.Status.Namespace, env.Name)
		}
		return nil
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
}
