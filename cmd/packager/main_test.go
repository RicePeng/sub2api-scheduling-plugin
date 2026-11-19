package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func fixture(t *testing.T) (manifest, []packageFile, ed25519.PrivateKey) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	m := newManifest(defaultVersion, defaultHost)
	files := []packageFile{
		{name: "ui/index.html", data: []byte("<!doctype html><title>local</title>"), mode: 0o644},
		{name: "ui/assets/app.js", data: []byte("'use strict';"), mode: 0o644},
		{name: "runtimes/linux-amd64/cache-rate-manager", data: []byte("linux runtime fixture"), mode: 0o755},
		{name: "runtimes/windows-amd64/cache-rate-manager.exe", data: []byte("windows runtime fixture"), mode: 0o755},
	}
	m.Runtimes["linux-amd64"] = runtimeEntry{Path: files[2].name}
	m.Runtimes["windows-amd64"] = runtimeEntry{Path: files[3].name}
	for _, file := range files {
		hash := sha256.Sum256(file.data)
		m.Files[file.name] = hex.EncodeToString(hash[:])
	}
	return m, files, key
}

func TestArchiveSignatureHashesAndReproducibility(t *testing.T) {
	m, files, private := fixture(t)
	var first, second bytes.Buffer
	if err := writeArchive(&first, m, files, defaultKeyID, private); err != nil {
		t.Fatal(err)
	}
	// Input enumeration order must not affect manifest or ZIP ordering.
	files[0], files[3] = files[3], files[0]
	if err := writeArchive(&second, m, files, defaultKeyID, private); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("same inputs produced different archive bytes")
	}
	archive, err := zip.NewReader(bytes.NewReader(first.Bytes()), int64(first.Len()))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	for _, entry := range archive.File {
		if _, exists := entries[entry.Name]; exists {
			t.Fatalf("duplicate path %q", entry.Name)
		}
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		entries[entry.Name] = data
		if strings.HasPrefix(entry.Name, "runtimes/linux-") && entry.Mode().Perm()&0o111 == 0 {
			t.Fatalf("Unix runtime is not executable: %s", entry.Name)
		}
		if bytes.Contains(data, []byte("PRIVATE KEY")) {
			t.Fatalf("private key material in %q", entry.Name)
		}
	}
	if len(entries) != len(m.Files)+2 {
		t.Fatalf("got %d entries, want only declared files plus manifest and signature", len(entries))
	}
	var parsed manifest
	if err := json.Unmarshal(entries["manifest.json"], &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.ID != pluginID || parsed.Name != "账号调用管理" || parsed.Requires.Sub2API != ">=0.2.23 <0.3.0" || parsed.Requires.RecommendedSub2APIVersion != defaultHost {
		t.Fatalf("incorrect manifest identity or compatibility: %+v", parsed)
	}
	if parsed.Requires.TestedSub2APIVersions == nil || len(parsed.Requires.TestedSub2APIVersions) != 0 {
		t.Fatal("tested_sub2api_versions must be an explicit empty array")
	}
	if parsed.Requires.PluginProtocol != 1 || parsed.Requires.TransportAPI != 1 || parsed.Requires.UIBridge != 1 || len(parsed.Capabilities) != 1 || parsed.Capabilities[0] != (capability{ID: "openai.oauth.outbound_transport.v1", Platform: "openai", AccountType: "oauth"}) {
		t.Fatal("incorrect capability or protocol declarations")
	}
	for name, expected := range parsed.Files {
		data, exists := entries[name]
		if !exists {
			t.Fatalf("declared file missing: %s", name)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != expected {
			t.Fatalf("hash mismatch for %s", name)
		}
	}
	var signed signature
	if err := json.Unmarshal(entries["signature.json"], &signed); err != nil {
		t.Fatal(err)
	}
	sig, err := base64.StdEncoding.DecodeString(signed.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if signed.Algorithm != "ed25519" || signed.KeyID != defaultKeyID || !ed25519.Verify(private.Public().(ed25519.PublicKey), entries["manifest.json"], sig) {
		t.Fatal("signature does not verify over exact manifest bytes")
	}
}

func TestArchiveRejectsInvalidFileSets(t *testing.T) {
	tests := []struct {
		name string
		edit func(*manifest, *[]packageFile)
		want string
	}{
		{"duplicate", func(_ *manifest, files *[]packageFile) { *files = append(*files, (*files)[0]) }, "duplicate"},
		{"missing", func(_ *manifest, files *[]packageFile) { *files = (*files)[1:] }, "missing"},
		{"undeclared", func(_ *manifest, files *[]packageFile) { *files = append(*files, packageFile{name: "ui/extra.js"}) }, "undeclared"},
		{"wrong hash", func(m *manifest, _ *[]packageFile) { m.Files["ui/index.html"] = strings.Repeat("0", 64) }, "hash mismatch"},
		{"traversal", func(_ *manifest, files *[]packageFile) { (*files)[0].name = "../secret" }, "unsafe"},
		{"reserved", func(_ *manifest, files *[]packageFile) { (*files)[0].name = "signature.json" }, "reserved"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			m, files, key := fixture(t)
			test.edit(&m, &files)
			err := writeArchive(io.Discard, m, files, defaultKeyID, key)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v; want error containing %q", err, test.want)
			}
		})
	}
}

