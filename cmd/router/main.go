// main.go
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
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

func loadConfig(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("failed to read config: %v", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("failed to parse config: %v", err)
	}
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
	idx := atomic.AddUint64(&pool.rr, 1)
	return pool.Endpoints[int(idx)%len(pool.Endpoints)]
}

func proxyStream(w http.ResponseWriter, r *http.Request, target string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.Path, r.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	req.Header = r.Header.Clone()

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()

	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	type Req struct {
		Model string `json:"model"`
		User  string `json:"user"`
	}

	var rq Req
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))

	json.Unmarshal(body, &rq)
	key := rq.User + ":" + rq.Model
	if val, ok := sticky.Load(key); ok {
		proxyStream(w, r, val.(string))
		return
	}

	pool := matchPool(rq.Model)
	if pool == nil {
		http.Error(w, "no pool for model", 400)
		return
	}

	endpoint := pickEndpoint(pool)
	sticky.Store(key, endpoint)
	proxyStream(w, r, endpoint)
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

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(combined)
}

func main() {
	configPath := flag.String("config", "pools.yaml", "path to pools config file")
	listen := flag.String("listen", "0.0.0.0:9090", "host:port to listen on")
	flag.Parse()

	loadConfig(*configPath)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", handleChat)
	mux.HandleFunc("/v1/models", handleModels)
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
