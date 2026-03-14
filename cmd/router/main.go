// main.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"gopkg.in/yaml.v3"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Pool struct {
	Name      string   `yaml:"name"`
	Endpoints []string `yaml:"endpoints"`
	Models    []string `yaml:"models"`
	rr        uint64
}

type Config struct {
	Pools []Pool `yaml:"pools"`
}

var cfg Config
var sticky sync.Map

func loadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("failed to read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("failed to parse config: %w", err)
	}
	return nil
}

func matchPool(model string) *Pool {
	for i := range cfg.Pools {
		for _, m := range cfg.Pools[i].Models {
			if m == model {
				return &cfg.Pools[i]
			}
		}
	}
	for i := range cfg.Pools {
		for _, m := range cfg.Pools[i].Models {
			if m == "*" {
				return &cfg.Pools[i]
			}
		}
	}
	return nil
}

func pickEndpoint(pool *Pool) string {
	if len(pool.Endpoints) == 1 {
		return pool.Endpoints[0]
	}
	idx := atomic.LoadUint64(&pool.rr)
	atomic.AddUint64(&pool.rr, 1)
	return pool.Endpoints[int(idx)%len(pool.Endpoints)]
}

// ResetRR resets the round-robin counter to 0 for testing purposes
func (p *Pool) ResetRR() {
	atomic.StoreUint64(&p.rr, 0)
}

func proxyStream(w http.ResponseWriter, r *http.Request, target string, body []byte) {
	// 1. Parse target to ensure we handle URLs correctly
	targetURL, err := url.Parse(target)
	if err != nil {
		http.Error(w, "Invalid target URL", http.StatusInternalServerError)
		return
	}

	// 2. Build the full destination path
	// This avoids double slashes and ensures query parameters are preserved
	destPath := strings.TrimSuffix(target, "/") + r.URL.Path
	if r.URL.RawQuery != "" {
		destPath += "?" + r.URL.RawQuery
	}

	// 3. Create the backend request
	// Note: If you read r.Body before this, you MUST pass a new Reader here.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, destPath, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 4. Clone headers and fix the Host
	req.Header = r.Header.Clone()
	req.Host = targetURL.Host
	req.ContentLength = int64(len(body))
	req.Header.Del("Transfer-Encoding")

	// 5. Execute the request
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "Backend unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 6. Transfer headers from backend to client
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	// CRITICAL: Strip headers that interfere with chunked streaming.
	// We delete Content-Length so Go's server can manage the chunked EOF properly.
	w.Header().Del("Content-Length")
	w.Header().Del("Connection")
	w.Header().Del("Keep-Alive")
	w.Header().Set("X-Accel-Buffering", "no") // Prevent Nginx buffering

	// 7. Initialize status and Flusher
	w.WriteHeader(resp.StatusCode)
	flusher, canFlush := w.(http.Flusher)

	// 8. The Streaming Loop
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// Client likely disconnected (benchy stopped)
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			// io.EOF means the stream ended naturally
			break
		}
	}
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	type Req struct {
		Model string `json:"model"`
		User  string `json:"user"`
	}

	var rq Req
	body, _ := io.ReadAll(r.Body)
	// r.Body.Close()
	// r.Body = io.NopCloser(bytes.NewReader(body))

	json.Unmarshal(body, &rq)
	key := rq.User + ":" + rq.Model
	if val, ok := sticky.Load(key); ok {
		log.Printf("routing to existing endpoint for user/model: %s", key)
		proxyStream(w, r, val.(string), body)
		return
	}

	pool := matchPool(rq.Model)
	if pool == nil {
		log.Printf("no pool found for model: %s", rq.Model)
		http.Error(w, "no pool for model", 400)
		return
	}

	endpoint := pickEndpoint(pool)
	sticky.Store(key, endpoint)
	log.Printf("routing to endpoint: %s for user/model: %s", endpoint, key)
	proxyStream(w, r, endpoint, body)
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	combined := make(map[string]interface{})

	for _, pool := range cfg.Pools {
		for _, ep := range pool.Endpoints {
			url := ep + "/v1/models"
			resp, err := http.Get(url)
			if err != nil {
				log.Printf("failed to get models from %s: %v", ep, err)
				continue
			}
			var data map[string]interface{}
			json.NewDecoder(resp.Body).Decode(&data)
			resp.Body.Close()
			for k, v := range data {
				combined[k] = v
			}
		}
	}

	log.Printf("returning combined models list with %d entries", len(combined))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(combined)
}

func main() {
	configPath := flag.String("config", "pools.yaml", "path to pools config file")
	listen := flag.String("listen", "0.0.0.0:9090", "host:port to listen on")
	flag.Parse()

	if err := loadConfig(*configPath); err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/chat/completions", handleChat)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:    *listen,
		Handler: mux,
	}

	go func() {
		log.Printf("router started on %s", *listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
