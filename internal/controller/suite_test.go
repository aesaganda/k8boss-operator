// Test suite for the three reconcilers.
//
// Runs against a REAL API server (controller-runtime's envtest: a local
// kube-apiserver + etcd with the operator's own CRDs installed from
// config/crd/bases). That matters here rather than being ceremony: three of the
// behaviours under test are API-server behaviours, not reconciler behaviours —
// the status subresource being separate from the spec (so a Status().Update()
// cannot smuggle a finalizer change through), a finalizer actually holding an
// object in Terminating instead of letting it vanish, and the CRD's
// `default: true` on spec.paused actually applying. A fake client fakes all
// three and would pass a reconciler that gets them wrong.
//
// The backend control-plane API is an httptest.Server, never a live backend:
// what is being tested is how the reconciler reacts to each documented response
// in internal_operator_api_CONTRACT.md, including the ones a healthy backend
// never sends.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"k8boss.io/operator/api/v1alpha1"
	"k8boss.io/operator/internal/backendclient"
)

var (
	testEnv *envtest.Environment
	cfg     *rest.Config
	k8sC    client.Client
	scheme  = runtime.NewScheme()

	// testCtx stands in for t.Context(), which needs go1.24 (this module is
	// go1.23). Nothing here depends on per-test cancellation.
	testCtx = context.Background()
)

func TestMain(m *testing.M) {
	logf.SetLogger(zap.New(zap.UseDevMode(true), zap.WriteTo(os.Stderr)))

	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "adding client-go scheme: %v\n", err)
		os.Exit(1)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		fmt.Fprintf(os.Stderr, "adding v1alpha1 to scheme: %v\n", err)
		os.Exit(1)
	}

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}

	var err error
	cfg, err = testEnv.Start()
	if err != nil {
		// Deliberately fatal, not skip. A suite that silently degrades to
		// "no control plane, everything passes" is the same defect class this
		// suite exists to hunt: a quiet failure indistinguishable from a
		// healthy quiet state.
		fmt.Fprintf(os.Stderr,
			"envtest could not start a control plane: %v\n"+
				"Install the binaries with:\n"+
				"  go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use 1.31.0 -p path\n"+
				"and export KUBEBUILDER_ASSETS to the printed directory.\n", err)
		os.Exit(1)
	}

	k8sC, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		fmt.Fprintf(os.Stderr, "building client: %v\n", err)
		_ = testEnv.Stop()
		os.Exit(1)
	}

	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

// ── backend double ──────────────────────────────────────────────────────

// backendStub is an httptest.Server speaking the control-plane contract. Each
// route returns whatever the test queued for it, and every request is recorded
// so a test can assert on calls NOT made (the paused path's whole point).
type backendStub struct {
	t   *testing.T
	srv *httptest.Server

	// handler per path suffix, e.g. "/segmentation-policies/reconcile".
	responses map[string]stubResponse

	calls []string
}

type stubResponse struct {
	code int
	body any
}

func okBody(m map[string]any) stubResponse { return stubResponse{code: 200, body: m} }

// apiError builds the documented error envelope: detail is a structured
// {category, code, message}, never a bare string.
func apiError(code int, category, errCode, msg string) stubResponse {
	return stubResponse{code: code, body: map[string]any{
		"detail": map[string]any{"category": category, "code": errCode, "message": msg},
	}}
}

