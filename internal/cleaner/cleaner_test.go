package cleaner

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	k8sclient "github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/k8s"
	"github.com/edevos/rackspace-spot-vm-cloudspace-k8s-cleaner/internal/spot"
	v1 "github.com/rackspace-spot/spot-go-sdk/api/v1"
)

const poolLabel = "rackspace-spot/vm-pool-name"

// fakeSpot satisfies spot.ListVMCloudSpaceser.
type fakeSpot struct {
	list *v1.VMCloudSpaceList
	err  error
}

func (f fakeSpot) ListVMCloudSpaces(ctx context.Context, org string) (*v1.VMCloudSpaceList, error) {
	return f.list, f.err
}

// node builds a corev1.Node with the pool label, a Ready condition, and an
// optional InternalIP.
func node(name, pool string, ready bool, ip string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{poolLabel: pool},
		},
	}
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	n.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: status},
	}
	if ip != "" {
		n.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: ip},
		}
	}
	return n
}

// aliveList builds a VMCloudSpaceList with one cloudspace whose AssignedServers
// contains the given (serverName, pool, ip) tuples.
func aliveList(servers ...struct{ name, pool, ip string }) *v1.VMCloudSpaceList {
	vmcs := v1.VMCloudSpace{
		Name:            "test-vmcs",
		Org:             "org",
		AssignedServers: map[string]v1.VMAssignedServer{},
	}
	for _, s := range servers {
		vmcs.AssignedServers[s.name] = v1.VMAssignedServer{
			ServerName:   s.name,
			NodePoolName: s.pool,
			IPAddress:    s.ip,
			State:        "Ready",
		}
	}
	return &v1.VMCloudSpaceList{Items: []v1.VMCloudSpace{vmcs}}
}

func newCleaner(spotC *spot.Client, nodes *corev1.Node, grace int, dryRun bool) (*Cleaner, *fake.Clientset) {
	cs := fake.NewSimpleClientset(nodes)
	kClient := k8sclient.NewFromCoreV1(cs.CoreV1().Nodes(), poolLabel, "name")
	return New(spotC, kClient, grace, dryRun), cs
}

func reset() {
	// nothing global; kept for readability
}

func TestHealthyNodeNotDeleted(t *testing.T) {
	// VM alive and node Ready: never deleted, stale stays 0.
	spotC := &spot.Client{
		Spot:    fakeSpot{list: aliveList(struct{ name, pool, ip string }{"node-1", "pool-a", "10.0.0.1"})},
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", true, ""), 2, false)

	for i := 0; i < 5; i++ {
		res, err := cln.Reconcile(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("tick %d: unexpected error: %v", i, err)
		}
		if res.Deleted != 0 {
			t.Fatalf("tick %d: expected 0 deletes, got %d", i, res.Deleted)
		}
	}
	got, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("node should still exist: %v", err)
	}
	if got.Name != "node-1" {
		t.Fatalf("unexpected node: %s", got.Name)
	}
}

