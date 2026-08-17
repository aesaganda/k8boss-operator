package backendclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// An unconfigured client must fail every call the same way, and must never put
// a request on the wire. The second half is the part worth a test: a stub that
// leaked a real request would reach the API with a blank token and an
// unresolvable URL, and the resulting transport error would be reported as
// "the backend is unreachable" — precisely the confidently-wrong answer the
// UnconfiguredError sentinel exists to prevent.
func TestUnconfiguredClientNeverCallsTheAPI(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := NewUnconfigured("K8BOSS_CLUSTER_ID", "K8BOSS_OPERATOR_TOKEN")
	// Point the stub at a real server so a leaked request would succeed and be
	// counted, rather than failing for an unrelated reason.
	c.baseURL = srv.URL

	ctx := context.Background()
	calls := map[string]error{}
	_, err := c.GetPaused(ctx)
	calls["GetPaused"] = err
	_, err = c.SetPaused(ctx, false, "test")
	calls["SetPaused"] = err
	_, err = c.ProviderHealth(ctx, 1)
	calls["ProviderHealth"] = err
	_, err = c.ReconcileSegmentationPolicy(ctx, 1, "ns", "n", SegmentationPolicySpec{})
	calls["ReconcileSegmentationPolicy"] = err
	_, err = c.ReconcileRuntimeSecurityPolicy(ctx, 1, "ns", "n", RuntimeSecurityPolicySpec{})
	calls["ReconcileRuntimeSecurityPolicy"] = err
	_, err = c.CloseCREdges(ctx, "SegmentationPolicy", 1, "ns", "n")
	calls["CloseCREdges"] = err

	for name, err := range calls {
		if err == nil {
			t.Errorf("%s: want an error from an unconfigured client, got nil", name)
			continue
		}
		if !IsUnconfigured(err) {
			t.Errorf("%s: want IsUnconfigured, got %v", name, err)
		}
		// Must NOT be mistakable for a response we never received.
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			t.Errorf("%s: unconfigured must not surface as *APIError (got %v) — "+
				"we never reached the API", name, err)
		}
		if !strings.Contains(err.Error(), "K8BOSS_CLUSTER_ID") ||
			!strings.Contains(err.Error(), "K8BOSS_OPERATOR_TOKEN") {
			t.Errorf("%s: error must name the missing settings, got %q", name, err.Error())
		}
	}

	if hits != 0 {
		t.Fatalf("an unconfigured client put %d request(s) on the wire; it must build none", hits)
	}
}

// A configured client must be unaffected — the guard keys off missing settings
// only, never off an empty token (which the backend legitimately treats as
// "no check" for cluster-internal callers).
func TestConfiguredClientIsNotUnconfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"paused": false}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "").GetPaused(context.Background()); err != nil {
		t.Fatalf("configured client with an empty token should still call the API: %v", err)
	}
	if IsUnconfigured(errors.New("boom")) {
		t.Error("IsUnconfigured must not match an unrelated error")
	}
}