func newBackendStub(t *testing.T) *backendStub {
	t.Helper()
	s := &backendStub{t: t, responses: map[string]stubResponse{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/internal/operator/v1"
		if r.URL.Path[:len(prefix)] != prefix {
			t.Errorf("operator called a path outside the contract prefix: %s", r.URL.Path)
		}
		key := r.URL.Path[len(prefix):]
		s.calls = append(s.calls, key)

		resp, ok := s.responses[key]
		if !ok {
			t.Errorf("unexpected backend call to %s (no response queued)", key)
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.code)
		_ = json.NewEncoder(w).Encode(resp.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *backendStub) on(path string, resp stubResponse) { s.responses[path] = resp }

func (s *backendStub) client() *backendclient.Client {
	return backendclient.New(s.srv.URL, "")
}

func (s *backendStub) called(path string) bool {
	for _, c := range s.calls {
		if c == path {
			return true
		}
	}
	return false
}

func (s *backendStub) assertNotCalled(path string) {
	s.t.Helper()
	if s.called(path) {
		s.t.Fatalf("backend %s was called, but this path must make no backend call at all "+
			"(calls seen: %v)", path, s.calls)
	}
}

// ── fixtures ────────────────────────────────────────────────────────────

const (
	pathSegReconcile     = "/segmentation-policies/reconcile"
	pathRuntimeReconcile = "/runtime-security-policies/reconcile"
	pathClose            = "/cr-edges/close"
	pathPaused           = "/paused"
	pathProviderHealth   = "/provider-health"
)

var uniq int

// newNamespace makes each test's objects independent, since one API server is
// shared across the whole package.
func newNamespace(t *testing.T) string {
	t.Helper()
	uniq++
	name := fmt.Sprintf("t%d-%d", time.Now().UnixNano()%1e6, uniq)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := k8sC.Create(testCtx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}
	return name
}

// applyPlatformConfig creates a cluster-scoped PlatformConfig and returns its
// name. It is deleted at test end so the singleton does not leak between tests.
func applyPlatformConfig(t *testing.T, paused bool, provider v1alpha1.FlowProviderKind) string {
	t.Helper()
	uniq++
	name := fmt.Sprintf("pc-%d-%d", time.Now().UnixNano()%1e6, uniq)
	pc := &v1alpha1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.PlatformConfigSpec{
			FlowProvider: provider,
			Paused:       paused,
		},
	}
	if err := k8sC.Create(testCtx, pc); err != nil {
		t.Fatalf("creating PlatformConfig: %v", err)
	}
	t.Cleanup(func() { _ = k8sC.Delete(testCtx, pc) })
	return name
}

// applyPlatformConfigOmittingPaused creates a PlatformConfig through the
// unstructured client with spec.paused genuinely absent from the request body.
// The typed client cannot express this: Paused is a plain bool with no
// omitempty, so a typed Create always sends `paused: false` and would test the
// CRD default by never exercising it.
func applyPlatformConfigOmittingPaused(t *testing.T) string {
	t.Helper()
	uniq++
	name := fmt.Sprintf("pcdef-%d-%d", time.Now().UnixNano()%1e6, uniq)
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": v1alpha1.GroupVersion.String(),
		"kind":       "PlatformConfig",
		"metadata":   map[string]any{"name": name},
		"spec": map[string]any{
			"flowProvider":                "Hubble",
			"knowledgeGraphConnectionRef": map[string]any{"name": "k8boss-operator"},
		},
	}}
	if err := k8sC.Create(testCtx, u); err != nil {
		t.Fatalf("creating PlatformConfig without spec.paused: %v", err)
	}
	t.Cleanup(func() { _ = k8sC.Delete(testCtx, u) })
	return name
}

func newSegPolicy(t *testing.T, ns, name string, spec v1alpha1.SegmentationPolicySpec) *v1alpha1.SegmentationPolicy {
	t.Helper()
	cr := &v1alpha1.SegmentationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
	if err := k8sC.Create(testCtx, cr); err != nil {
		t.Fatalf("creating SegmentationPolicy: %v", err)
	}
	return cr
}

func newRuntimePolicy(t *testing.T, ns, name string, spec v1alpha1.RuntimeSecurityPolicySpec) *v1alpha1.RuntimeSecurityPolicy {
	t.Helper()
	cr := &v1alpha1.RuntimeSecurityPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       spec,
	}
	if err := k8sC.Create(testCtx, cr); err != nil {
		t.Fatalf("creating RuntimeSecurityPolicy: %v", err)
	}
	return cr
}

func req(ns, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: client.ObjectKey{Namespace: ns, Name: name}}
}

// ── assertions ──────────────────────────────────────────────────────────

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// assertCondition checks type/status/reason together. Reason is asserted
// exactly, because the whole contract with a human reading the CR is that the
// condition names WHICH question failed — "Ready=False, reason=ReconcileFailed"
// where the truth is ProviderUnavailable is the defect, not a nitpick.
func assertCondition(t *testing.T, conds []metav1.Condition, condType string,
	status metav1.ConditionStatus, reason string) *metav1.Condition {
	t.Helper()
	c := findCondition(conds, condType)
	if c == nil {
		t.Fatalf("no %s condition was set at all (conditions: %+v)", condType, conds)
	}
	if c.Status != status || c.Reason != reason {
		t.Fatalf("%s condition = (%s, %s), want (%s, %s); message=%q",
			condType, c.Status, c.Reason, status, reason, c.Message)
	}
	return c
}