func TestKeygenKeepsPrivateOutsideSourceAndNeverOverwrites(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cache-manager-plugin")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module sub2api.local/cache-manager\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	private := filepath.Join(base, "keys", "publisher.pem")
	public := filepath.Join(base, "keys", "publisher.pub")
	args := []string{"keygen", "-source", root, "-private-key", private, "-public-key", public}
	var output bytes.Buffer
	if err := run(args, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	key, err := readPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(public)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(pub)) != base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)) {
		t.Fatal("public file does not match private key")
	}
	before, err := os.ReadFile(private)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output.Bytes(), before) || strings.Contains(output.String(), "BEGIN PRIVATE KEY") {
		t.Fatal("keygen printed private key material")
	}
	if err := run(args, io.Discard, io.Discard); err == nil {
		t.Fatal("keygen overwrote existing key files")
	}
	after, err := os.ReadFile(private)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("existing private key changed")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(private)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("private key is not mode 0600")
		}
	}
	for _, forbidden := range []string{filepath.Join(root, "key.pem"), filepath.Join(base, "sub2api", "data", "key.pem")} {
		if err := validatePrivateKeyPath(root, forbidden); err == nil {
			t.Fatalf("allowed private key under source or host: %s", forbidden)
		}
	}
	// A pre-existing public file must also prevent creation of a new private key.
	newPrivate := filepath.Join(base, "keys", "new.pem")
	if err := createKeyPair(newPrivate, public, []byte("secret"), []byte("public")); err == nil {
		t.Fatal("existing public key accepted")
	}
	if _, err := os.Stat(newPrivate); !os.IsNotExist(err) {
		t.Fatal("failed key generation left private key behind")
	}
}

func TestUIRequiresEntrypointAndIncludesEveryFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "ui", "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ui", "assets", "app.js"), []byte("app"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectUI(root); err == nil {
		t.Fatal("missing UI entrypoint accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "ui", "index.html"), []byte("index"), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := collectUI(root)
	if err != nil || len(files) != 2 {
		t.Fatalf("got %d UI files, error %v", len(files), err)
	}
}

func TestTargetsRejectDuplicatesAndUnsupportedPlatforms(t *testing.T) {
	for _, targets := range []string{"linux/amd64,linux/amd64", "darwin/arm64", ""} {
		if _, err := parseTargets(targets); err == nil {
			t.Fatalf("accepted invalid targets %q", targets)
		}
	}
	if _, err := parseTargets(defaultTargets); err != nil {
		t.Fatal(err)
	}
}

func TestOutputsCannotReplacePrivateKey(t *testing.T) {
	root := t.TempDir()
	for _, private := range []string{"publisher.s2plugin", "trusted-publisher.yaml"} {
		out := filepath.Join(root, "publisher.s2plugin")
		if err := validateOutputPaths(filepath.Join(root, private), out); err == nil {
			t.Fatalf("allowed output to replace %s", private)
		}
	}
}
