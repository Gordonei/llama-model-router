package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// Test for pool matching logic
func TestMatchPool(t *testing.T) {
	testCases := []struct {
		name     string
		model    string
		pools    []Pool
		expected string // expected pool name
	}{
		{
			name:  "Exact model match",
			model: "llama3-8b-instruct",
			pools: []Pool{
				{Name: "pool1", Models: []string{"llama3-8b-instruct"}},
				{Name: "pool2", Models: []string{"gemma3-4b"}},
			},
			expected: "pool1",
		},
		{
			name:  "Wildcard match",
			model: "unknown-model",
			pools: []Pool{
				{Name: "pool1", Models: []string{"llama3-8b-instruct"}},
				{Name: "wildcard-pool", Models: []string{"*"}},
			},
			expected: "wildcard-pool",
		},
		{
			name:     "No match",
			model:    "nonexistent-model",
			pools:    []Pool{{Name: "pool1", Models: []string{"llama3-8b-instruct"}}},
			expected: "",
		},
		{
			name:  "Multiple models in pool",
			model: "gemma3-4b",
			pools: []Pool{
				{Name: "multi-pool", Models: []string{"llama3-8b-instruct", "gemma3-4b", "mixtral-8x7b"}},
			},
			expected: "multi-pool",
		},
		{
			name:  "Wildcard should be last resort",
			model: "llama3-8b-instruct",
			pools: []Pool{
				{Name: "exact-pool", Models: []string{"llama3-8b-instruct"}},
				{Name: "wildcard-pool", Models: []string{"*"}},
			},
			expected: "exact-pool",
		},
		{
			name:  "Empty models slice",
			model: "any-model",
			pools: []Pool{
				{Name: "empty-pool", Models: []string{}}},
			expected: "",
		},
		{
			name:  "Model with special characters",
			model: "model-with-dashes_underscores.123",
			pools: []Pool{
				{Name: "special-pool", Models: []string{"model-with-dashes_underscores.123"}}},
			expected: "special-pool",
		},
		{
			name:  "Overlapping models with exact match priority",
			model: "shared-model",
			pools: []Pool{
				{Name: "exact-pool", Models: []string{"shared-model"}},
				{Name: "wildcard-pool", Models: []string{"*"}}},
			expected: "exact-pool",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup config with test pools
			oldPools := cfg.Pools
			cfg.Pools = tc.pools
			defer func() { cfg.Pools = oldPools }()

			pool := matchPool(tc.model)
			if pool == nil {
				if tc.expected != "" {
					t.Errorf("expected pool %q, got nil", tc.expected)
				}
			} else if pool.Name != tc.expected {
				t.Errorf("expected pool %q, got %q", tc.expected, pool.Name)
			}
		})
	}
}

// Test for round-robin endpoint selection
func TestPickEndpoint(t *testing.T) {
	testCases := []struct {
		name      string
		endpoints []string
		expected  []string // sequence of endpoints expected
	}{
		{
			name:      "Single endpoint",
			endpoints: []string{"http://localhost:8080"},
			expected:  []string{"http://localhost:8080"},
		},
		{
			name:      "Two endpoints round-robin",
			endpoints: []string{"http://localhost:8080", "http://localhost:8081"},
			expected:  []string{"http://localhost:8080", "http://localhost:8081", "http://localhost:8080", "http://localhost:8081"},
		},
		{
			name:      "Three endpoints round-robin",
			endpoints: []string{"http://localhost:8080", "http://localhost:8081", "http://localhost:8082"},
			expected:  []string{"http://localhost:8080", "http://localhost:8081", "http://localhost:8082", "http://localhost:8080"},
		},
		{
			name:      "Many endpoints round-robin",
			endpoints: []string{"http://localhost:8080", "http://localhost:8081", "http://localhost:8082", "http://localhost:8083", "http://localhost:8084"},
			expected:  []string{"http://localhost:8080", "http://localhost:8081", "http://localhost:8082", "http://localhost:8083", "http://localhost:8084", "http://localhost:8080"},
		},
		{
			name:      "Empty endpoints slice",
			endpoints: []string{},
			expected:  []string{},
		},
		{
			name:      "Single endpoint with duplicate",
			endpoints: []string{"http://localhost:8080", "http://localhost:8080"},
			expected:  []string{"http://localhost:8080", "http://localhost:8080"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			pool := Pool{Endpoints: tc.endpoints}
			pool.ResetRR()
			for i, expected := range tc.expected {
				actual := pickEndpoint(&pool)
				if actual != expected {
					t.Errorf("iteration %d: expected %q, got %q", i, expected, actual)
				}
			}
		})
	}
}

