// Package takt provides a traefik provider plugin that publishes the
// services a takt server holds as traefik dynamic configuration.
//
// The plugin follows the takt API's list of services labelled
// "traefik.enable=true" and turns each into routers and a load-balanced
// service, with the backend addresses takt resolved for the healthy workload
// instances. Router configuration is read from the takt service's labels,
// following the same convention as traefik's docker provider:
// "traefik.http.routers.<name>.rule" and friends.
//
// Traefik interprets this package with yaegi, which is why it talks to the
// takt API with plain HTTP against vendored configuration types rather than
// through takt's client package.
package takt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type (
	// The Config type contains the fields traefik decodes from the plugin's
	// static configuration.
	Config struct {
		// The base URL of the takt API.
		Endpoint string `json:"endpoint,omitempty"`
		// How long to wait before reopening a stream the server ended or
		// refused, as a Go duration string.
		RetryInterval string `json:"retryInterval,omitempty"`
		// The bearer token the plugin presents, for a takt server with
		// authentication enabled. Empty presents no credential. Prefer
		// tokenFile, which keeps the token out of the static configuration
		// and picks up a rotation without a restart.
		Token string `json:"token,omitempty"`
		// The path to a file holding the bearer token, read fresh each time
		// a stream is opened so a rotated token is picked up without
		// restarting traefik. Set this or token, not both.
		TokenFile string `json:"tokenFile,omitempty"`
	}

	// The Provider type follows a takt server's services and publishes them
	// as traefik dynamic configuration.
	Provider struct {
		endpoint  string
		retry     time.Duration
		token     string
		tokenFile string
		client    *http.Client
		logger    *slog.Logger
		ctx       context.Context
		cancel    context.CancelFunc
	}
)

// CreateConfig returns the plugin's default configuration. Traefik calls it
// before decoding the operator's configuration over the top.
func CreateConfig() *Config {
	return &Config{
		Endpoint:      "http://127.0.0.1:7373",
		RetryInterval: "5s",
	}
}

// New returns a Provider that reads services from the takt server the
// configuration names. Traefik calls it once at startup.
func New(_ context.Context, config *Config, name string) (*Provider, error) {
	retry, err := time.ParseDuration(config.RetryInterval)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the retry interval: %w", err)
	}

	if retry <= 0 {
		return nil, errors.New("the retry interval must be greater than zero")
	}

	endpoint, err := url.Parse(config.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse the endpoint: %w", err)
	}

	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("the endpoint %q is not an http or https URL", config.Endpoint)
	}

	if config.Token != "" && config.TokenFile != "" {
		return nil, errors.New("set only one of token and tokenFile")
	}

	// Read once here so a missing or unreadable file stops traefik at
	// startup rather than turning every stream into a 401.
	if config.TokenFile != "" {
		if _, err = os.ReadFile(config.TokenFile); err != nil {
			return nil, fmt.Errorf("failed to read the token file: %w", err)
		}
	}

	// The stream outlives the context traefik hands New, so the provider
	// carries its own, ended by Stop.
	ctx, cancel := context.WithCancel(context.Background())

	return &Provider{
		endpoint:  strings.TrimSuffix(endpoint.String(), "/"),
		retry:     retry,
		token:     config.Token,
		tokenFile: config.TokenFile,
		client:    &http.Client{},
		logger:    slog.New(slog.NewTextHandler(os.Stdout, nil)).With("plugin", name),
		ctx:       ctx,
		cancel:    cancel,
	}, nil
}

// authorization returns the bearer token the plugin presents. It reads the
// token file on each call, so a rotated token is picked up when the next
// stream opens, and falls back to the static token, or to empty when neither
// is configured.
func (p *Provider) authorization() (string, error) {
	if p.tokenFile == "" {
		return p.token, nil
	}

	data, err := os.ReadFile(p.tokenFile)
	if err != nil {
		return "", fmt.Errorf("failed to read the token file: %w", err)
	}

	return strings.TrimSpace(string(data)), nil
}

// Init reports whether the provider is ready to run. The configuration was
// validated in New, so there is nothing left to check.
func (p *Provider) Init() error {
	return nil
}

// Provide publishes a dynamic configuration on the channel whenever the
// services takt reports produce one that differs from the last published.
// Traefik owns the channel, and ends the stream through Stop.
func (p *Provider) Provide(cfgChan chan<- json.Marshaler) error {
	go p.follow(cfgChan)

	return nil
}

// Stop ends the stream. Traefik calls it once on shutdown.
func (p *Provider) Stop() error {
	p.cancel()

	return nil
}

// follow keeps a stream of the services open, sending the configuration each
// set produces, and reopens it after the retry interval when it ends or fails.
// The last configuration stands in the meantime, so a briefly unreachable
// server does not empty traefik's routing table.
func (p *Provider) follow(cfgChan chan<- json.Marshaler) {
	var last []byte
	publish := func(services []taktService) error {
		payload, err := json.Marshal(p.buildConfiguration(services))
		if err != nil {
			return fmt.Errorf("failed to marshal the configuration: %w", err)
		}

		if bytes.Equal(payload, last) {
			return nil
		}

		select {
		case cfgChan <- json.RawMessage(payload):
			last = payload
		case <-p.ctx.Done():
		}

		return nil
	}

	for {
		err := p.streamServices(p.ctx, publish)
		switch {
		case p.ctx.Err() != nil:
			return
		case err != nil:
			p.logger.With("error", err).Error("failed to follow the services")
		default:
			p.logger.Warn("the server ended the stream")
		}

		select {
		case <-p.ctx.Done():
			return
		case <-time.After(p.retry):
		}
	}
}
