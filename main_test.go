/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package shimtest_test

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/containerd/typeurl/v2"
	"github.com/opencontainers/runtime-spec/specs-go"

	"github.com/containerd/shimtest"
)

// runConfig is the JSON-driven configuration used by this package's
// own _test.go runner. It embeds the public Config and adds fields
// only meaningful to the local CLI flow (skip list, uid/gid for
// re-exec).
type runConfig struct {
	shimtest.Config

	// Skip is a list of feature names whose suite the local runner
	// should not invoke. Recognized values: "exec", "transfer",
	// "uds", "oom". Library callers achieve the same effect by not
	// constructing the corresponding XxxSuite.
	Skip []string `json:"skip,omitempty"`

	// UID is the user id to run tests as. Defaults to the current
	// user's uid. When the effective uid is 0 (root) and UID is set
	// to a non-zero value, the test binary re-executes itself as
	// that user via sudo.
	UID *int `json:"uid,omitempty"`

	// GID is the group id to run tests as. Defaults to the current
	// user's gid.
	GID *int `json:"gid,omitempty"`
}

// JSON aliases for the embedded Config fields. These match the
// historical JSON profile schema (snake_case keys).
type runConfigJSON struct {
	ShimBinary           string            `json:"shim_binary"`
	Env                  map[string]string `json:"env,omitempty"`
	FormatMounts         bool              `json:"format_mounts,omitempty"`
	Debug                bool              `json:"debug,omitempty"`
	ProvideNetwork       bool              `json:"provide_network,omitempty"`
	ContainerAnnotations map[string]string `json:"container_annotations,omitempty"`

	Skip []string `json:"skip,omitempty"`
	UID  *int     `json:"uid,omitempty"`
	GID  *int     `json:"gid,omitempty"`
}

// UnmarshalJSON populates a runConfig from the historical JSON
// schema. Goes via runConfigJSON so the embedded Config doesn't
// require json:"...,inline" — Go's encoding/json doesn't support
// inline naturally.
func (c *runConfig) UnmarshalJSON(data []byte) error {
	var j runConfigJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return err
	}
	c.Config = shimtest.Config{
		ShimBinary:           j.ShimBinary,
		FormatMounts:         j.FormatMounts,
		Env:                  j.Env,
		Debug:                j.Debug,
		ProvideNetwork:       j.ProvideNetwork,
		ContainerAnnotations: j.ContainerAnnotations,
	}
	c.Skip = j.Skip
	c.UID = j.UID
	c.GID = j.GID
	return nil
}

// MarshalJSON re-serializes a runConfig in the historical schema —
// used when re-execing as a different user (the parent serializes
// the resolved config to a temp file the child reads).
func (c runConfig) MarshalJSON() ([]byte, error) {
	return json.Marshal(runConfigJSON{
		ShimBinary:           c.ShimBinary,
		FormatMounts:         c.FormatMounts,
		Env:                  c.Env,
		Debug:                c.Debug,
		ProvideNetwork:       c.ProvideNetwork,
		ContainerAnnotations: c.ContainerAnnotations,
		Skip:                 c.Skip,
		UID:                  c.UID,
		GID:                  c.GID,
	})
}

var (
	testCfg     runConfig
	configFlag  = flag.String("shimtest.config", "", "path to shimtest JSON configuration file")
	configDir   = flag.String("shimtest.configdir", "", "directory of JSON configuration files (one subtest per file)")
	testConfigs map[string]runConfig
)

// TestMain is the package's entry point. It parses CLI flags, loads
// JSON configurations, and either runs the test suite directly or
// re-executes the binary under sudo when a config requires a
// different uid.
func TestMain(m *testing.M) {
	flag.Parse()

	// Register OCI spec types with typeurl (normally done by
	// containerd client init).
	const prefix = "types.containerd.io"
	major := strconv.Itoa(specs.VersionMajor)
	typeurl.Register(&specs.Process{}, prefix, "opencontainers/runtime-spec", major, "Process")

	testConfigs = discoverConfigs()
	if len(testConfigs) == 0 {
		fmt.Fprintln(os.Stderr, "shimtest: no configuration provided (use -shimtest.config or -shimtest.configdir)")
		os.Exit(1)
	}

	euid := os.Geteuid()
	uid := os.Getuid()
	isReexec := os.Getenv("SHIMTEST_REEXEC") != ""

	var exitCode int

	var reexecConfigs []string
	runnable := false
	for name, cfg := range testConfigs {
		targetUID := uid
		if cfg.UID != nil {
			targetUID = *cfg.UID
		}
		if targetUID == uid {
			runnable = true
		} else if euid == 0 && !isReexec {
			reexecConfigs = append(reexecConfigs, name)
		}
	}

	if runnable {
		exitCode = m.Run()
	}

	for _, name := range reexecConfigs {
		cfg := testConfigs[name]
		targetUID := *cfg.UID
		u, err := user.LookupId(strconv.Itoa(targetUID))
		if err != nil {
			fmt.Fprintf(os.Stderr, "shimtest: cannot find user with uid %d: %v\n", targetUID, err)
			exitCode = 1
			continue
		}
		fmt.Fprintf(os.Stderr, "\n=== RE-EXEC: running %s as %s (uid %d) ===\n\n", name, u.Username, targetUID)
		code := reexecAsUser(u.Username, cfg)
		if code != 0 {
			exitCode = code
		}
	}

	os.Exit(exitCode)
}

