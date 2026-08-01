// Package backendclient is the operator's only door into the K8Boss
// Knowledge Graph (ADR-0003 §2: "the operator never writes to Postgres
// directly"). It speaks the contract documented in
// backend/app/api/internal_operator_api_CONTRACT.md and nothing else.
//
// Two things this package exists to get right, both of them instances of the
// repo-wide defect standard ("a wrong answer delivered confidently is worse
// than no answer"):
//
//  1. Every non-2xx response is parsed into a *APIError carrying the API's
//     stable {category, code, message} keys, so a caller can build a specific
//     status condition ("provider_unavailable — Tetragon not confirmed
//     installed") instead of a generic "request failed". Callers switch on
//     Category/Code, never on Message.
//
//  2. `paused` (423) and `provider_unavailable` (503) are distinguishable from
//     each other and from a transport failure. A paused API applied *nothing*
//     and will keep applying nothing until a human unpauses; a 503 means we
//     could not look, which is not the same as looking and finding zero.
package backendclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Finalizer/reconcile calls are single round trips against an in-cluster
// Service; a short timeout keeps a wedged backend from pinning a worker.
const defaultTimeout = 30 * time.Second

// Client is safe for concurrent use by the manager's controller workers.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a client for the control-plane API. baseURL is the backend root
// (e.g. "http://k8boss-backend:8000"); the /internal/operator/v1 prefix is
// appended here so callers never hand-build paths. token may be empty — the
// backend treats an empty configured OPERATOR_API_TOKEN as "no check"
// (cluster-internal Service networking), same trade-off as OTLP_INGEST_TOKEN.
func New(baseURL, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// ── error shape ─────────────────────────────────────────────────────────

// APIError is a non-2xx response from the control-plane API, decoded into the
// stable machine keys the contract guarantees. Category/Code are safe to
// switch on; Message is for humans and logs.
type APIError struct {
	StatusCode int
	Category   string
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	// Deliberately specific: this string ends up verbatim in a
	// status.conditions message, where "request failed" would be useless.
	switch {
	case e.Category != "" && e.Code != "":
		return fmt.Sprintf("HTTP %d %s/%s: %s", e.StatusCode, e.Category, e.Code, e.Message)
	case e.Category != "":
		return fmt.Sprintf("HTTP %d %s: %s", e.StatusCode, e.Category, e.Message)
	default:
		return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Message)
	}
}

// IsPaused reports whether err is the API's persisted kill switch saying "no
// mutation was applied". This is an expected operational state, not a fault:
// callers must surface it as its own condition reason and back off, not retry
// hot and not report it as a failure to reconcile.
func IsPaused(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.StatusCode == http.StatusLocked || e.Category == "paused")
}

// IsProviderUnavailable reports whether err is "we could not look", as opposed
// to "we looked and found nothing" — the RuntimeSecurityPolicy reconcile
// raises it when Tetragon is not confirmed installed. Never collapse this into
// a zero-evidence success.
func IsProviderUnavailable(err error) bool {
	var e *APIError
	return errors.As(err, &e) &&
		(e.StatusCode == http.StatusServiceUnavailable || e.Category == "provider_unavailable")
}

// IsWriteNotVerified reports whether the write was committed but a post-commit
// re-read did not confirm it landed. Retryable, and distinct from a rejected
// write: the graph may or may not have the edges, so nothing may be concluded
// from it until a later reconcile succeeds.
func IsWriteNotVerified(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Category == "write_not_verified"
}

// errorDetail matches both shapes the backend can emit: this router's
// structured {"detail": {...}} and the globally-registered cluster-layer
// handlers' {"detail": "...", "reason": "..."} .
type errorDetail struct {
	Detail json.RawMessage `json:"detail"`
	Reason string          `json:"reason"`
}

