// Command spot-vm-cleaner is a long-lived controller that removes k3s node
// objects whose backing Rackspace Spot VM is gone, after a grace period.
//
// See README.md for the cloud-init label requirement and deployment steps.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/cleaner"
	"github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/config"
	k8sclient "github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/k8s"
	"github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/spot"
	v1 "github.com/rackspace-spot/spot-go-sdk/api/v1"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "err", err.Error())
		os.Exit(2)
	}

	slog.Info("starting rackspace-spot-vm-cloudspace-k8s-cleaner",
		"org", cfg.SpotOrg,
		"vm_pool_label", cfg.VMPoolLabel,
		"match_by", cfg.MatchBy,
		"clean_interval", cfg.CleanInterval.String(),
		"grace_ticks", cfg.GraceTicks,
		"dry_run", cfg.DryRun)

	// Build the Spot SDK client.
	spotClient, err := v1.NewSpotClient(&v1.Config{
		RefreshToken: cfg.SpotRefreshToken,
		BaseURL:      cfg.SpotBaseURL,
		OAuthURL:     cfg.SpotAuthURL,
	})
	if err != nil {
		slog.Error("failed to create spot client", "err", err.Error())
		os.Exit(1)
	}

	// Authenticate once at startup; subsequent calls reuse/refresh the token
	// as needed by the SDK.
	if _, err := spotClient.Authenticate(context.Background()); err != nil {
		slog.Error("spot authentication failed", "err", err.Error())
		os.Exit(1)
	}

	spotC := &spot.Client{Spot: spotClient, Org: cfg.SpotOrg, MatchBy: cfg.MatchBy}

	// Build the in-cluster k8s client (we run as a ServiceAccount in k3s).
	kClient, err := k8sclient.NewInCluster(cfg.VMPoolLabel, cfg.MatchBy)
	if err != nil {
		slog.Error("failed to create kubernetes client", "err", err.Error())
		os.Exit(1)
	}

	cln := cleaner.New(spotC, kClient, cfg.GraceTicks, cfg.DryRun)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tick := time.NewTicker(cfg.CleanInterval)
	defer tick.Stop()

	// Run one reconcile immediately on startup, then on each tick.
	//
	// Any reconcile error (Spot API failure, RBAC "permission denied" on nodes,
	// a failed delete) is fatal: we exit non-zero and let the Pod restart
	// policy handle it, rather than looping forever in a broken state.
	run := func() {
		start := time.Now()
		res, err := cln.Reconcile(ctx, start)
		if err != nil {
			if ctx.Err() != nil {
				// Shutting down: the in-flight request was cancelled by SIGTERM.
				slog.Info("reconcile aborted by shutdown", "err", err.Error())
				return
			}
			slog.Error("reconcile failed", "err", err.Error(), "duration", time.Since(start).String())
			os.Exit(1)
		}
		slog.Info("reconcile complete",
			"managed_nodes", res.ManagedNodes,
			"evaluated", res.Evaluated,
			"deleted", res.Deleted,
			"dry_run", cfg.DryRun,
			"duration", time.Since(start).String())
	}

	run()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down", "reason", ctx.Err().Error())
			if errors.Is(ctx.Err(), context.Canceled) {
				return
			}
			return
		case <-tick.C:
			run()
		}
	}
}
