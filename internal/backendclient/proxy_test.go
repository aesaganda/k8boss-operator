package backendclient

import (
	"net/http"
	"testing"
)

// The CSV published to OperatorHub carries
// `features.operators.openshift.io/proxy-aware: "true"`, which is a public
// claim that this operator honours a cluster-wide egress proxy. The mechanism
// it rests on is one easily-lost line: New() leaves http.Client.Transport nil,
// so requests go through http.DefaultTransport, whose Proxy field is
// http.ProxyFromEnvironment — and that is what reads the
// HTTP_PROXY/HTTPS_PROXY/NO_PROXY that OLM injects into the manager Deployment
// on a cluster with a Proxy object.
//
// Setting a custom Transport (for a CA bundle, a timeout, connection limits —
// all plausible future edits) silently drops proxy support unless the new
// Transport sets Proxy itself, and nothing else in the suite would notice. The
// label would then be a lie told on a listing page, which is the defect this
// repo ranks above a broken page.
//
// Asserting on env-var plumbing instead would be worse than useless here:
// net/http caches the proxy config behind a sync.Once on first use, so such a
// test passes or fails on test ORDER within the package.
func TestNewUsesProxyHonouringTransport(t *testing.T) {
	c := New("http://k8boss-backend.k8boss-system.svc:8010", "tok")

	switch tr := c.http.Transport.(type) {
	case nil:
		// http.DefaultTransport, whose Proxy is ProxyFromEnvironment.
	case *http.Transport:
		if tr.Proxy == nil {
			t.Fatal("client has a custom *http.Transport with a nil Proxy: " +
				"HTTP_PROXY/HTTPS_PROXY/NO_PROXY are ignored, which falsifies " +
				"features.operators.openshift.io/proxy-aware=true in the CSV. " +
				"Set Proxy: http.ProxyFromEnvironment, or flip the label to false.")
		}
	default:
		t.Fatalf("client uses a %T, which cannot be shown to honour the proxy "+
			"environment; either restore a proxy-aware transport or flip "+
			"features.operators.openshift.io/proxy-aware to false in "+
			"config/manifests/bases/k8boss-operator.clusterserviceversion.yaml", tr)
	}

	// An unconfigured client reaches the wire for nothing, but it is the same
	// struct and a future refactor could set Transport in one constructor only.
	if u := NewUnconfigured("K8BOSS_BACKEND_URL"); u.http == nil {
		t.Fatal("NewUnconfigured left http nil; it must stay a usable client shell")
	}
}
