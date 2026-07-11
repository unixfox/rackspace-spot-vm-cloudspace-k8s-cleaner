// Package k8s provides the k3s node operations the cleaner needs: listing
// spot-managed nodes by label, reading their Ready state and identity, and
// deleting a node object.
package k8s

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

// NodesListerDeleter is the subset of the core/v1 Nodes client the cleaner
// uses. Declared as an interface so tests can substitute a fake clientset.
type NodesListerDeleter interface {
	List(ctx context.Context, opts metav1.ListOptions) (*corev1.NodeList, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
}

// Client wraps the in-cluster kubernetes clientset with the cleaner's
// node-scoping label and identity-selection mode.
type Client struct {
	Nodes       NodesListerDeleter
	VMPoolLabel string // label key whose presence marks a spot node
	MatchBy     string // "name" or "ip"
}

// NewInCluster builds a Client using in-cluster config and a ServiceAccount.
// It fails fast if not running inside a cluster.
func NewInCluster(vmPoolLabel, matchBy string) (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("k8s: load in-cluster config (are we running in a pod?): %w", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("k8s: build clientset: %w", err)
	}
	return NewFromCoreV1(cs.CoreV1().Nodes(), vmPoolLabel, matchBy), nil
}

// NewFromCoreV1 wires an existing core/v1 nodes interface into a Client.
// Useful for tests.
func NewFromCoreV1(nodes corev1client.NodeInterface, vmPoolLabel, matchBy string) *Client {
	return &Client{Nodes: nodes, VMPoolLabel: vmPoolLabel, MatchBy: matchBy}
}

// ManagedNode is a spot-managed k3s node with the identity used for matching
// against the Spot alive-VM set, plus its Ready state.
type ManagedNode struct {
	Name     string
	Pool     string // value of the VMPoolLabel
	Identity string // node.Name (matchBy=name) or InternalIP (matchBy=ip)
	NotReady bool
}

// ListSpotNodes returns all nodes carrying the VMPoolLabel.
func (c *Client) ListSpotNodes(ctx context.Context) ([]ManagedNode, error) {
	if c.VMPoolLabel == "" {
		return nil, fmt.Errorf("k8s: VMPoolLabel is empty")
	}

	req, err := labels.NewRequirement(c.VMPoolLabel, selection.Exists, nil)
	if err != nil {
		return nil, fmt.Errorf("k8s: build label selector for %q: %w", c.VMPoolLabel, err)
	}
	sel := labels.NewSelector().Add(*req)

	list, err := c.Nodes.List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("k8s: list nodes with selector %q: %w", sel.String(), err)
	}

	out := make([]ManagedNode, 0, len(list.Items))
	for i := range list.Items {
		n := &list.Items[i]
		pool := n.Labels[c.VMPoolLabel]
		if pool == "" {
			continue
		}
		mn := ManagedNode{
			Name:     n.Name,
			Pool:     pool,
			NotReady: isNotReady(n),
		}
		switch strings.ToLower(c.MatchBy) {
		case "ip":
			mn.Identity = internalIP(n)
		default:
			mn.Identity = n.Name
		}
		out = append(out, mn)
	}
	return out, nil
}

// DeleteNode removes a node object. It is a no-op (returns nil) if the node
// is already gone (NotFound), which is fine because the goal is "not in the
// cluster".
func (c *Client) DeleteNode(ctx context.Context, name string) error {
	err := c.Nodes.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("k8s: delete node %q: %w", name, err)
	}
	return nil
}

// isNotReady reports whether the Node's "Ready" status condition is anything
// other than True. A node that has not yet reported a Ready condition (e.g.
// very fresh) is treated as NotReady.
func isNotReady(n *corev1.Node) bool {
	for _, cond := range n.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status != corev1.ConditionTrue
		}
	}
	return true
}

// internalIP returns the node's first InternalIP address, or "" if none.
func internalIP(n *corev1.Node) string {
	for _, addr := range n.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			return addr.Address
		}
	}
	return ""
}
