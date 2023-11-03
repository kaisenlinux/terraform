// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package cloudplugin

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func assertResolvedBinary(t *testing.T, binary *Binary, assertCached bool) {
	t.Helper()

	if binary == nil {
		t.Fatal("expected non-nil binary")
	}

	if binary.ResolvedFromCache != assertCached {
		t.Errorf("expected ResolvedFromCache to be %v, got %v", assertCached, binary.ResolvedFromCache)
	}

	info, err := os.Stat(binary.Path)
	if err != nil {
		t.Fatalf("expected no error when getting binary location, got %q", err)
	}

	if info.IsDir() || info.Size() == 0 {
		t.Fatalf("expected non-zero file at %q", binary.Path)
	}

	if binary.ProductVersion != "0.1.0" { // from sample manifest
		t.Errorf("expected product binary %q, got %q", "0.1.0", binary.ProductVersion)
	}
}

func TestBinaryManager_Resolve(t *testing.T) {
	publicKey, err := os.ReadFile("testdata/sample.public.key")
	if err != nil {
		t.Fatal(err)
	}

	server, err := newCloudPluginManifestHTTPTestServer(t)
	if err != nil {
		t.Fatalf("could not create test server: %s", err)
	}
	defer server.Close()

	serverURL, _ := url.Parse(server.URL)
	serviceURL := serverURL.JoinPath("/api/cloudplugin/v1")

	tempDir := t.TempDir()
	manager, err := NewBinaryManager(context.Background(), tempDir, serviceURL, "darwin", "amd64")
	if err != nil {
		t.Fatalf("expected no err, got: %s", err)
	}
	manager.signingKey = string(publicKey)
	manager.binaryName = "toucan.txt" // The file contained in the test archive

	binary, err := manager.Resolve()
	if err != nil {
		t.Fatalf("expected no err, got %s", err)
	}

	assertResolvedBinary(t, binary, false)

	// Resolving a second time should return a cached binary
	binary, err = manager.Resolve()
	if err != nil {
		t.Fatalf("expected no err, got %s", err)
	}

	assertResolvedBinary(t, binary, true)

	// Change the local binary data
	err = os.WriteFile(filepath.Join(filepath.Dir(binary.Path), ".version"), []byte("0.0.9"), 0644)
	if err != nil {
		t.Fatalf("could not write to .binary file: %s", err)
	}

	binary, err = manager.Resolve()
	if err != nil {
		t.Fatalf("expected no err, got %s", err)
	}

	assertResolvedBinary(t, binary, false)
}