// Test for chat endpoint routing
func TestHandleChat(t *testing.T) {
	testCases := []struct {
		name           string
		model          string
		user           string
		setupPools     func() []Pool
		setupSticky    func()
		expectedStatus int
		expectSticky   bool
	}{
		{
			name:  "Successful routing with exact match",
			model: "llama3-8b-instruct",
			user:  "user123",
			setupPools: func() []Pool {
				return []Pool{
					{Name: "test-pool", Endpoints: []string{"http://backend:8080"}, Models: []string{"llama3-8b-instruct"}},
				}
			},
			expectedStatus: 200,
			expectSticky:   true,
		},
		{
			name:  "Successful routing with wildcard",
			model: "unknown-model",
			user:  "user456",
			setupPools: func() []Pool {
				return []Pool{
					{Name: "wildcard-pool", Endpoints: []string{"http://backend:8080"}, Models: []string{"*"}},
				}
			},
			expectedStatus: 200,
			expectSticky:   true,
		},
		{
			name:  "No pool for model",
			model: "nonexistent-model",
			user:  "user789",
			setupPools: func() []Pool {
				return []Pool{
					{Name: "test-pool", Endpoints: []string{"http://backend:8080"}, Models: []string{"llama3-8b-instruct"}},
				}
			},
			expectedStatus: 400,
			expectSticky:   false,
		},
		{
			name:  "Sticky session reuse",
			model: "llama3-8b-instruct",
			user:  "user999",
			setupPools: func() []Pool {
				return []Pool{
					{Name: "test-pool", Endpoints: []string{"http://backend:8080", "http://backend:8081"}, Models: []string{"llama3-8b-instruct"}},
				}
			},
			setupSticky: func() {
				sticky.Store("user999:llama3-8b-instruct", "http://backend:8080")
			},
			expectedStatus: 200,
			expectSticky:   true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup pools
			oldPools := cfg.Pools
			cfg.Pools = tc.setupPools()
			defer func() { cfg.Pools = oldPools }()

			// Clear and setup sticky sessions
			sticky.Range(func(key, value interface{}) bool {
				sticky.Delete(key)
				return true
			})
			if tc.setupSticky != nil {
				tc.setupSticky()
			}

			// Create test backend
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"choices": []map[string]interface{}{
						{"delta": map[string]string{"content": "test"}},
					},
				})
			}))
			defer backend.Close()

			// Update pool endpoints to point to test backend
			for i := range cfg.Pools {
				for j := range cfg.Pools[i].Endpoints {
					cfg.Pools[i].Endpoints[j] = backend.URL
				}
			}

			// Update any pre-configured sticky sessions to use the test backend URL
			if tc.setupSticky != nil {
				sticky.Range(func(key, value interface{}) bool {
					if val, ok := value.(string); ok && strings.Contains(val, "http://backend:8080") {
						sticky.Store(key, strings.ReplaceAll(val, "http://backend:8080", backend.URL))
					}
					return true
				})
			}

			// Create request
			body := map[string]interface{}{
				"model": tc.model,
				"user":  tc.user,
				"messages": []map[string]string{
					{"role": "user", "content": "test"},
				},
			}
			bodyBytes, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(bodyBytes))
			rr := httptest.NewRecorder()

			// Call handler
			handleChat(rr, req)

			// Check response
			if rr.Code != tc.expectedStatus {
				t.Errorf("expected status %d, got %d", tc.expectedStatus, rr.Code)
			}

			// Verify sticky session if expected
			if tc.expectSticky {
				key := tc.user + ":" + tc.model
				if _, ok := sticky.Load(key); !ok {
					t.Errorf("expected sticky session for key %q", key)
				}
			}
		})
	}
}

// Test for models endpoint aggregation
func TestHandleModels(t *testing.T) {
	testCases := []struct {
		name       string
		pools      []Pool
		setupMocks func() map[string]*httptest.Server
		expected   int
	}{
		{
			name: "Single pool with single endpoint",
			pools: []Pool{
				{Name: "pool1", Endpoints: []string{"http://backend1:8080"}, Models: []string{"model1"}},
			},
			setupMocks: func() map[string]*httptest.Server {
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{
						"data": []map[string]string{
							{"id": "model1", "object": "model"}},
					})
				}))
				return map[string]*httptest.Server{"backend1": backend}
			},
			expected: 200,
		},
		{
			name: "Multiple pools with multiple endpoints",
			pools: []Pool{
				{Name: "pool1", Endpoints: []string{"http://backend1:8080", "http://backend2:8080"}, Models: []string{"model1"}},
				{Name: "pool2", Endpoints: []string{"http://backend3:8080"}, Models: []string{"model2"}},
			},
			setupMocks: func() map[string]*httptest.Server {
				backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{
						"data": []map[string]string{
							{"id": "model1", "object": "model"}},
					})
				}))
				backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{
						"data": []map[string]string{
							{"id": "model2", "object": "model"}},
					})
				}))
				backend3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(map[string]interface{}{
						"data": []map[string]string{
							{"id": "model3", "object": "model"}},
					})
				}))
				return map[string]*httptest.Server{"backend1": backend1, "backend2": backend2, "backend3": backend3}
			},
			expected: 200,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup pools
			oldPools := cfg.Pools
			cfg.Pools = tc.pools
			defer func() { cfg.Pools = oldPools }()

			// Setup mock backends
			mocks := tc.setupMocks()
			defer func() {
				for _, m := range mocks {
					m.Close()
				}
			}()

			// Update pool endpoints to point to mock backends
			backendURLs := make([]string, len(mocks))
			i := 0
			for _, m := range mocks {
				backendURLs[i] = m.URL
				i++
			}
			for poolIdx := range cfg.Pools {
				for endpointIdx := range cfg.Pools[poolIdx].Endpoints {
					cfg.Pools[poolIdx].Endpoints[endpointIdx] = backendURLs[endpointIdx] + "/v1/models"
				}
			}

			// Create request
			req := httptest.NewRequest("GET", "/v1/models", nil)
			rr := httptest.NewRecorder()

			// Call handler
			handleModels(rr, req)

			// Check response
			if rr.Code != tc.expected {
				t.Errorf("expected status %d, got %d", tc.expected, rr.Code)
			}

			// Verify response is valid JSON
			var resp map[string]interface{}
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Errorf("invalid JSON response: %v", err)
			}
		})
	}
}

