package orca

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// enableQuery is the label filter the plugin sends with every list request,
// so the server only returns services that opted in with the
// "traefik.enable=true" label.
const enableQuery = `$.labels."traefik.enable"=true`

type (
	// The orcaService type is the slice of the orca API's service resource the
	// plugin reads: the name, the labels carrying the traefik configuration,
	// the target's protocol, and the resolved backend addresses.
	orcaService struct {
		// The name that identifies the service.
		Name string `json:"name"`
		// Key-value pairs attached to the service.
		Labels map[string]string `json:"labels"`
		// Which workload instances the service selects.
		Target orcaTarget `json:"target"`
		// The selected instances that are fit to serve.
		Backends []orcaBackend `json:"backends"`
	}

	// The orcaTarget type is the part of a service's target the plugin reads.
	orcaTarget struct {
		// The transport protocol of the target port. Empty means tcp.
		Protocol string `json:"protocol"`
	}

	// The orcaBackend type is one address a service balances requests across.
	orcaBackend struct {
		// The host address that reaches the instance, as "host:port".
		Address string `json:"address"`
	}
)

// fetchServices reads the opted-in services from the orca server.
func (p *Provider) fetchServices(ctx context.Context) ([]orcaService, error) {
	target := p.endpoint + "/api/v1/services?query=" + url.QueryEscape(enableQuery)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create the request: %w", err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send the request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server answered with status %d", resp.StatusCode)
	}

	var result struct {
		Services []orcaService `json:"services"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode the response: %w", err)
	}

	return result.Services, nil
}