func TestGoneVMButNodeReadyNotDeleted(t *testing.T) {
	// VM gone but node is Ready: do not delete.
	spotC := &spot.Client{
		Spot:    fakeSpot{list: aliveList()}, // no alive VMs
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", true, ""), 2, false)

	for i := 0; i < 5; i++ {
		res, err := cln.Reconcile(context.Background(), time.Now())
		if err != nil {
			t.Fatalf("tick %d: unexpected error: %v", i, err)
		}
		if res.Deleted != 0 || res.Evaluated != 0 {
			t.Fatalf("tick %d: expected 0 deletes/evaluated, got deleted=%d evaluated=%d", i, res.Deleted, res.Evaluated)
		}
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("node should still exist: %v", err)
	}
}

func TestGraceUntilDeleted(t *testing.T) {
	spotC := &spot.Client{
		Spot:    fakeSpot{list: aliveList()}, // VM is gone
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", false, ""), 2, false)

	// Tick 1: candidate, but grace not reached.
	res, err := cln.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if res.Evaluated != 1 || res.Deleted != 0 {
		t.Fatalf("tick 1: expected evaluated=1 deleted=0, got evaluated=%d deleted=%d", res.Evaluated, res.Deleted)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("node should still exist after tick 1: %v", err)
	}

	// Tick 2: grace reached -> deleted.
	res, err = cln.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("tick 2: expected 1 delete, got %d", res.Deleted)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err == nil {
		t.Fatalf("node should have been deleted")
	}
}

func TestDryRunDoesNotDelete(t *testing.T) {
	spotC := &spot.Client{
		Spot:    fakeSpot{list: aliveList()},
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", false, ""), 1, true)

	res, err := cln.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Evaluated != 1 {
		t.Fatalf("expected evaluated=1, got %d", res.Evaluated)
	}
	if res.Deleted != 0 {
		t.Fatalf("dry-run must not delete, got %d", res.Deleted)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("node must still exist under dry-run: %v", err)
	}
}

func TestSpotAPIErrorIsReturned(t *testing.T) {
	spotC := &spot.Client{
		Spot:    fakeSpot{err: errBoom},
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", false, ""), 1, false)

	res, err := cln.Reconcile(context.Background(), time.Now())
	if err == nil {
		t.Fatalf("expected error from Spot API, got nil")
	}
	if res.Deleted != 0 || res.ManagedNodes != 0 {
		t.Fatalf("expected empty result on Spot error, got %+v", res)
	}
	// Node untouched.
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("node must still exist on Spot error: %v", err)
	}
}

func TestRecoveryResetsStale(t *testing.T) {
	// VM gone + NotReady for one tick (stale=1, grace=3 not reached), then VM
	// comes back: stale must reset to 0 and node never deleted.
	listGone := aliveList()
	listBack := aliveList(struct{ name, pool, ip string }{"node-1", "pool-a", "10.0.0.1"})

	spotC := &spot.Client{
		Spot:    &swapSpot{list: listGone},
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", false, ""), 3, false)

	if _, err := cln.Reconcile(context.Background(), time.Now()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	// VM returns.
	spotC.Spot.(*swapSpot).list = listBack
	if _, err := cln.Reconcile(context.Background(), time.Now()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}
	// Now Remove VM again and go a few ticks; stale must restart from 0, not
	// resume at 1.
	spotC.Spot.(*swapSpot).list = listGone
	for i := 0; i < 2; i++ {
		if _, err := cln.Reconcile(context.Background(), time.Now()); err != nil {
			t.Fatalf("tick 3+%d: %v", i, err)
		}
	}
	// grace=3, so after 2 more ticks stale=2, not yet deleted.
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("node should still exist: %v", err)
	}
	if cln.stale["node-1"] != 2 {
		t.Fatalf("expected stale=2 after recovery+2 gone ticks, got %d", cln.stale["node-1"])
	}
	// One more tick -> grace reached -> deleted.
	res, _ := cln.Reconcile(context.Background(), time.Now())
	if res.Deleted != 1 {
		t.Fatalf("expected 1 delete, got %d", res.Deleted)
	}
}

func TestMatchesByName(t *testing.T) {
	// node "node-1" in pool-a; alive VM "node-1" in pool-a -> not deleted.
	spotC := &spot.Client{
		Spot:    fakeSpot{list: aliveList(struct{ name, pool, ip string }{"node-1", "pool-a", "10.0.0.1"})},
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", false, ""), 1, false)
	res, err := cln.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Evaluated != 0 || res.Deleted != 0 {
		t.Fatalf("node with alive VM should not be a candidate or deleted, got evaluated=%d deleted=%d", res.Evaluated, res.Deleted)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("node should still exist: %v", err)
	}
}

func TestMatchesByIP(t *testing.T) {
	spotC := &spot.Client{
		Spot:    fakeSpot{list: aliveList(struct{ name, pool, ip string }{"server-xyz", "pool-a", "10.0.0.5"})},
		Org:     "org",
		MatchBy: "ip",
	}
	cs := fake.NewSimpleClientset(node("node-1", "pool-a", false, "10.0.0.5"))
	kClient := k8sclient.NewFromCoreV1(cs.CoreV1().Nodes(), poolLabel, "ip")
	cln := New(spotC, kClient, 1, false)

	res, err := cln.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Evaluated != 0 || res.Deleted != 0 {
		t.Fatalf("node with alive IP should not be a candidate, got evaluated=%d deleted=%d", res.Evaluated, res.Deleted)
	}
}

func TestUnlabeledNodeIgnored(t *testing.T) {
	// A node WITHOUT the pool label must never be touched, even if it's the
	// only node and no VMs are alive.
	spotC := &spot.Client{Spot: fakeSpot{list: aliveList()}, Org: "org", MatchBy: "name"}
	unlabeled := node("plain-node", "pool-a", false, "")
	delete(unlabeled.Labels, poolLabel) // remove the label
	cs := fake.NewSimpleClientset(unlabeled)
	kClient := k8sclient.NewFromCoreV1(cs.CoreV1().Nodes(), poolLabel, "name")
	cln := New(spotC, kClient, 1, false)

	res, err := cln.Reconcile(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.ManagedNodes != 0 {
		t.Fatalf("unlabeled node should not be managed, got %d", res.ManagedNodes)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "plain-node", metav1.GetOptions{}); err != nil {
		t.Fatalf("plain node must still exist: %v", err)
	}
}

func TestDeletePermissionDeniedIsFatal(t *testing.T) {
	// RBAC rejects the delete: Reconcile must surface the error (main exits)
	// instead of counting it and carrying on.
	spotC := &spot.Client{
		Spot:    fakeSpot{list: aliveList()}, // VM gone
		Org:     "org",
		MatchBy: "name",
	}
	cln, cs := newCleaner(spotC, node("node-1", "pool-a", false, ""), 1, false)
	cs.PrependReactor("delete", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			corev1.Resource("nodes"), "node-1", errBoom)
	})

	res, err := cln.Reconcile(context.Background(), time.Now())
	if err == nil {
		t.Fatalf("expected error when delete is forbidden, got nil")
	}
	if !apierrors.IsForbidden(errors.Unwrap(errors.Unwrap(err))) {
		t.Fatalf("expected a wrapped Forbidden error, got %v", err)
	}
	if res.Deleted != 0 {
		t.Fatalf("expected 0 deletes, got %d", res.Deleted)
	}
	if _, err := cs.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{}); err != nil {
		t.Fatalf("node must still exist after a forbidden delete: %v", err)
	}
}

// swapSpot is a fakeSpot whose returned list can be swapped at runtime.
type swapSpot struct {
	list *v1.VMCloudSpaceList
	err  error
}

func (s *swapSpot) ListVMCloudSpaces(ctx context.Context, org string) (*v1.VMCloudSpaceList, error) {
	return s.list, s.err
}

// errBoom is a sentinel error used by TestSpotAPIErrorSkipsTick.
type errBoomType struct{}

func (errBoomType) Error() string { return "boom" }

var errBoom = errBoomType{}
