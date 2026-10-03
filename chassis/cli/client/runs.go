package client

// Live runs: the tenant's runs in flight on the chassis, and the abort of
// one or of every run of a stack (admin/runs.go).

import (
	"context"
	"net/http"
	"net/url"
)

// LiveRun is one run in flight, as the chassis lists it.
type LiveRun struct {
	RID       string `json:"rid"`
	Tenant    string `json:"tenant"`
	Src       string `json:"src"`
	Entry     string `json:"entry,omitempty"`
	Stack     string `json:"stack,omitempty"`
	Stage     string `json:"stage,omitempty"`
	Started   string `json:"started"`
	AgeMs     int64  `json:"age_ms"`
	AbortedBy string `json:"aborted_by,omitempty"`
}

type listRunsResponse struct {
	Runs []LiveRun `json:"runs"`
}

// AbortResult is what an abort answers: how many runs it ended.
type AbortResult struct {
	Aborted int    `json:"aborted"`
	RID     string `json:"rid,omitempty"`
	Stack   string `json:"stack,omitempty"`
	// Published: on a fleet, the abort was also sent to every node as a
	// control event; Aborted counts the admin plane's own process only.
	Published bool `json:"published,omitempty"`
}

// ListRuns returns the target tenant's runs in flight on the chassis.
func (c *Client) ListRuns(ctx context.Context) ([]LiveRun, error) {
	var out listRunsResponse
	if err := c.DoScoped(ctx, http.MethodGet, "/runs", nil, &out); err != nil {
		return nil, err
	}
	if out.Runs == nil {
		out.Runs = []LiveRun{}
	}
	return out.Runs, nil
}

// AbortRun ends one run by rid. A run that is not in flight on the chassis
// answers a 404 HTTPError.
func (c *Client) AbortRun(ctx context.Context, rid, reason string) (AbortResult, error) {
	var out AbortResult
	err := c.DoScoped(ctx, http.MethodPost, "/runs/"+url.PathEscape(rid)+"/abort",
		map[string]string{"reason": reason}, &out)
	return out, err
}

// AbortStack ends every run of the stack in flight on the chassis.
func (c *Client) AbortStack(ctx context.Context, stack, reason string) (AbortResult, error) {
	var out AbortResult
	err := c.DoScoped(ctx, http.MethodPost, stackPath(stack, "/abort"),
		map[string]string{"reason": reason}, &out)
	return out, err
}
