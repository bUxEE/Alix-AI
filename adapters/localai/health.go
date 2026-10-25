package localai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/buxee/alix-ai/internal/providers"
	"github.com/buxee/alix-ai/pkg/contracts"
)

// maxModelEntries bounds the models read from /v1/models.
const maxModelEntries = 10000

// Version asks the runtime which release it is (GET /version, which needs the
// key). It is how callers refuse an unqualified runtime (SEC-072).
func (c *Client) Version(ctx context.Context) (RuntimeVersion, error) {
	var out struct {
		Version string `json:"version"`
	}
	if err := c.getJSON(ctx, pathVer, &out); err != nil {
		return RuntimeVersion{}, err
	}
	return ParseVersion(out.Version), nil
}

// ListModels returns the ids of the models the runtime lists (GET /v1/models).
// It reads the list only; nothing here can install or remove a model.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, pathModels, &out); err != nil {
		return nil, err
	}
	if len(out.Data) > maxModelEntries {
		return nil, schemaInvalid("runtime listed an implausible number of models")
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	return ids, nil
}

// Short, fixed reasons. A reason never carries text from the runtime.
const (
	reasonUnreachable   = "runtime unreachable"
	reasonProbeTimeout  = "readiness probe timed out"
	reasonStarting      = "runtime starting: startup preload in progress"
	reasonKeyRejected   = "runtime rejected the configured API key"
	reasonNoVersion     = "runtime reported no version"
	reasonVersionRead   = "runtime version unreadable"
	reasonUnqualified   = "unqualified LocalAI version"
	reasonModelList     = "model list unreadable"
	reasonNoController  = "controller model not installed"
	reasonModelsMissing = "configured model not installed"
)

// Health implements providers.AIRuntimeProvider. It checks, in order:
//
//  1. GET /readyz (no key): 200 is ready, 503 means the startup preload is
//     still running.
//  2. GET /version: reported in HealthReport.Version. With
//     Config.QualifiedVersions set, any other version is unavailable.
//  3. GET /v1/models: LocalAI answers /readyz with 200 even when a model
//     failed to download, so the configured models must be listed. A missing
//     controller model is unavailable; a missing router, embedding or profile
//     model is degraded.
//
// An unreachable or not-ready runtime is a successful probe that reports
// unavailable (the error return is for a probe that could not run: the
// caller's context ended).
func (c *Client) Health(ctx context.Context) (providers.HealthReport, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	probeFailed := func(err error) (providers.HealthReport, error) {
		if perr := parent.Err(); perr != nil {
			return providers.HealthReport{}, providers.ContextError(perr)
		}
		reason := reasonUnreachable
		if contracts.HasCode(err, contracts.ErrTimeout) {
			reason = reasonProbeTimeout
		}
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: reason}, nil
	}

	// 1. readiness
	resp, err := c.exchange(ctx, http.MethodGet, pathReady, nil, "application/json")
	if err != nil {
		return probeFailed(err)
	}
	status := resp.StatusCode
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	_ = resp.Body.Close()
	switch {
	case status == http.StatusOK:
	case status == http.StatusServiceUnavailable:
		return providers.HealthReport{Status: providers.HealthUnavailable, Reason: reasonStarting}, nil
	default:
		return providers.HealthReport{
			Status: providers.HealthUnavailable,
			Reason: fmt.Sprintf("readiness probe answered HTTP %d", status),
		}, nil
	}

	// 2. version
	ver, err := c.Version(ctx)
	if err != nil {
		if parent.Err() != nil {
			return probeFailed(err)
		}
		switch {
		case isUpstreamAuthFailure(err):
			return providers.HealthReport{Status: providers.HealthUnavailable, Reason: reasonKeyRejected}, nil
		case contracts.HasCode(err, contracts.ErrTimeout):
			// Ready a moment ago and now silent: not a state to serve from.
			return providers.HealthReport{Status: providers.HealthUnavailable, Reason: reasonProbeTimeout}, nil
		case contracts.HasCode(err, contracts.ErrUnavailable):
			return providers.HealthReport{Status: providers.HealthUnavailable, Reason: reasonUnreachable}, nil
		case len(c.cfg.QualifiedVersions) > 0:
			return providers.HealthReport{Status: providers.HealthUnavailable, Reason: reasonVersionRead}, nil
		}
		return providers.HealthReport{Status: providers.HealthDegraded, Reason: reasonVersionRead}, nil
	}
	report := providers.HealthReport{Status: providers.HealthOK, Version: ver.Version}
	if len(c.cfg.QualifiedVersions) > 0 && !c.cfg.isQualified(ver.Version) {
		report.Status, report.Reason = providers.HealthUnavailable, reasonUnqualified
		return report, nil
	}
	if ver.Version == "" {
		report.Status, report.Reason = providers.HealthDegraded, reasonNoVersion
	}

	// 3. models
	listed, err := c.ListModels(ctx)
	if err != nil {
		if parent.Err() != nil {
			return probeFailed(err)
		}
		report.Status, report.Reason = providers.HealthUnavailable, reasonModelList
		return report, nil
	}
	required, optional := c.configuredModels()
	var reasons []string
	for _, m := range required {
		if !slices.Contains(listed, m) {
			report.Status = providers.HealthUnavailable
			reasons = append(reasons, reasonNoController)
			break
		}
	}
	for _, m := range optional {
		if !slices.Contains(listed, m) {
			if report.Status == providers.HealthOK {
				report.Status = providers.HealthDegraded
			}
			reasons = append(reasons, reasonModelsMissing)
			break
		}
	}
	if len(reasons) > 0 {
		report.Reason = strings.Join(reasons, "; ")
	}
	return report, nil
}

func isUpstreamAuthFailure(err error) bool {
	var ce *contracts.Error
	if !errors.As(err, &ce) {
		return false
	}
	s, _ := ce.Details["upstream_status"].(int)
	return s == http.StatusUnauthorized || s == http.StatusForbidden
}

// configuredModels returns the default controller model (its absence makes
// the runtime unusable) and every other configured model (its absence only
// degrades it), without duplicates.
func (c *Client) configuredModels() (required, optional []string) {
	add := func(list *[]string, name string) {
		if name != "" && !slices.Contains(*list, name) && !slices.Contains(required, name) {
			*list = append(*list, name)
		}
	}
	add(&required, c.cfg.Models.Controller)
	add(&optional, c.cfg.Models.Router)
	add(&optional, c.cfg.Models.Embedder)
	names := make([]string, 0, len(c.cfg.Profiles))
	for n := range c.cfg.Profiles {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		p := c.cfg.Profiles[n]
		add(&optional, p.Controller)
		add(&optional, p.Router)
		add(&optional, p.Embedder)
	}
	return required, optional
}
