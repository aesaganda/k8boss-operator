// Package preflight answers one question, for the readiness probe: are the
// two things this operator cannot work without actually there?
//
//  1. The three K8Boss CRDs are served by the API server. Without them the
//     manager's caches never sync and no CR is ever reconciled.
//  2. The backend control-plane API is reachable and accepts our credentials.
//     It is the operator's ONLY door into the Knowledge Graph (ADR-0003 §2),
//     so an operator that cannot reach it can do nothing but fail reconciles.
//
// The standing invariant this package exists to honour: a check that could not
// determine an answer reports "could not determine" — never "ready". Every
// error path here says WHICH question went unanswered, in the same spirit as
// no_flow_observed vs no_flow_provider_available elsewhere in the repo. There
// is deliberately no "degrade to ready" path: an unreachable backend fails
// readiness loudly, and the pod is pulled out of service until it is not.
//
// Wiring (one line in cmd/main.go, after the manager is built):
//
//	mgr.AddReadyzCheck("preflight", preflight.New(mgr.GetConfig(), backend).Check)
//
// Deliberately NOT a liveness check. Liveness stays process-local
// (healthz.Ping): restarting a healthy operator because the backend is down
// fixes nothing and turns someone else's outage into a crash-loop.
package preflight

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"

	"k8boss.io/operator/api/v1alpha1"
	"k8boss.io/operator/internal/backendclient"
)

// BackendProbe is the slice of the control-plane client this package needs.
// GET /paused is the cheapest honest probe in the contract: read-only, never
// gated by the kill switch, and a 200 proves both reachability AND that our
// bearer token is accepted. *backendclient.Client satisfies it.
type BackendProbe interface {
	GetPaused(ctx context.Context) (*backendclient.PausedState, error)
}

// ResourceLister is the slice of client-go's discovery client this package
// needs. Kept as an interface so the check is testable without an API server.
type ResourceLister interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// requiredResources are the plural names of the three CRDs (ADR-0003 §3).
var requiredResources = []string{
	"platformconfigs",
	"runtimesecuritypolicies",
	"segmentationpolicies",
}

// defaultTimeout bounds one probe. Shorter than the backend client's own
// 30s transport timeout on purpose: a readiness probe that hangs is a
// readiness probe that answers nothing.
const defaultTimeout = 5 * time.Second

// Checker runs the preflight conditions. The zero value is not usable; build
// one with New or by setting Discovery and Backend directly (tests do).
type Checker struct {
	// Discovery is built lazily from Config on first use when nil, so New
	// never fails and main.go stays a one-liner.
	Discovery ResourceLister
	Config    *rest.Config

	Backend BackendProbe

	// Timeout bounds a single probe; defaultTimeout when zero.
	Timeout time.Duration
}

// New returns a Checker whose Check method is a healthz.Checker.
func New(cfg *rest.Config, backend BackendProbe) *Checker {
	return &Checker{Config: cfg, Backend: backend}
}

// Check is a healthz.Checker: nil means every condition was positively
// confirmed. Any other return means the operator is NOT ready, and the error
// text distinguishes "confirmed missing" from "could not look".
func (c *Checker) Check(req *http.Request) error {
	ctx := context.Background()
	if req != nil {
		ctx = req.Context()
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Order matters only for message quality: CRDs first, because "the CRDs
	// are not installed" explains a lot more than a backend error would.
	if err := c.checkCRDs(); err != nil {
		return err
	}
	return c.checkBackend(ctx)
}

// checkCRDs confirms the API server serves all three k8boss.io kinds.
//
// Needs no RBAC of its own: the discovery endpoints are readable by every
// authenticated principal via the stock system:discovery ClusterRole. On a
// cluster that has removed that binding this reports "could not determine",
// which is the correct answer — not a pass.
func (c *Checker) checkCRDs() error {
	gv := v1alpha1.GroupVersion.String()

	lister := c.Discovery
	if lister == nil {
		if c.Config == nil {
			return fmt.Errorf("could not determine whether the K8Boss CRDs are installed: " +
				"no Kubernetes client config was provided to the preflight checker")
		}
		d, err := discovery.NewDiscoveryClientForConfig(c.Config)
		if err != nil {
			return fmt.Errorf("could not determine whether the K8Boss CRDs are installed: "+
				"building a discovery client failed: %w", err)
		}
		c.Discovery = d
		lister = d
	}

	list, err := lister.ServerResourcesForGroupVersion(gv)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Confirmed absent, not merely unknown — say so precisely.
			return fmt.Errorf("the K8Boss CRDs are NOT installed: the API server serves no %s "+
				"resources (run `make install` from operator/)", gv)
		}
		return fmt.Errorf("could not determine whether the K8Boss CRDs are installed: "+
			"discovery for %s failed: %w", gv, err)
	}

	served := make(map[string]struct{}, len(list.APIResources))
	for _, r := range list.APIResources {
		// Skip subresources ("segmentationpolicies/status").
		if strings.Contains(r.Name, "/") {
			continue
		}
		served[r.Name] = struct{}{}
	}

	var missing []string
	for _, want := range requiredResources {
		if _, ok := served[want]; !ok {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the K8Boss CRDs are incompletely installed: %s serves no %s "+
			"(run `make install` from operator/)", gv, strings.Join(missing, ", "))
	}
	return nil
}

// checkBackend confirms the control-plane API answered us.
//
// The VALUE of the flag is not a readiness condition. paused=true is a
// deliberate operational state in which the operator is working correctly by
// refusing to mutate; failing readiness for it would take a correctly-behaving
// operator out of service and hide the pause behind a probe failure.
func (c *Checker) checkBackend(ctx context.Context) error {
	if c.Backend == nil {
		return fmt.Errorf("could not determine whether the control-plane API is reachable: " +
			"no backend client was provided to the preflight checker")
	}
	if _, err := c.Backend.GetPaused(ctx); err != nil {
		// "Nobody has told us where the backend is" is not a probe failure.
		// There is no outage to report and no request was attempted; the
		// operator is up, idle, and already saying so on every CR it owns
		// (ReasonAwaitingConfiguration). Failing readiness here would restate
		// a configuration gap as someone else's downtime — and, under OLM,
		// would hold the ClusterServiceVersion out of Succeeded forever for a
		// deployment that is behaving exactly as designed.
		//
		// This is the one and only degrade-to-ready path, and it is narrow on
		// purpose: it fires only for a client that was built with no
		// connection settings at all. A CONFIGURED backend that cannot be
		// reached still fails readiness loudly, per the package doc above.
		if backendclient.IsUnconfigured(err) {
			return nil
		}
		return fmt.Errorf("the control-plane API is NOT confirmed reachable, so no reconcile "+
			"could reach the Knowledge Graph: GET /internal/operator/v1/paused failed: %w", err)
	}
	return nil
}
