package dockerapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

func TestRunStepResourceLimits(t *testing.T) {
	for _, tc := range []struct {
		name         string
		memory, cpus int64
	}{
		{name: "bounded", memory: 128 << 20, cpus: 500_000_000},
		{name: "legacy"},
		{name: "memory only", memory: 64 << 20},
		{name: "CPU only", cpus: 1_000_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created := make(chan map[string]json.RawMessage, 1)
			deleted := make(chan struct{}, 1)
			mux := http.NewServeMux()
			mux.HandleFunc("GET /images/test-image/json", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{}`)
			})
			mux.HandleFunc("POST /containers/create", func(w http.ResponseWriter, r *http.Request) {
				var body struct{ HostConfig map[string]json.RawMessage }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					http.Error(w, "invalid JSON", http.StatusBadRequest)
					return
				}
				created <- body.HostConfig
				fmt.Fprint(w, `{"Id":"step"}`)
			})
			mux.HandleFunc("POST /containers/step/start", func(http.ResponseWriter, *http.Request) {})
			mux.HandleFunc("GET /containers/step/logs", func(http.ResponseWriter, *http.Request) {})
			mux.HandleFunc("POST /containers/step/wait", func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, `{"StatusCode":0}`)
			})
			mux.HandleFunc("DELETE /containers/step", func(w http.ResponseWriter, _ *http.Request) {
				deleted <- struct{}{}
				w.WriteHeader(http.StatusNoContent)
			})
			fakeDaemon(t, mux)
			client, err := Connect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := client.RunStep(context.Background(), Step{Image: "test-image", Cmd: []string{"true"}, MemoryBytes: tc.memory, NanoCPUs: tc.cpus}, io.Discard); err != nil {
				t.Fatal(err)
			}
			host := <-created
			for name, want := range map[string]int64{"Memory": tc.memory, "MemorySwap": tc.memory, "NanoCpus": tc.cpus} {
				raw, exists := host[name]
				if want == 0 {
					if exists {
						t.Errorf("legacy HostConfig includes %s=%s", name, raw)
					}
					continue
				}
				var got int64
				if !exists || json.Unmarshal(raw, &got) != nil || got != want {
					t.Errorf("HostConfig.%s = %s, want %d", name, raw, want)
				}
			}
			select {
			case <-deleted:
			default:
				t.Fatal("completed build container was not removed")
			}
		})
	}
}
