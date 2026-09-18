package takt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// enableQuery is the label filter the plugin sends with every request, so the
// server only returns services that opted in with the "traefik.enable=true"
// label.
const enableQuery = `$.labels."traefik.enable"=true`

type (
	// The taktService type is the slice of the takt API's service resource the
	// plugin reads: the name, the labels carrying the traefik configuration,
	// the target's protocol, and the resolved backend addresses.
	taktService struct {
		// The name that identifies the service.
		Name string `json:"name"`
		// Key-value pairs attached to the service.
		Labels map[string]string `json:"labels"`
		// Which workload instances the service selects.
		Target taktTarget `json:"target"`
		// The selected instances that are fit to serve.
		Backends []taktBackend `json:"backends"`
	}

	// The taktTarget type is the part of a service's target the plugin reads.
	taktTarget struct {
		// The transport protocol of the target port. Empty means tcp.
		Protocol string `json:"protocol"`
	}

	// The taktBackend type is one address a service balances requests across.
	taktBackend struct {
		// The host address that reaches the instance, as "host:port".
		Address string `json:"address"`
	}
)

// streamServices follows the opted-in services on the takt server, calling fn
// with the whole set each time the server writes it. The first set arrives at
// once, and the rest as the backends change. It returns nil when the server
// ends the stream, and the caller's context error when the caller does.
func (p *Provider) streamServices(ctx context.Context, fn func([]taktService) error) error {
	target := p.endpoint + "/api/v1/services?follow=true&query=" + url.QueryEscape(enableQuery)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("failed to create the request: %w", err)
	}

	token, err := p.authorization()
	if err != nil {
		return err
	}

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send the request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the server answered with status %d", resp.StatusCode)
	}

	// Each line is the whole set, in the shape a plain list answers with.
	decoder := json.NewDecoder(resp.Body)
	for {
		var line struct {
			Services []taktService `json:"services"`
		}

		err = decoder.Decode(&line)
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			return fmt.Errorf("failed to decode the response: %w", err)
		}

		if err = fn(line.Services); err != nil {
			return err
		}
	}
}
