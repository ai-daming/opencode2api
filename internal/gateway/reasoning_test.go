package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// Capture the actual HTTP body after routing, conversion and default effort.
// The fixture rejects every request: this tests transmission, not whether a
// real provider supports or honors disabling reasoning.
func TestReasoningControlsForwardedHTTP(t *testing.T) {
	inputs := []struct {
		name string
		from wire.Protocol
		off  map[string]any
	}{
		{"anthropic", wire.Anthropic, map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"responses", wire.Responses, map[string]any{"reasoning": map[string]any{"effort": "none"}}},
		{"chat-effort", wire.Chat, map[string]any{"reasoning_effort": "none"}},
		{"chat-thinking", wire.Chat, map[string]any{"thinking": map[string]any{"type": "disabled"}}},
	}
	defaults := []struct {
		name, effort, perModel, resolved string
		tier                             config.Tier
	}{
		{"unset", "", "", "", config.TierGo},
		{"high", "high", "", "high", config.TierGo},
		{"per-model", "low", "high", "high", config.TierGo},
		{"none", "none", "", "none", config.TierGo},
		{"auto-high", "auto", "", "high", config.TierZen},
	}
	for _, to := range []wire.Protocol{wire.Chat, wire.Responses, wire.Anthropic} {
		for _, def := range defaults {
			t.Run(string(to)+"/"+def.name, func(t *testing.T) {
				model := "reasoning-test"
				if def.effort == "auto" {
					model += "-free"
				}
				captured := make(chan map[string]any, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != wire.Path(to) {
						t.Errorf("upstream path = %s, want %s", r.URL.Path, wire.Path(to))
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					select {
					case captured <- body:
					default:
						t.Error("unexpected extra upstream attempt")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					io.WriteString(w, `{"error":{"message":"capture-only fixture"}}`)
				}))
				t.Cleanup(upstream.Close)
				cfgBody := map[string]any{
					"server_keys": []string{"local-test"}, string(def.tier) + "_keys": []string{"upstream-test"},
					"prefer": def.tier, "proxies": []string{"direct"},
					"upstream": map[string]string{"go": upstream.URL, "zen": upstream.URL},
					"models":   map[string]any{"protocols": map[string]string{model: string(to)}},
					"reasoning": map[string]any{
						"effort": def.effort, "effort_by_model": map[string]string{},
					},
					"retry": map[string]any{"max_attempts": 1, "timeout_seconds": 5},
				}
				if def.perModel != "" {
					cfgBody["reasoning"].(map[string]any)["effort_by_model"] = map[string]string{model: def.perModel}
				}
				cfgPath := filepath.Join(t.TempDir(), "config.json")
				encoded, err := json.Marshal(cfgBody)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cfgPath, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := config.Load(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), telemetry.NewMonitor())
				if err != nil {
					t.Fatal(err)
				}
				var zen, goModels []string
				if def.tier == config.TierZen {
					zen = []string{model}
				} else {
					goModels = []string{model}
				}
				g.catalog.ReplaceWithCapabilities(zen, goModels, nil, nil, map[config.Tier]map[string]models.Metadata{
					def.tier: {model: {Reasoning: true}},
				})
				if def.effort == "auto" {
					g.availability, err = models.NewAvailabilityStore("")
					if err != nil {
						t.Fatal(err)
					}
					err = g.availability.Record(model, 0, true, "", "key", time.Now(), models.EffortProbe{Effort: "high", Protocol: string(to)})
					if err != nil {
						t.Fatal(err)
					}
				}
				if got := g.resolvedEffort(model, def.tier, to); got != def.resolved {
					t.Fatalf("default effort = %q, want %q", got, def.resolved)
				}
				server := httptest.NewServer(g.Handler())
				t.Cleanup(server.Close)
				client := server.Client()
				client.Timeout = 10 * time.Second
				for _, in := range inputs {
					for _, off := range []bool{false, true} {
						for _, stream := range []bool{false, true} {
							t.Run(fmt.Sprintf("%s/off=%v/stream=%v", in.name, off, stream), func(t *testing.T) {
								body := map[string]any{"model": model, "stream": stream, "max_tokens": 2048,
									"messages": []any{map[string]any{"role": "user", "content": "Reply OK"}}}
								if in.from == wire.Responses {
									delete(body, "messages")
									body["input"] = "Reply OK"
								}
								if off {
									for key, value := range in.off {
										body[key] = value
									}
								}
								encoded, err := json.Marshal(body)
								if err != nil {
									t.Fatal(err)
								}
								req, err := http.NewRequest(http.MethodPost, server.URL+wire.Path(in.from), bytes.NewReader(encoded))
								if err != nil {
									t.Fatal(err)
								}
								req.Header.Set("Authorization", "Bearer local-test")
								req.Header.Set("Content-Type", "application/json")
								resp, err := client.Do(req)
								if err != nil {
									t.Fatal(err)
								}
								response, err := io.ReadAll(resp.Body)
								resp.Body.Close()
								if err != nil {
									t.Fatal(err)
								}
								if resp.StatusCode != 400 || !bytes.Contains(response, []byte("capture-only fixture")) {
									t.Fatalf("gateway status=%d body=%s", resp.StatusCode, response)
								}
								var got map[string]any
								select {
								case got = <-captured:
								default:
									t.Fatal("no outbound request")
								}
								// Free-tier requests are intentionally forced to SSE upstream.
								if got["stream"] != (stream || def.effort == "auto") {
									t.Fatal("stream flag changed")
								}
								controls := map[string]any{}
								for _, key := range []string{"thinking", "reasoning", "reasoning_effort", "output_config"} {
									if value, ok := got[key]; ok {
										controls[key] = value
									}
								}
								want := map[string]any{}
								if off {
									switch to {
									case wire.Chat:
										want["reasoning_effort"] = "none"
									case wire.Responses:
										want["reasoning"] = map[string]any{"effort": "none"}
									case wire.Anthropic:
										want["thinking"] = map[string]any{"type": "disabled"}
									}
									if in.from == to {
										want = in.off // Preserve a native provider field in same-protocol requests.
									}
								} else if def.resolved == "high" {
									switch to {
									case wire.Chat:
										want["reasoning_effort"] = "high"
									case wire.Responses:
										want["reasoning"] = map[string]any{"effort": "high"}
									case wire.Anthropic:
										want["thinking"] = map[string]any{"type": "enabled", "budget_tokens": float64(8192)}
										want["output_config"] = map[string]any{"effort": "high"}
									}
								}
								if !reflect.DeepEqual(controls, want) {
									t.Errorf("outbound controls = %#v, want %#v", controls, want)
								}
							})
						}
					}
				}
			})
		}
	}
}