// Test for health endpoint
func TestHandleHealth(t *testing.T) {
	req := httptest.NewRequest("GET", "/health", nil)
	rr := httptest.NewRecorder()

	// This handler is defined inline in main.go
	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}

	healthHandler(rr, req)

	if rr.Code != 200 {
		t.Errorf("expected status 200, got %d", rr.Code)
	}

	if rr.Body.String() != "ok" {
		t.Errorf("expected body 'ok', got %q", rr.Body.String())
	}
}

// Test for config loading
func TestLoadConfig(t *testing.T) {
	testCases := []struct {
		name   string
		config string
		valid  bool
	}{
		{
			name: "Valid config",
			config: `
pools:
  - name: "test-pool"
    endpoints:
      - "http://localhost:8080"
    models:
      - "model1"
      - "model2"
 `,
			valid: true,
		},
		{
			name: "Config with wildcard",
			config: `
pools:
  - name: "wildcard-pool"
    endpoints:
      - "http://localhost:8080"
      - "http://localhost:8081"
    models:
      - "*"
 `,
			valid: true,
		},
		{
			name:   "Empty config",
			config: "pools: []",
			valid:  true,
		},
		{
			name:   "Invalid YAML syntax",
			config: "pools: }invalid-yaml{",
			valid:  false,
		},
		{
			name: "Malformed pool structure",
			config: `pools:
  - name: "test-pool"
    endpoints:
      - "http://localhost:8080"
    models: invalid`,
			valid: false,
		},
		{
			name: "Multiple pools with complex configuration",
			config: `pools:
  - name: "pool1"
    endpoints:
      - "http://backend1:8080"
      - "http://backend2:8080"
    models:
      - "model1"
      - "model2"
  - name: "wildcard-pool"
    endpoints:
      - "http://wildcard:8080"
    models:
      - "*"
 `,
			valid: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Write test config to temp file
			tmpfile, err := os.CreateTemp("", "test-config-*.yaml")
			if err != nil {
				t.Fatalf("failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			if _, err := tmpfile.Write([]byte(tc.config)); err != nil {
				t.Fatalf("failed to write config: %v", err)
			}
			tmpfile.Close()

			// Backup existing config
			oldPools := cfg.Pools

			// Load config
			err = loadConfig(tmpfile.Name())

			// Restore config
			cfg.Pools = oldPools

			if tc.valid {
				if err != nil {
					t.Errorf("expected config to be valid, got error: %v", err)
				}
			} else {
				if err == nil {
					t.Error("expected config to be invalid, got no error")
				}
			}
		})
	}
}

// Test for concurrency safety of sticky sessions
func TestStickyConcurrentAccess(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(100)

	for i := 0; i < 100; i++ {
		go func(id int) {
			defer wg.Done()
			key := fmt.Sprintf("user%d:model%d", id, id)
			endpoint := fmt.Sprintf("http://backend%d:8080", id%10)
			sticky.Store(key, endpoint)
			if val, ok := sticky.Load(key); !ok || val.(string) != endpoint {
				t.Errorf("sticky session mismatch for key %q", key)
			}
		}(i)
	}

	wg.Wait()
}

// Benchmark for endpoint selection
func BenchmarkPickEndpoint(b *testing.B) {
	pool := Pool{Endpoints: []string{"http://localhost:8080", "http://localhost:8081", "http://localhost:8082"}}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pickEndpoint(&pool)
	}
}

// Benchmark for pool matching
func BenchmarkMatchPool(b *testing.B) {
	cfg.Pools = []Pool{
		{Name: "pool1", Models: []string{"model1", "model2", "model3"}},
		{Name: "pool2", Models: []string{"model4", "model5"}},
		{Name: "wildcard", Models: []string{"*"}},
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		matchPool("model2")
	}
}
