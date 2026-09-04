package orca_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	orca "github.com/dsb-labs/traefik-plugin-orca"
)

func TestNew(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name         string
		Config       orca.Config
		ExpectsError bool
	}{
		{
			Name:   "valid",
			Config: orca.Config{Endpoint: "http://127.0.0.1:7373", PollInterval: "5s"},
		},
		{
			Name:         "unparseable interval",
			Config:       orca.Config{Endpoint: "http://127.0.0.1:7373", PollInterval: "soon"},
			ExpectsError: true,
		},
		{
			Name:         "zero interval",
			Config:       orca.Config{Endpoint: "http://127.0.0.1:7373", PollInterval: "0s"},
			ExpectsError: true,
		},
		{
			Name:         "endpoint without a scheme",
			Config:       orca.Config{Endpoint: "127.0.0.1:7373", PollInterval: "5s"},
			ExpectsError: true,
		},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			provider, err := orca.New(t.Context(), &tc.Config, "orca")
			if tc.ExpectsError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NoError(t, provider.Init())
		})
	}
}

func TestProvider_Provide(t *testing.T) {
	t.Parallel()

	tt := []struct {
		Name string
		File string
	}{
		{Name: "http routers from labels", File: "http"},
		{Name: "tcp routers fill the tcp section", File: "tcp"},
		{Name: "udp target fills the udp section", File: "udp"},
		{Name: "no routers still generates the service", File: "bare"},
		{Name: "load balancer options apply", File: "options"},
		{Name: "sticky sessions and health checks apply", File: "session"},
		{Name: "labels that fail to decode are skipped", File: "ignored"},
	}

	for _, tc := range tt {
		t.Run(tc.Name, func(t *testing.T) {
			response, err := os.ReadFile(filepath.Join("testdata", tc.File+"_response.json"))
			require.NoError(t, err)
			expected, err := os.ReadFile(filepath.Join("testdata", tc.File+"_expected.json"))
			require.NoError(t, err)

			cfgChan := startProvider(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, `$.labels."traefik.enable"=true`, r.URL.Query().Get("query"))
				w.Write(response)
			})

			actual, err := json.Marshal(receive(t, cfgChan))
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(actual))
		})
	}

	t.Run("an unchanged configuration is not resent", func(t *testing.T) {
		first, err := os.ReadFile(filepath.Join("testdata", "bare_response.json"))
		require.NoError(t, err)
		second, err := os.ReadFile(filepath.Join("testdata", "http_response.json"))
		require.NoError(t, err)

		var polls atomic.Int64
		cfgChan := startProvider(t, func(w http.ResponseWriter, r *http.Request) {
			if polls.Add(1) <= 2 {
				w.Write(first)
				return
			}

			w.Write(second)
		})

		one, err := json.Marshal(receive(t, cfgChan))
		require.NoError(t, err)
		two, err := json.Marshal(receive(t, cfgChan))
		require.NoError(t, err)

		expectedFirst, err := os.ReadFile(filepath.Join("testdata", "bare_expected.json"))
		require.NoError(t, err)
		expectedSecond, err := os.ReadFile(filepath.Join("testdata", "http_expected.json"))
		require.NoError(t, err)

		assert.JSONEq(t, string(expectedFirst), string(one))
		assert.JSONEq(t, string(expectedSecond), string(two))
		assert.GreaterOrEqual(t, polls.Load(), int64(3))
	})

	t.Run("a failed read keeps the polling alive", func(t *testing.T) {
		response, err := os.ReadFile(filepath.Join("testdata", "bare_response.json"))
		require.NoError(t, err)

		var polls atomic.Int64
		cfgChan := startProvider(t, func(w http.ResponseWriter, r *http.Request) {
			if polls.Add(1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}

			w.Write(response)
		})

		actual, err := json.Marshal(receive(t, cfgChan))
		require.NoError(t, err)

		expected, err := os.ReadFile(filepath.Join("testdata", "bare_expected.json"))
		require.NoError(t, err)
		assert.JSONEq(t, string(expected), string(actual))
	})
}

// startProvider runs a Provider polling a test server that answers with the
// given handler, returning the channel the provider publishes on. The server
// and the provider are stopped when the test ends.
func startProvider(t *testing.T, handler http.HandlerFunc) chan json.Marshaler {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	provider, err := orca.New(t.Context(), &orca.Config{Endpoint: server.URL, PollInterval: "10ms"}, "orca")
	require.NoError(t, err)
	require.NoError(t, provider.Init())

	cfgChan := make(chan json.Marshaler, 16)
	require.NoError(t, provider.Provide(cfgChan))
	t.Cleanup(func() {
		assert.NoError(t, provider.Stop())
	})

	return cfgChan
}

// receive returns the next configuration the provider publishes, failing the
// test when none arrives in time.
func receive(t *testing.T, cfgChan <-chan json.Marshaler) json.Marshaler {
	t.Helper()

	select {
	case payload := <-cfgChan:
		return payload
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for a configuration")
		return nil
	}
}
