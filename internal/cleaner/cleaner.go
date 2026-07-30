// Package cleaner contains the reconcile loop: discover alive Spot VMs,
// find spot-managed k3s nodes whose backing VM is gone AND that are NotReady,
// and delete those node objects after a configurable grace period.
package cleaner

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/k8s"
	"github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/spot"
)

// Cleaner ties together the Spot and Kubernetes clients and owns the
// per-node "stale" counters used to enforce the grace period.
type Cleaner struct {
	Spot       *spot.Client
	K8s        *k8s.Client
	GraceTicks int
	DryRun     bool

	// stale[nodeName] = number of consecutive ticks the node was observed as
	// (VM gone AND NotReady). Reset to 0 on any healthy/alive observation.
	stale map[string]int
}

// Result summarises one reconcile tick for logging/observability.
type Result struct {
	ManagedNodes int
	Deleted      int
	Evaluated    int // nodes that were gone+NotReady this tick
}

// New constructs a Cleaner.
func New(s *spot.Client, k *k8s.Client, graceTicks int, dryRun bool) *Cleaner {
	return &Cleaner{
		Spot:       s,
		K8s:        k,
		GraceTicks: graceTicks,
		DryRun:     dryRun,
		stale:      make(map[string]int),
	}
}

// Reconcile performs one tick. Any error — Spot API failure, node listing
// failure, or a node delete rejected by the API server (e.g. RBAC "permission
// denied") — is returned immediately and is fatal to the caller: a cleaner
// that cannot see or act on the cluster must not keep running silently.
func (c *Cleaner) Reconcile(ctx context.Context, now time.Time) (Result, error) {
	res := Result{}

	alive, err := c.Spot.AliveVMsByPool(ctx)
	if err != nil {
		return res, err
	}

	nodes, err := c.K8s.ListSpotNodes(ctx)
	if err != nil {
		return res, err
	}
	res.ManagedNodes = len(nodes)

	// Set of node names still relevant this tick (so we can forget stale
	// entries for nodes that were deleted externally or lost their label).
	seen := make(map[string]struct{}, len(nodes))

	for _, n := range nodes {
		seen[n.Name] = struct{}{}

		// alive[n.Pool] returns nil for pools with no live VMs; indexing a nil map
		// yields a zero-value false, so a node in such a pool is treated as
		// "VM gone". The NotReady gate below still applies before deletion.
		_, vmAlive := alive[n.Pool][spot.Identity(n.Identity)]

		if vmAlive || !n.NotReady {
			// Either the VM still exists or the node is Ready (or both).
			// Either way, it is not a deletion candidate right now.
			c.stale[n.Name] = 0
			continue
		}

		// Candidate: VM gone AND node NotReady.
		res.Evaluated++
		c.stale[n.Name]++

		if c.stale[n.Name] < c.GraceTicks {
			slog.Info("node candidate not yet eligible",
				"node", n.Name, "pool", n.Pool, "identity", n.Identity,
				"stale", c.stale[n.Name], "grace", c.GraceTicks, "dry_run", c.DryRun)
			continue
		}

		slog.Info("deleting stale spot node",
			"node", n.Name, "pool", n.Pool, "identity", n.Identity,
			"stale", c.stale[n.Name], "dry_run", c.DryRun)

		if c.DryRun {
			delete(c.stale, n.Name)
			continue
		}

		if err := c.K8s.DeleteNode(ctx, n.Name); err != nil {
			return res, fmt.Errorf("delete node %q (pool %q): %w", n.Name, n.Pool, err)
		}
		res.Deleted++
		delete(c.stale, n.Name)
	}

	// Forget stale entries for nodes we no longer see (deleted out-of-band or
	// label removed).
	for name := range c.stale {
		if _, ok := seen[name]; !ok {
			delete(c.stale, name)
		}
	}

	return res, nil
}