type structuredDetail struct {
	Category string `json:"category"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

func parseAPIError(status int, body []byte) *APIError {
	out := &APIError{StatusCode: status}

	var env errorDetail
	if err := json.Unmarshal(body, &env); err != nil || len(env.Detail) == 0 {
		out.Message = truncate(string(body))
		return out
	}

	var sd structuredDetail
	if err := json.Unmarshal(env.Detail, &sd); err == nil && (sd.Category != "" || sd.Code != "") {
		out.Category, out.Code, out.Message = sd.Category, sd.Code, sd.Message
		return out
	}

	// Cluster-layer handler shape: detail is a plain string, reason is the
	// machine key. Map it onto the same fields so callers have one contract.
	var plain string
	if err := json.Unmarshal(env.Detail, &plain); err == nil {
		out.Message = plain
		out.Code = env.Reason
		if env.Reason != "" {
			out.Category = "cluster"
		}
		return out
	}

	out.Message = truncate(string(body))
	return out
}

func truncate(s string) string {
	const max = 512
	s = strings.TrimSpace(s)
	if len(s) > max {
		return s[:max] + "…"
	}
	if s == "" {
		return "(empty response body)"
	}
	return s
}

// ── wire shapes ─────────────────────────────────────────────────────────

// WorkloadSelector mirrors v1alpha1.WorkloadSelector on the wire (camelCase
// preserved, per the contract).
type WorkloadSelector struct {
	MatchLabels map[string]string `json:"matchLabels"`
}

type SegmentationRule struct {
	Ports         []int32           `json:"ports"`
	PeerSelector  map[string]string `json:"peerSelector"`
	PeerNamespace string            `json:"peerNamespace"`
}

type SegmentationPolicySpec struct {
	Selector    WorkloadSelector   `json:"selector"`
	Ingress     []SegmentationRule `json:"ingress"`
	Egress      []SegmentationRule `json:"egress"`
	ProviderRef string             `json:"providerRef"`
}

type RuntimeSecurityPolicySpec struct {
	Selector         WorkloadSelector `json:"selector"`
	TracingPolicyRef string           `json:"tracingPolicyRef"`
	DeniedBinaries   []string         `json:"deniedBinaries"`
}

type segmentationReconcileRequest struct {
	ClusterID int                    `json:"cluster_id"`
	Namespace string                 `json:"namespace"`
	Name      string                 `json:"name"`
	Spec      SegmentationPolicySpec `json:"spec"`
}

type runtimeSecurityReconcileRequest struct {
	ClusterID int                       `json:"cluster_id"`
	Namespace string                    `json:"namespace"`
	Name      string                    `json:"name"`
	Spec      RuntimeSecurityPolicySpec `json:"spec"`
}

type closeRequest struct {
	CRKind    string `json:"cr_kind"`
	ClusterID int    `json:"cluster_id"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// ReconcileResult is the 200 body of either reconcile endpoint.
//
// UnverifiedEdgeKeys is expected to be empty on a 200 (the server turns a
// non-empty list into a 500 write_not_verified), but it is decoded and
// re-checked client-side anyway: a Ready=True that depends on the server
// having remembered to check is a Ready=True built on someone else's
// diligence.
type ReconcileResult struct {
	Status             string   `json:"status"`
	EdgesUpserted      int      `json:"edges_upserted"`
	EdgesClosed        int      `json:"edges_closed"`
	EdgeKeys           []string `json:"edge_keys"`
	MatchedWorkloads   int      `json:"matched_workloads"`
	UnverifiedEdgeKeys []string `json:"unverified_edge_keys"`

	// ProviderInstalled is only sent by the runtime-security endpoint. A nil
	// pointer means "the field was absent", which is not the same as false.
	ProviderInstalled *bool `json:"provider_installed"`
}

// CloseResult is the 200 body of the finalizer's close endpoint.
type CloseResult struct {
	Status             string   `json:"status"`
	EdgesClosed        int      `json:"edges_closed"`
	UnverifiedEdgeKeys []string `json:"unverified_edge_keys"`
}

// PausedState is the 200 body of GET/PUT /paused.
type PausedState struct {
	Paused    bool    `json:"paused"`
	UpdatedAt *string `json:"updated_at"`
	UpdatedBy *string `json:"updated_by"`

	// EverSet false means no row was ever written and the server is
	// defaulting to paused — reported honestly rather than as a bare true.
	EverSet bool `json:"ever_set"`
}

// ProviderHealth is one entry of GET /provider-health.
//
// Checked is the load-bearing field. Checked=false means K8Boss could not
// determine this provider's health at all; Healthy is nil in that case and
// must never be rendered as healthy=false. This is the same distinction as
// no_flow_observed vs no_flow_provider_available elsewhere in the repo.
type ProviderHealth struct {
	Name           string   `json:"name"`
	Checked        bool     `json:"checked"`
	Healthy        *bool    `json:"healthy"`
	Message        string   `json:"message"`
	LastQueryError *string  `json:"last_query_error"`
	Blockers       []string `json:"blockers"`
}

// Determinate reports whether this entry carries an actual health verdict.
// An entry that was not checked, or that was checked but came back with no
// verdict, is indeterminate — the caller must surface it as "unknown", not
// as "unhealthy".
func (p ProviderHealth) Determinate() bool { return p.Checked && p.Healthy != nil }

// ProviderHealthResponse is the 200 body of GET /provider-health.
type ProviderHealthResponse struct {
	ClusterID int              `json:"cluster_id"`
	Providers []ProviderHealth `json:"providers"`
}

// ── transport ───────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request body for %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("building request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport failure is deliberately NOT an *APIError: we never
		// reached the API, so we know nothing about what it would have said.
		return fmt.Errorf("calling %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response from %s %s: %w", method, path, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return parseAPIError(resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		// A 200 we cannot decode is not a success we may act on.
		return fmt.Errorf("decoding 200 response from %s %s: %w (body: %s)",
			method, path, err, truncate(string(raw)))
	}
	return nil
}

const apiPrefix = "/internal/operator/v1"

// ReconcileSegmentationPolicy pushes one CR's spec at the control plane. The
// call is idempotent server-side — safe to make on every reconcile.
func (c *Client) ReconcileSegmentationPolicy(ctx context.Context, clusterID int,
	namespace, name string, spec SegmentationPolicySpec) (*ReconcileResult, error) {

	out := &ReconcileResult{}
	err := c.do(ctx, http.MethodPost, apiPrefix+"/segmentation-policies/reconcile",
		segmentationReconcileRequest{ClusterID: clusterID, Namespace: namespace, Name: name, Spec: spec},
		out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReconcileRuntimeSecurityPolicy pushes one CR's spec at the control plane.
// A 503 (see IsProviderUnavailable) means Tetragon was not confirmed
// installed and NOTHING was written — it is not a zero-edge success.
func (c *Client) ReconcileRuntimeSecurityPolicy(ctx context.Context, clusterID int,
	namespace, name string, spec RuntimeSecurityPolicySpec) (*ReconcileResult, error) {

	out := &ReconcileResult{}
	err := c.do(ctx, http.MethodPost, apiPrefix+"/runtime-security-policies/reconcile",
		runtimeSecurityReconcileRequest{ClusterID: clusterID, Namespace: namespace, Name: name, Spec: spec},
		out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CloseCREdges closes (never deletes) every graph edge tracked for one CR.
// Called from a finalizer; idempotent, and blocked by the kill switch.
func (c *Client) CloseCREdges(ctx context.Context, crKind string, clusterID int,
	namespace, name string) (*CloseResult, error) {

	out := &CloseResult{}
	err := c.do(ctx, http.MethodPost, apiPrefix+"/cr-edges/close",
		closeRequest{CRKind: crKind, ClusterID: clusterID, Namespace: namespace, Name: name},
		out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetPaused pushes PlatformConfig.spec.paused at the server-side persisted
// flag. This endpoint is itself never gated by the flag — otherwise a paused
// operator could not be unpaused through it.
func (c *Client) SetPaused(ctx context.Context, paused bool, by string) (*PausedState, error) {
	out := &PausedState{}
	body := struct {
		Paused bool   `json:"paused"`
		By     string `json:"by"`
	}{Paused: paused, By: by}
	if err := c.do(ctx, http.MethodPut, apiPrefix+"/paused", body, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetPaused reads the server-side persisted flag.
func (c *Client) GetPaused(ctx context.Context) (*PausedState, error) {
	out := &PausedState{}
	if err := c.do(ctx, http.MethodGet, apiPrefix+"/paused", nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// ProviderHealth reads flow + runtime provider health for one cluster.
// Read-only; not gated by the kill switch.
func (c *Client) ProviderHealth(ctx context.Context, clusterID int) (*ProviderHealthResponse, error) {
	out := &ProviderHealthResponse{}
	path := fmt.Sprintf("%s/provider-health?cluster_id=%d", apiPrefix, clusterID)
	if err := c.do(ctx, http.MethodGet, path, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}
