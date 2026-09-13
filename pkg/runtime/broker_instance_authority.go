// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BrokerInstanceAuthority is v1 trusted launcher input. Mount its containing
// directory read-only into only the broker, and replace the file atomically
// after final registration on every recreation. It contains no credentials.
type BrokerInstanceAuthority struct {
	Version        int    `json:"version"`
	BrokerID       string `json:"brokerId"`
	Runtime        string `json:"runtime"`
	DaemonID       string `json:"daemonId"`
	ContainerID    string `json:"containerId"`
	ComposeProject string `json:"composeProject"`
	ComposeService string `json:"composeService"`
	Hostname       string `json:"hostname"`
}

// BrokerInstanceAuthorityConfigurer is broker-only dependency injection. IDs
// from agent environment/configuration never install retirement authority.
type BrokerInstanceAuthorityConfigurer interface {
	ConfigureBrokerInstanceAuthority(string, func() (string, error))
}

func (r *DockerRuntime) ConfigureBrokerInstanceAuthority(path string, brokerID func() (string, error)) {
	r.instanceAuthorityPath, r.authenticatedBrokerID = path, brokerID
}

func (r *DockerRuntime) currentBrokerInstance(ctx context.Context) (string, error) {
	if r.instanceAuthorityPath == "" {
		return "", nil // No exemption: existing foreign/ancestor guard stays closed.
	}
	authority, err := readBrokerInstanceAuthority(r.instanceAuthorityPath)
	if err != nil {
		return "", fmt.Errorf("trusted broker instance authority unavailable: %w", err)
	}
	if r.authenticatedBrokerID == nil {
		return "", fmt.Errorf("authenticated broker identity unavailable")
	}
	brokerID, err := r.authenticatedBrokerID()
	if err != nil || brokerID == "" || brokerID != authority.BrokerID {
		return "", fmt.Errorf("trusted instance authority does not match authenticated broker identity")
	}
	// The actual selected command/daemon is authoritative, not endpoint strings.
	out, err := exec.CommandContext(ctx, r.Command, "info", "--format", "{{.ID}}").Output()
	if err != nil || strings.TrimSpace(string(out)) != authority.DaemonID {
		return "", fmt.Errorf("trusted instance authority does not match selected daemon")
	}
	out, err = exec.CommandContext(ctx, r.Command, "ps", "-a", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+authority.ComposeProject,
		"--filter", "label=com.docker.compose.service="+authority.ComposeService,
		"--format", "{{.ID}}").Output()
	ids := strings.Fields(string(out))
	if err != nil || len(ids) != 1 || ids[0] != authority.ContainerID {
		return "", fmt.Errorf("trusted broker instance is missing, stale or ambiguous")
	}
	var facts struct {
		ID     string `json:"id"`
		Config struct {
			Hostname string            `json:"Hostname"`
			Labels   map[string]string `json:"Labels"`
		} `json:"config"`
		State struct {
			Running bool `json:"Running"`
		} `json:"state"`
		Mounts []struct {
			Destination string `json:"Destination"`
			Type        string `json:"Type"`
			RW          *bool  `json:"RW"`
		} `json:"mounts"`
	}
	out, err = exec.CommandContext(ctx, r.Command, "inspect", "--format",
		`{"id":{{json .Id}},"config":{"Hostname":{{json .Config.Hostname}},"Labels":{{json .Config.Labels}}},"state":{"Running":{{json .State.Running}}},"mounts":{{json .Mounts}}}`,
		authority.ContainerID).Output()
	if err != nil || json.Unmarshal(out, &facts) != nil || facts.ID != authority.ContainerID || !facts.State.Running ||
		facts.Config.Hostname != authority.Hostname || facts.Config.Labels["com.docker.compose.project"] != authority.ComposeProject ||
		facts.Config.Labels["com.docker.compose.service"] != authority.ComposeService {
		return "", fmt.Errorf("trusted broker instance inspection cross-check failed")
	}
	// A direct file bind pins the old inode after atomic replacement. Require
	// the launcher-owned directory itself, read-only, so each open sees refresh.
	directory := filepath.Dir(r.instanceAuthorityPath)
	directoryMount := false
	for _, mount := range facts.Mounts {
		if mount.Destination != directory && retirementMountWithin(directory, mount.Destination) {
			return "", fmt.Errorf("trusted authority directory is shadowed by a nested mount")
		}
		if mount.Destination == directory && mount.Type == "bind" && mount.RW != nil && !*mount.RW {
			directoryMount = true
		}
	}
	if directoryMount {
		return authority.ContainerID, nil
	}
	return "", fmt.Errorf("trusted broker authority directory is not mounted read-only")
}

func readBrokerInstanceAuthority(path string) (BrokerInstanceAuthority, error) {
	var authority BrokerInstanceAuthority
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return authority, fmt.Errorf("authority path must be clean and absolute")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return authority, fmt.Errorf("authority path must exist without symlink aliases")
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil || !dirInfo.IsDir() || dirInfo.Mode().Perm()&0022 != 0 {
		return authority, fmt.Errorf("authority directory is not launcher-protected")
	}
	file, err := os.Open(path)
	if err != nil {
		return authority, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Size() > 16384 {
		return authority, fmt.Errorf("authority file is not protected regular input")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16385))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&authority); err != nil {
		return authority, fmt.Errorf("invalid authority JSON")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return authority, fmt.Errorf("trailing authority JSON")
	}
	if authority.Version != 1 || authority.Runtime != "docker" || authority.BrokerID == "" || authority.DaemonID == "" ||
		len(authority.ContainerID) != 64 || strings.Trim(authority.ContainerID, "0123456789abcdef") != "" ||
		authority.ComposeProject == "" || authority.ComposeService == "" || authority.Hostname == "" {
		return authority, fmt.Errorf("unsupported or incomplete authority identity")
	}
	return authority, nil
}