// discoverConfigs returns the set of named configs to run.
func discoverConfigs() map[string]runConfig {
	configs := make(map[string]runConfig)

	if *configDir != "" {
		entries, err := os.ReadDir(*configDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shimtest: failed to read config dir %s: %v\n", *configDir, err)
			os.Exit(1)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			name := strings.TrimSuffix(e.Name(), ".json")
			cfg, err := loadConfigFile(filepath.Join(*configDir, e.Name()))
			if err != nil {
				fmt.Fprintf(os.Stderr, "shimtest: %v\n", err)
				os.Exit(1)
			}
			configs[name] = cfg
		}
	}

	if *configFlag != "" {
		cfg, err := loadConfigFile(*configFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "shimtest: %v\n", err)
			os.Exit(1)
		}
		name := strings.TrimSuffix(filepath.Base(*configFlag), ".json")
		configs[name] = cfg
	}

	if len(configs) == 0 {
		if v := os.Getenv("SHIM_BINARY"); v != "" {
			configs["default"] = runConfig{Config: shimtest.Config{ShimBinary: v}}
		}
	}

	return configs
}

// loadConfigFile reads and validates a single config file.
func loadConfigFile(path string) (runConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return runConfig{}, fmt.Errorf("failed to read config %s: %w", path, err)
	}
	var cfg runConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return runConfig{}, fmt.Errorf("failed to parse config %s: %w", path, err)
	}

	if v := os.Getenv("SHIM_BINARY"); v != "" {
		cfg.ShimBinary = v
	}
	if cfg.ShimBinary == "" {
		return runConfig{}, fmt.Errorf("shim_binary is required in %s", path)
	}
	return cfg, nil
}

// activateConfig sets the package-global testCfg and applies env
// vars for the given configuration. Called by the runner before
// dispatching to suites.
func activateConfig(cfg runConfig) {
	testCfg = cfg
	for k, v := range cfg.Env {
		os.Setenv(k, v)
	}
}

// checkRunnable returns an empty string if the config can run in the
// current process, or a reason string if it cannot.
func checkRunnable(cfg runConfig) string {
	uid := os.Getuid()
	targetUID := uid
	if cfg.UID != nil {
		targetUID = *cfg.UID
	}
	if targetUID != uid {
		return fmt.Sprintf("requires uid %d, running as %d", targetUID, uid)
	}
	return ""
}

// featureSkipped reports whether the given feature is in the active
// config's Skip list.
func featureSkipped(feature string) bool {
	for _, s := range testCfg.Skip {
		if s == feature {
			return true
		}
	}
	return false
}

// skipFeature skips the test if the given feature is in the active
// config's Skip list. Used by benchmarks (and the bench-only
// skipFeatureBench alias).
func skipFeature(tb testing.TB, feature string) {
	tb.Helper()
	if featureSkipped(feature) {
		tb.Skipf("feature %q disabled in config", feature)
	}
}

// skipFeatureBench is kept for bench backward-compat.
func skipFeatureBench(b *testing.B, feature string) {
	b.Helper()
	skipFeature(b, feature)
}

// reexecAsUser re-executes the test binary as the given user via
// sudo, passing the full config as a temp file.
func reexecAsUser(username string, cfg runConfig) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "shimtest: cannot determine executable path: %v\n", err)
		return 1
	}

	shimAbs, err := exec.LookPath(cfg.ShimBinary)
	if err != nil {
		shimAbs = cfg.ShimBinary
	}
	shimAbs, _ = filepath.Abs(shimAbs)
	cfg.ShimBinary = shimAbs

	cfgData, err := json.Marshal(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "shimtest: marshal config for reexec: %v\n", err)
		return 1
	}
	cfgFile, err := os.CreateTemp("", "shimtest-config-*.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "shimtest: create temp config: %v\n", err)
		return 1
	}
	cfgFile.Write(cfgData)
	cfgFile.Close()
	os.Chmod(cfgFile.Name(), 0644)
	defer os.Remove(cfgFile.Name())

	childEnv := []string{
		"SHIMTEST_REEXEC=1",
		"PATH=" + filepath.Dir(shimAbs) + ":" + os.Getenv("PATH"),
	}
	for k, v := range cfg.Env {
		childEnv = append(childEnv, k+"="+v)
	}

	sudoArgs := []string{"-Hu", username, "--", "env"}
	sudoArgs = append(sudoArgs, childEnv...)
	sudoArgs = append(sudoArgs, exe)
	sudoArgs = append(sudoArgs, "-shimtest.config="+cfgFile.Name())

	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "-shimtest.config=") || strings.HasPrefix(arg, "--shimtest.config=") {
			continue
		}
		if strings.HasPrefix(arg, "-shimtest.configdir=") || strings.HasPrefix(arg, "--shimtest.configdir=") {
			continue
		}
		if arg == "-shimtest.config" || arg == "--shimtest.config" ||
			arg == "-shimtest.configdir" || arg == "--shimtest.configdir" {
			i++
			continue
		}
		sudoArgs = append(sudoArgs, arg)
	}

	cmd := exec.Command("sudo", sudoArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "shimtest: reexec as %s failed: %v\n", username, err)
		return 1
	}
	return 0
}
