package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

// Config holds all runtime configuration for the cleaner, loaded from
// environment variables (with sensible defaults where it makes sense).
type Config struct {
	// Spot platform credentials & endpoint.
	SpotRefreshToken string `envconfig:"SPOT_REFRESH_TOKEN" required:"true"`
	SpotOrg          string `envconfig:"SPOT_ORG" required:"true"`
	SpotBaseURL      string `envconfig:"SPOT_BASE_URL" default:"https://spot.rackspace.com"`
	SpotAuthURL      string `envconfig:"SPOT_AUTH_URL" default:"https://login.spot.rackspace.com"`

	// Label on a k3s node used to (a) scope the cleaner to spot nodes and
	// (b) carry the VM pool name so the alive-VM check narrows to the
	// correct pool. cloud-init must set this label on every spot VM node.
	VMPoolLabel string `envconfig:"SPOT_VM_POOL_LABEL" default:"rackspace-spot/vm-pool-name"`

	// How a k3s node is matched to a Spot VM.
	//   "name" -> node.Name == Spot VM server name (AssignedServers map key /
	//            VMAssignedServer.ServerName). Requires cloud-init to set the
	//            node name (hostname) to the Spot VM server name.
	//   "ip"   -> node status address InternalIP == VMAssignedServer.IPAddress.
	MatchBy string `envconfig:"MATCH_BY" default:"name"`

	// Reconcile cadence and safety buffer.
	CleanInterval time.Duration `envconfig:"CLEAN_INTERVAL" default:"60s"`
	// Number of consecutive ticks a node must be (gone VM && NotReady) before
	// it is deleted. Guards against transient Spot API hiccups.
	GraceTicks int  `envconfig:"GRACE_TICKS" default:"2"`
	DryRun     bool `envconfig:"DRY_RUN" default:"false"`
}

// Load reads configuration from environment variables and validates it.
func Load() (Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return c, fmt.Errorf("envconfig: %w", err)
	}

	if strings.TrimSpace(c.SpotRefreshToken) == "" {
		return c, fmt.Errorf("SPOT_REFRESH_TOKEN is required")
	}
	if strings.TrimSpace(c.SpotOrg) == "" {
		return c, fmt.Errorf("SPOT_ORG is required")
	}

	switch strings.ToLower(strings.TrimSpace(c.MatchBy)) {
	case "name":
		c.MatchBy = "name"
	case "ip":
		c.MatchBy = "ip"
	default:
		return c, fmt.Errorf("MATCH_BY must be \"name\" or \"ip\", got %q", c.MatchBy)
	}

	if c.CleanInterval <= 0 {
		return c, fmt.Errorf("CLEAN_INTERVAL must be > 0, got %v", c.CleanInterval)
	}
	if c.GraceTicks < 1 {
		c.GraceTicks = 1
	}

	return c, nil
}
