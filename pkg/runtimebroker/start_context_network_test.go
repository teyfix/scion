package runtimebroker

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestBuildStartContextDockerPolicyRuntimeScope(t *testing.T) {
	for _, runtimeName := range []string{"mock", "container", "kubernetes", "docker", "podman"} {
		for _, templateState := range []string{"missing", "malformed"} {
			t.Run(runtimeName+"/"+templateState, func(t *testing.T) {
				cfg := DefaultServerConfig()
				cfg.StateDir = t.TempDir()
				srv := newTestServerForStartContextRuntime(t, cfg, runtimeName)
				template := filepath.Join(t.TempDir(), "template")
				if templateState == "malformed" {
					if err := os.Mkdir(template, 0755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(template, "scion-agent.json"), []byte(`{"docker":`), 0644); err != nil {
						t.Fatal(err)
					}
				}
				_, err := srv.buildStartContext(context.Background(), startContextInputs{
					Name: "policy-agent", Operation: opCreate,
					Config: &CreateAgentConfig{Template: template},
				})
				if runtimeName != "docker" && runtimeName != "podman" {
					if err != nil {
						t.Fatalf("%s template resolution should reach the manager: %v", runtimeName, err)
					}
					return
				}
				var policyErr *startContextError
				if !errors.As(err, &policyErr) || policyErr.Status != http.StatusConflict || policyErr.Message != "Failed to resolve Docker network policy" || policyErr.OriginalErr == nil {
					t.Fatalf("Docker policy resolution must fail closed: %v", err)
				}
			})
		}
	}
}

func TestBuildStartContextDockerPolicyUsesDispatchRuntime(t *testing.T) {
	for _, runtimes := range [][2]string{{"docker", "kubernetes"}, {"kubernetes", "docker"}} {
		t.Run(runtimes[0]+"-to-"+runtimes[1], func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			srv, project := newTestServerForStartContextMultiProfile(t, cfg, runtimes[0], "other", runtimes[1])
			_, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "policy-agent", ProjectPath: project, Operation: opCreate,
				Config: &CreateAgentConfig{Profile: "other", Template: "missing-template"},
			})
			if runtimes[1] == "docker" {
				var policyErr *startContextError
				if !errors.As(err, &policyErr) || policyErr.Message != "Failed to resolve Docker network policy" {
					t.Fatalf("Docker dispatch must resolve Docker policy: %v", err)
				}
			} else if err != nil {
				t.Fatalf("Kubernetes dispatch should reach the manager: %v", err)
			}
		})
	}
}

func TestBuildStartContextRetainedRecoveryStillValidatesNonDocker(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.StateDir = t.TempDir()
	srv := newTestServerForStartContext(t, cfg)
	_, err := srv.buildStartContext(context.Background(), startContextInputs{
		Name: "missing-retained-agent", Operation: opHTTPStart,
		RuntimeRecovery: &api.RuntimeRecovery{AgentID: "agent", ProjectID: "project", RuntimeBrokerID: "broker", AdmissionVersion: 1},
	})
	var policyErr *startContextError
	if !errors.As(err, &policyErr) || policyErr.Status != http.StatusConflict || policyErr.Message != "Failed to resolve Docker network policy" || policyErr.OriginalErr == nil {
		t.Fatalf("retained recovery must validate state before dispatch: %v", err)
	}
}

func TestBuildStartContextDockerPolicyTiersKeepCallbackConsistent(t *testing.T) {
	for _, runtimeName := range []string{"docker", "podman"} {
		t.Run(runtimeName, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.StateDir = t.TempDir()
			cfg.HubEndpoint = "http://localhost:8080"
			srv := newTestServerForStartContextRuntime(t, cfg, runtimeName)
			t.Setenv(runtime.ForceHostNetworkEnvVar, "1")
			project, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			project = filepath.Join(project, ".scion")
			settings := "schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: " + runtimeName + "\n    docker:\n      networks: [profile-preview]\nruntimes:\n  " + runtimeName + ":\n    type: " + runtimeName + "\n    docker:\n      networks: [runtime-preview]\n"
			if err := os.WriteFile(filepath.Join(project, "settings.yaml"), []byte(settings), 0644); err != nil {
				t.Fatal(err)
			}
			template := t.TempDir()
			if err := os.WriteFile(filepath.Join(template, "scion-agent.json"), []byte(`{"docker":{"networks":["template-preview"]}}`), 0644); err != nil {
				t.Fatal(err)
			}
			sc, err := srv.buildStartContext(context.Background(), startContextInputs{
				Name: "policy-agent", ProjectPath: project, Operation: opCreate,
				Config:       &CreateAgentConfig{Template: template},
				InlineConfig: &api.ScionConfig{Docker: &api.DockerConfig{Networks: []string{"request-preview"}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			host := "host.docker.internal"
			if runtimeName == "podman" {
				host = "host.containers.internal"
			}
			wantEndpoint := "http://" + host + ":8080"
			if sc.Opts.Env["SCION_NETWORK_MODE"] != "runtime-preview" || sc.Opts.Env["SCION_HUB_ENDPOINT"] != wantEndpoint || sc.Opts.Env["SCION_HUB_URL"] != wantEndpoint || sc.Opts.TrustedHubEndpoint != wantEndpoint {
				t.Fatalf("effective network and Hub callback disagree: env=%v trusted=%q", sc.Opts.Env, sc.Opts.TrustedHubEndpoint)
			}
		})
	}
}
