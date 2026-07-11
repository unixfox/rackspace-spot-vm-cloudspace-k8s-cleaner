// Package spot wraps the Rackspace Spot SDK to provide the alive-VM set
// needed by the cleaner, keyed by VM pool name.
package spot

import (
	"context"
	"fmt"

	v1 "github.com/rackspace-spot/spot-go-sdk/api/v1"
)

// Identity is either the Spot VM server name (matchBy == "name") or its IP
// address (matchBy == "ip"). It is the value compared against a k3s node's
// identity to decide whether that node still has a live backing VM.
type Identity string

// AliveMap maps a VM pool name to the set of alive VM identities in that pool.
type AliveMap map[string]map[Identity]struct{}

// ListVMCloudSpaceser is the subset of the Spot SDK client the cleaner needs.
// Declared as an interface so tests can substitute a fake.
type ListVMCloudSpaceser interface {
	ListVMCloudSpaces(ctx context.Context, org string) (*v1.VMCloudSpaceList, error)
}

// Client wraps a Spot SDK client and the org to query.
type Client struct {
	Spot    ListVMCloudSpaceser
	Org     string
	MatchBy string
}

// AliveVMsByPool queries all VM Cloudspaces in the org and returns, per pool,
// the set of alive VM identities. A VM is considered part of a pool via
// VMAssignedServer.NodePoolName. The identity used for matching is chosen by
// MatchBy: "name" -> ServerName, "ip" -> IPAddress.
//
// On any error from the Spot API,AliveVMsByPool returns (nil, error) so the
// caller can skip the tick entirely rather than risk wrongful deletions.
func (c *Client) AliveVMsByPool(ctx context.Context) (AliveMap, error) {
	if c.Org == "" {
		return nil, fmt.Errorf("spot: org is required")
	}
	list, err := c.Spot.ListVMCloudSpaces(ctx, c.Org)
	if err != nil {
		return nil, fmt.Errorf("spot: list VM cloudspaces for org %q: %w", c.Org, err)
	}
	if list == nil {
		return AliveMap{}, nil
	}

	out := AliveMap{}
	for _, vmcs := range list.Items {
		for serverKey, srv := range vmcs.AssignedServers {
			pool := srv.NodePoolName
			if pool == "" {
				// Fall back to scanning the cloudspace's VMPools is not needed:
				// NodePoolName is the authoritative pool membership. If empty,
				// we cannot safely attribute this VM to a pool, so skip it.
				continue
			}

			var id Identity
			switch c.MatchBy {
			case "ip":
				if srv.IPAddress == "" {
					continue
				}
				id = Identity(srv.IPAddress)
			default: // "name"
				// The map key is the server name; ServerName is also populated
				// and equal to the key in practice. Prefer the explicit field
				// and fall back to the map key to be robust.
				if srv.ServerName != "" {
					id = Identity(srv.ServerName)
				} else {
					id = Identity(serverKey)
				}
			}
			if id == "" {
				continue
			}

			if out[pool] == nil {
				out[pool] = make(map[Identity]struct{})
			}
			out[pool][id] = struct{}{}
		}
	}
	return out, nil
}
