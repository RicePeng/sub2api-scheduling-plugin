// Command packager creates locally signed Sub2API plugin packages.
package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	pluginID       = "local.cache-rate-manager"
	defaultVersion = "0.1.1"
	defaultHost    = "0.2.23"
	defaultKeyID   = "local-cache-rate-manager-v1"
	defaultTargets = "linux/amd64,linux/arm64,windows/amd64"
)

var (
	versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)
	keyIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,159}$`)
)

type manifest struct {
	SchemaVersion int                     `json:"schema_version"`
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Version       string                  `json:"version"`
	Description   string                  `json:"description"`
	Requires      requirements            `json:"requires"`
	Capabilities  []capability            `json:"capabilities"`
	Runtimes      map[string]runtimeEntry `json:"runtimes"`
	UI            uiEntry                 `json:"ui"`
	Files         map[string]string       `json:"files"`
}

type requirements struct {
	Sub2API                   string   `json:"sub2api"`
	RecommendedSub2APIVersion string   `json:"recommended_sub2api_version"`
	TestedSub2APIVersions     []string `json:"tested_sub2api_versions"`
	PluginProtocol            int      `json:"plugin_protocol"`
	TransportAPI              int      `json:"transport_api"`
	UIBridge                  int      `json:"ui_bridge"`
}

type capability struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	AccountType string `json:"account_type"`
}

type runtimeEntry struct {
	Path string `json:"path"`
}

type uiEntry struct {
	Entrypoint string `json:"entrypoint"`
}

type signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

type packageFile struct {
	name string
	data []byte
	mode os.FileMode
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "packager:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: packager <keygen|build> [flags]; use <command> -h for help")
	}
	switch args[0] {
	case "keygen":
		return runKeygen(args[1:], stdout, stderr)
	case "build":
		return runBuild(args[1:], stdout, stderr)
	case "-h", "--help", "help":
		fmt.Fprintln(stdout, "Usage: packager <keygen|build> [flags]\n  keygen: create an Ed25519 publisher key pair outside the source and host trees\n  build: cross-compile, hash, and sign a .s2plugin archive\nRun packager <command> -h for command flags.")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runKeygen(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	flags.SetOutput(stderr)
	privatePath := flags.String("private-key", "", "new PKCS#8 PEM private key path (outside source and host trees)")
	publicPath := flags.String("public-key", "", "new Base64 Ed25519 public key path")
	source := flags.String("source", ".", "plugin source directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *privatePath == "" || *publicPath == "" {
		return errors.New("keygen requires -private-key PATH and -public-key PATH, with no positional arguments")
	}
	root, err := sourceRoot(*source)
	if err != nil {
		return err
	}
	if err := validatePrivateKeyPath(root, *privatePath); err != nil {
		return err
	}
	private, err := canonicalPath(*privatePath)
	if err != nil {
		return err
	}
	public, err := canonicalPath(*publicPath)
	if err != nil {
		return err
	}
	if samePath(private, public) {
		return errors.New("private and public key paths must differ")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("generate Ed25519 key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return fmt.Errorf("encode private key: %w", err)
	}
	privateBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	publicBytes := []byte(base64.StdEncoding.EncodeToString(pub) + "\n")
	if err := createKeyPair(private, public, privateBytes, publicBytes); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Created publisher key files. Keep the private key on the build machine only.\nPublic key: %s\n", public)
	return nil
}

func createKeyPair(privatePath, publicPath string, privateBytes, publicBytes []byte) error {
	for _, name := range []string{privatePath, publicPath} {
		if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
			return fmt.Errorf("create key directory: %w", err)
		}
	}
	private, err := os.OpenFile(privatePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private key (existing files are never overwritten): %w", err)
	}
	complete := false
	defer func() {
		_ = private.Close()
		if !complete {
			_ = os.Remove(privatePath)
		}
	}()
	public, err := os.OpenFile(publicPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create public key (existing files are never overwritten): %w", err)
	}
	defer func() {
		_ = public.Close()
		if !complete {
			_ = os.Remove(publicPath)
		}
	}()
	if _, err := private.Write(privateBytes); err != nil {
		return fmt.Errorf("write private key: %w", err)
	}
	if err := private.Sync(); err != nil {
		return fmt.Errorf("sync private key: %w", err)
	}
	if err := private.Close(); err != nil {
		return fmt.Errorf("close private key: %w", err)
	}
	if _, err := public.Write(publicBytes); err != nil {
		return fmt.Errorf("write public key: %w", err)
	}
	if err := public.Sync(); err != nil {
		return fmt.Errorf("sync public key: %w", err)
	}
	if err := public.Close(); err != nil {
		return fmt.Errorf("close public key: %w", err)
	}
	complete = true
	return nil
}

func runBuild(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	privatePath := flags.String("private-key", "", "PKCS#8 PEM private key path (outside source and host trees)")
	keyID := flags.String("key-id", defaultKeyID, "trusted publisher key ID")
	version := flags.String("version", defaultVersion, "plugin semantic version")
	hostVersion := flags.String("host-version", defaultHost, "recommended Sub2API version (0.2.23 <= version < 0.3.0)")
	out := flags.String("out", "", "output .s2plugin path (default dist/sub2api-scheduling-plugin-VERSION.s2plugin)")
	targets := flags.String("targets", defaultTargets, "comma-separated linux/amd64,linux/arm64,windows/amd64 targets")
	goPath := flags.String("go", "", "Go compiler executable (default: go from PATH)")
	source := flags.String("source", ".", "plugin source directory")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *privatePath == "" {
		return errors.New("build requires -private-key PATH, with no positional arguments")
	}
	if !validVersion(*version) {
		return errors.New("-version must be a semantic version")
	}
	if err := validateHostVersion(*hostVersion); err != nil {
		return err
	}
	if !keyIDPattern.MatchString(*keyID) || *keyID == "sub2api-openai-transport-v1" {
		return errors.New("-key-id must be a local publisher identifier, not the host's official publisher ID")
	}
	root, err := sourceRoot(*source)
	if err != nil {
		return err
	}
	if err := validatePrivateKeyPath(root, *privatePath); err != nil {
		return err
	}
	private, err := readPrivateKey(*privatePath)
	if err != nil {
		return err
	}
	platforms, err := parseTargets(*targets)
	if err != nil {
		return err
	}
	if *goPath == "" {
		*goPath, err = exec.LookPath("go")
		if err != nil {
			return errors.New("Go compiler not found; set -go to the compiler executable")
		}
	} else {
		*goPath, err = exec.LookPath(*goPath)
		if err != nil {
			return fmt.Errorf("find Go compiler: %w", err)
		}
	}
	*goPath, err = filepath.Abs(*goPath)
	if err != nil {
		return fmt.Errorf("resolve Go compiler: %w", err)
	}
	if *out == "" {
		*out = filepath.Join(root, "dist", "sub2api-scheduling-plugin-"+*version+".s2plugin")
	}
	if strings.ToLower(filepath.Ext(*out)) != ".s2plugin" {
		return errors.New("-out must have the .s2plugin extension")
	}
	if err := validateOutputPaths(*privatePath, *out); err != nil {
		return err
	}
	files, err := collectUI(root)
	if err != nil {
		return err
	}
	buildDir, err := os.MkdirTemp("", "cache-rate-manager-build-*")
	if err != nil {
		return fmt.Errorf("create temporary build directory: %w", err)
	}
	defer os.RemoveAll(buildDir)
	m := newManifest(*version, *hostVersion)
	for _, target := range platforms {
		parts := strings.Split(target, "/")
		name := "cache-rate-manager"
		if parts[0] == "windows" {
			name += ".exe"
		}
		runtimeKey := strings.ReplaceAll(target, "/", "-")
		binary := filepath.Join(buildDir, runtimeKey, name)
		if err := os.MkdirAll(filepath.Dir(binary), 0o700); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Building %s\n", target)
		command := exec.Command(*goPath, "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-ldflags=-s -w -buildid= -X main.version="+*version, "-o", binary, "./cmd/plugin")
		command.Dir = root
		command.Env = buildEnvironment(os.Environ(), parts[0], parts[1])
		command.Stdout = stderr
		command.Stderr = stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("build %s: %w", target, err)
		}
		data, err := os.ReadFile(binary)
		if err != nil {
			return fmt.Errorf("read runtime %s: %w", target, err)
		}
		entry := "runtimes/" + runtimeKey + "/" + name
		files = append(files, packageFile{name: entry, data: data, mode: 0o755})
		m.Runtimes[runtimeKey] = runtimeEntry{Path: entry}
	}
	for _, file := range files {
		hash := sha256.Sum256(file.data)
		m.Files[file.name] = hex.EncodeToString(hash[:])
	}
	if err := writeAtomic(*out, 0o644, func(w io.Writer) error {
		return writeArchive(w, m, files, *keyID, private)
	}); err != nil {
		return fmt.Errorf("write package: %w", err)
	}
	public := private.Public().(ed25519.PublicKey)
	trustPath := filepath.Join(filepath.Dir(*out), "trusted-publisher.yaml")
	trust := "# Merge this public-key fragment into the host configuration; review the key first.\nplugins:\n  trusted_publishers:\n    " + strconv.Quote(*keyID) + ": " + strconv.Quote(base64.StdEncoding.EncodeToString(public)) + "\n"
	if err := writeAtomic(trustPath, 0o644, func(w io.Writer) error {
		_, err := io.WriteString(w, trust)
		return err
	}); err != nil {
		return fmt.Errorf("write public trust configuration: %w", err)
	}
	fmt.Fprintf(stdout, "Built %s\nPublic trust configuration: %s\nNo deployed Sub2API version is declared tested; enabling requires accept_untested.\n", *out, trustPath)
	return nil
}

func newManifest(version, hostVersion string) manifest {
	return manifest{
		SchemaVersion: 1,
		ID:            pluginID,
		Name:          "账号调用管理",
		Version:       version,
		Description:   "API Key 账号缓存与倍率监控、分组调度及安全恢复。",
		Requires: requirements{
			Sub2API: ">=0.2.23 <0.3.0", RecommendedSub2APIVersion: hostVersion,
			TestedSub2APIVersions: []string{}, PluginProtocol: 1, TransportAPI: 1, UIBridge: 1,
		},
		Capabilities: []capability{{ID: "openai.oauth.outbound_transport.v1", Platform: "openai", AccountType: "oauth"}},
		Runtimes:     map[string]runtimeEntry{},
		UI:           uiEntry{Entrypoint: "ui/index.html"},
		Files:        map[string]string{},
	}
}

func parseTargets(raw string) ([]string, error) {
	seen := map[string]bool{}
	var targets []string
	for _, target := range strings.Split(raw, ",") {
		target = strings.TrimSpace(target)
		if target != "linux/amd64" && target != "linux/arm64" && target != "windows/amd64" {
			return nil, fmt.Errorf("unsupported target %q", target)
		}
		if seen[target] {
			return nil, fmt.Errorf("duplicate target %q", target)
		}
		seen[target] = true
		targets = append(targets, target)
	}
	sort.Strings(targets)
	return targets, nil
}

func validateHostVersion(version string) error {
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 || parts[0] != "0" || parts[1] != "2" {
		return errors.New("-host-version must be a stable version >=0.2.23 <0.3.0")
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil || patch < 23 || strconv.Itoa(patch) != parts[2] {
		return errors.New("-host-version must be a stable version >=0.2.23 <0.3.0")
	}
	return nil
}

func validVersion(version string) bool {
	if !versionPattern.MatchString(version) {
		return false
	}
	withoutBuild, _, _ := strings.Cut(version, "+")
	_, prerelease, _ := strings.Cut(withoutBuild, "-")
	for _, part := range strings.Split(prerelease, ".") {
		if len(part) < 2 || part[0] != '0' {
			continue
		}
		numeric := true
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				numeric = false
				break
			}
		}
		if numeric {
			return false
		}
	}
	return true
}

func buildEnvironment(base []string, goos, goarch string) []string {
	values := map[string]string{"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0", "GOWORK": "off"}
	var env []string
	for _, item := range base {
		name, _, _ := strings.Cut(item, "=")
		if _, replaced := values[strings.ToUpper(name)]; !replaced {
			env = append(env, item)
		}
	}
	for _, name := range []string{"GOOS", "GOARCH", "CGO_ENABLED", "GOWORK"} {
		env = append(env, name+"="+values[name])
	}
	return env
}

func collectUI(root string) ([]packageFile, error) {
	uiRoot := filepath.Join(root, "ui")
	info, err := os.Lstat(uiRoot)
	if err != nil || !info.IsDir() {
		return nil, errors.New("ui must be a real directory containing index.html")
	}
	var files []packageFile
	err = filepath.WalkDir(uiRoot, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("UI entries must be regular files, not links or devices: %s", name)
		}
		rel, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		files = append(files, packageFile{name: filepath.ToSlash(rel), data: data, mode: 0o644})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collect UI: %w", err)
	}
	foundIndex := false
	for _, file := range files {
		if file.name == "ui/index.html" {
			foundIndex = true
		}
	}
	if !foundIndex {
		return nil, errors.New("UI entrypoint ui/index.html is missing")
	}
	return files, nil
}

func writeArchive(w io.Writer, m manifest, files []packageFile, keyID string, private ed25519.PrivateKey) error {
	if len(private) != ed25519.PrivateKeySize {
		return errors.New("invalid Ed25519 private key")
	}
	seen := map[string]bool{}
	for _, file := range files {
		if !safeArchivePath(file.name) || file.name == "manifest.json" || file.name == "signature.json" {
			return fmt.Errorf("unsafe or reserved archive path %q", file.name)
		}
		if seen[file.name] {
			return fmt.Errorf("duplicate archive path %q", file.name)
		}
		seen[file.name] = true
		expected, declared := m.Files[file.name]
		if !declared {
			return fmt.Errorf("undeclared archive file %q", file.name)
		}
		hash := sha256.Sum256(file.data)
		if hex.EncodeToString(hash[:]) != expected {
			return fmt.Errorf("hash mismatch for %q", file.name)
		}
	}
	for name := range m.Files {
		if !seen[name] {
			return fmt.Errorf("missing declared file %q", name)
		}
	}
	if !seen[m.UI.Entrypoint] {
		return errors.New("missing UI entrypoint")
	}
	for _, entry := range m.Runtimes {
		if !seen[entry.Path] {
			return fmt.Errorf("missing runtime %q", entry.Path)
		}
	}
	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifestBytes = append(manifestBytes, '\n')
	sig := signature{Algorithm: "ed25519", KeyID: keyID, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifestBytes))}
	signatureBytes, err := json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return err
	}
	signatureBytes = append(signatureBytes, '\n')
	entries := append([]packageFile{}, files...)
	entries = append(entries, packageFile{name: "manifest.json", data: manifestBytes, mode: 0o644}, packageFile{name: "signature.json", data: signatureBytes, mode: 0o644})
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	archive := zip.NewWriter(w)
	for _, file := range entries {
		header := &zip.FileHeader{Name: file.name, Method: zip.Deflate}
		header.SetMode(file.mode)
		header.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
		entry, err := archive.CreateHeader(header)
		if err != nil {
			_ = archive.Close()
			return err
		}
		if _, err := entry.Write(file.data); err != nil {
			_ = archive.Close()
			return err
		}
	}
	return archive.Close()
}

func safeArchivePath(name string) bool {
	return name != "" && name == strings.TrimSpace(name) && !strings.HasPrefix(name, "/") && !strings.ContainsAny(name, "\\\x00:") && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../")
}

func sourceRoot(source string) (string, error) {
	root, err := canonicalPath(source)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.Contains(string(data), "module sub2api.local/cache-manager") {
		return "", errors.New("-source must point to the cache-manager-plugin directory containing go.mod")
	}
	return root, nil
}

func validatePrivateKeyPath(root, name string) error {
	key, err := canonicalPath(name)
	if err != nil {
		return err
	}
	// The sibling host is never an appropriate key store, including its data/plugins runtime.
	for _, blocked := range []string{root, filepath.Join(filepath.Dir(root), "sub2api")} {
		blocked, err = canonicalPath(blocked)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(blocked, key)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return errors.New("private key must stay outside the plugin source and sibling Sub2API host trees; use a separate secure key directory")
		}
	}
	return nil
}

func validateOutputPaths(privatePath, output string) error {
	private, err := canonicalPath(privatePath)
	if err != nil {
		return err
	}
	for _, name := range []string{output, filepath.Join(filepath.Dir(output), "trusted-publisher.yaml")} {
		name, err = canonicalPath(name)
		if err != nil {
			return err
		}
		if samePath(private, name) {
			return errors.New("package outputs must not replace the signing private key")
		}
	}
	return nil
}

func canonicalPath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	current := absolute
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		}
		if !os.IsNotExist(err) || filepath.Dir(current) == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = filepath.Dir(current)
	}
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func readPrivateKey(name string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(name)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("private key must be a regular file readable only by its owner (chmod 600)")
	}
	data, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("private key must contain one PKCS#8 PRIVATE KEY PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid PKCS#8 private key")
	}
	private, ok := key.(ed25519.PrivateKey)
	if !ok || len(private) != ed25519.PrivateKeySize {
		return nil, errors.New("private key must be Ed25519")
	}
	return private, nil
}

func writeAtomic(name string, mode os.FileMode, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(name), ".packager-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err := file.Chmod(mode); err != nil {
		return err
	}
	if err := write(file); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), name)
}
