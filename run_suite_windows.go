//go:build windows

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

package shimtest

import (
	"os"
	"path/filepath"
	"testing"

	taskAPI "github.com/containerd/containerd/api/runtime/task/v3"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/ttrpc"
	"golang.org/x/sys/windows"
)

func (s *RunSuite) registerPlatformRunTests(t *testing.T) {
	t.Run("DeleteCleansRootfsOnCrash", s.testDeleteCleansRootfsOnCrash)
}

// testDeleteCleansRootfsOnCrash verifies that the shim's `delete` subcommand
// removes bundle/rootfs/ even when the shim process died before the in-process
// Delete RPC cleanup could run. On Windows, a leftover rootfs/ causes
// bindfilter.BfRemoveMapping to return ERROR_ACCESS_DENIED, blocking bundle
// deletion on the next daemon start.
func (s *RunSuite) testDeleteCleansRootfsOnCrash(t *testing.T) {
	shimBin, bundleDir, rootfsMounts := shimSetup(t, s.cfg)

	cid := containerID(t)
	createOCISpec(t, bundleDir, []string{"/bin/forever"}, s.cfg)

	stdoutPath, stderrPath := createIOFifos(t, bundleDir)
	ns := uniqueTestNamespace(t, "run")
	ctx := namespaces.WithNamespace(t.Context(), ns)

	params := startShim(t, shimBin, bundleDir, cid, ns, s.cfg)

	conn := connectShim(t, params.Address)
	client := ttrpc.NewClient(conn)
	defer client.Close()

	tc := taskAPI.NewTTRPCTaskClient(client)

	drainFifo(t, ctx, stdoutPath)
	drainFifo(t, ctx, stderrPath)

	if _, err := tc.Create(ctx, newCreateTaskRequest(t, cid, bundleDir, stdoutPath, stderrPath, rootfsMounts)); err != nil {
		t.Fatal("create failed:", err)
	}
	if _, err := tc.Start(ctx, &taskAPI.StartRequest{ID: cid}); err != nil {
		t.Fatal("start failed:", err)
	}

	pidData, err := os.ReadFile(filepath.Join(bundleDir, "shim.pid"))
	if err != nil {
		t.Fatal("failed to read shim.pid:", err)
	}
	shimPID, err := parseIntBytes(pidData)
	if err != nil || shimPID == 0 {
		t.Fatalf("failed to parse shim pid from %q: %v", string(pidData), err)
	}

	// Kill directly so the in-process Delete RPC cleanup never runs, leaving rootfs/ on disk.
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(shimPID))
	if err != nil {
		t.Fatalf("OpenProcess(%d): %v", shimPID, err)
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		windows.CloseHandle(h)
		t.Fatalf("TerminateProcess(%d): %v", shimPID, err)
	}
	windows.WaitForSingleObject(h, windows.INFINITE)
	windows.CloseHandle(h)

	// If rootfs/ is already gone the kill came too late and the orderly cleanup
	// ran, so the test would not exercise the crash path.
	rootfsDir := filepath.Join(bundleDir, "rootfs")
	if _, err := os.Stat(rootfsDir); os.IsNotExist(err) {
		t.Fatal("pre-condition failed: rootfs/ does not exist after shim crash; the test did not exercise the crash path")
	}

	// mirrors what containerd does when it detects a dead shim
	deleteShim(t, shimBin, bundleDir, cid, ns, s.cfg)

	if _, err := os.Stat(rootfsDir); !os.IsNotExist(err) {
		t.Errorf("rootfs/ still exists after shim delete subcommand; "+
			"containerd's bundle cleanup will fail with ERROR_ACCESS_DENIED "+
			"(bindfilter.BfRemoveMapping on a non-bind-filter directory): %v", err)
	}
}
