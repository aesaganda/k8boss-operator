package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"k8boss.io/operator/internal/backendclient"
)

type fakeLister struct {
	list *metav1.APIResourceList
	err  error
}

func (f fakeLister) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	return f.list, f.err
}

func allCRDsServed() fakeLister {
	return fakeLister{list: &metav1.APIResourceList{APIResources: []metav1.APIResource{
		{Name: "platformconfigs"},
		{Name: "platformconfigs/status"},
		{Name: "segmentationpolicies"},
		{Name: "segmentationpolicies/status"},
		{Name: "runtimesecuritypolicies"},
		{Name: "runtimesecuritypolicies/status"},
	}}}
}

type fakeBackend struct {
	state *backendclient.PausedState
	err   error
}

func (f fakeBackend) GetPaused(context.Context) (*backendclient.PausedState, error) {
	return f.state, f.err
}

func reachable() fakeBackend {
	return fakeBackend{state: &backendclient.PausedState{Paused: true, EverSet: true}}
}

func TestReadyOnlyWhenBothConditionsConfirmed(t *testing.T) {
	c := &Checker{Discovery: allCRDsServed(), Backend: reachable()}
	if err := c.Check(nil); err != nil {
		t.Fatalf("expected ready, got %v", err)
	}
}

// paused is an operational state, not an unreadiness: the operator is working
// correctly by refusing to mutate.
func TestPausedIsStillReady(t *testing.T) {
	c := &Checker{
		Discovery: allCRDsServed(),
		Backend:   fakeBackend{state: &backendclient.PausedState{Paused: true}},
	}
	if err := c.Check(nil); err != nil {
		t.Fatalf("paused must not fail readiness, got %v", err)
	}
}

func TestCRDsConfirmedAbsent(t *testing.T) {
	c := &Checker{
		Discovery: fakeLister{err: apierrors.NewNotFound(
			schema.GroupResource{Group: "k8boss.io"}, "v1alpha1")},
		Backend: reachable(),
	}
	err := c.Check(nil)
	if err == nil {
		t.Fatal("missing CRDs must fail readiness")
	}
	if !strings.Contains(err.Error(), "NOT installed") {
		t.Fatalf("expected a confirmed-absent message, got %v", err)
	}
}

// The load-bearing case: discovery blew up, so we do not KNOW whether the CRDs
// are there. That must never read as ready, and must not claim they're absent.
func TestCRDsUndeterminedIsNotReady(t *testing.T) {
	c := &Checker{
		Discovery: fakeLister{err: errors.New("connection refused")},
		Backend:   reachable(),
	}
	err := c.Check(nil)
	if err == nil {
		t.Fatal("an undetermined CRD check must fail readiness")
	}
	if !strings.Contains(err.Error(), "could not determine") {
		t.Fatalf("expected a could-not-determine message, got %v", err)
	}
	if strings.Contains(err.Error(), "NOT installed") {
		t.Fatalf("must not assert absence from a failed lookup, got %v", err)
	}
}

func TestPartialCRDInstallIsNotReady(t *testing.T) {
	c := &Checker{
		Discovery: fakeLister{list: &metav1.APIResourceList{APIResources: []metav1.APIResource{
			{Name: "platformconfigs"},
		}}},
		Backend: reachable(),
	}
	err := c.Check(nil)
	if err == nil {
		t.Fatal("a partial CRD install must fail readiness")
	}
	for _, want := range []string{"segmentationpolicies", "runtimesecuritypolicies"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected %q named as missing, got %v", want, err)
		}
	}
}

// Subresources are not kinds: a list containing only "*/status" entries must
// not be read as the CRDs being served.
func TestSubresourcesDoNotSatisfyTheCheck(t *testing.T) {
	c := &Checker{
		Discovery: fakeLister{list: &metav1.APIResourceList{APIResources: []metav1.APIResource{
			{Name: "platformconfigs/status"},
			{Name: "segmentationpolicies/status"},
			{Name: "runtimesecuritypolicies/status"},
		}}},
		Backend: reachable(),
	}
	if err := c.Check(nil); err == nil {
		t.Fatal("subresource-only discovery must fail readiness")
	}
}

func TestUnreachableBackendIsNotReady(t *testing.T) {
	c := &Checker{
		Discovery: allCRDsServed(),
		Backend:   fakeBackend{err: errors.New("dial tcp: connection refused")},
	}
	err := c.Check(nil)
	if err == nil {
		t.Fatal("an unreachable control-plane API must fail readiness")
	}
	if !strings.Contains(err.Error(), "NOT confirmed reachable") {
		t.Fatalf("expected a reachability message, got %v", err)
	}
}

// A rejected token means we reached the API but cannot use it — still not ready.
func TestRejectedTokenIsNotReady(t *testing.T) {
	c := &Checker{
		Discovery: allCRDsServed(),
		Backend: fakeBackend{err: &backendclient.APIError{
			StatusCode: 401, Category: "unauthenticated", Code: "invalid_operator_token",
		}},
	}
	if err := c.Check(nil); err == nil {
		t.Fatal("a rejected operator token must fail readiness")
	}
}

// A misconstructed Checker must fail closed, not pass by having nothing to do.
func TestMisconfiguredCheckerFailsClosed(t *testing.T) {
	if err := (&Checker{Backend: reachable()}).Check(nil); err == nil {
		t.Fatal("a checker with no discovery source must not report ready")
	}
	if err := (&Checker{Discovery: allCRDsServed()}).Check(nil); err == nil {
		t.Fatal("a checker with no backend client must not report ready")
	}
}
