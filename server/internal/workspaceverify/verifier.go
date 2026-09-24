// Package workspaceverify verifies immutable SekaiText-Moe web workspace artifacts.
package workspaceverify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	ManifestFilename = "web-workspace-manifest.json"
	SchemaVersion    = 4
	MaxManifestBytes = 1 << 20
	MaxFiles         = 4096
	MaxFileBytes     = 128 << 20
	MaxTotalBytes    = 512 << 20

	ModeDisabled = "disabled"
	ModeExternal = "external"

	// Route.ProducerProof states how a route treats the mutation header. Every
	// proof route rejects a malformed header (400) and a stale header or a
	// running producer (409). Only required routes answer 428 without it;
	// optional routes admit a request without it leniently, which still
	// answers 409 while the producer runs.
	ProducerProofNone     = "none"
	ProducerProofOptional = "optional"
	ProducerProofRequired = "required"

	producerRepository = "https://github.com/SnowGlow-aww/SekaiText-Moe"
	artifactName       = "sekaitext-moe-web-workspace"
	artifactBasePath   = "/workspace/"
	artifactEntrypoint = "index.html"
)

var (
	canonicalSHA40 = regexp.MustCompile(`^[0-9a-f]{40}$`)
	canonicalSHA64 = regexp.MustCompile(`^[0-9a-f]{64}$`)
	appVersion     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)
)

type Config struct {
	Mode                     string
	Root                     string
	ManifestSHA256           string
	Production               bool
	ModeConfigured           bool
	RootConfigured           bool
	ManifestSHA256Configured bool
}

type Manifest struct {
	SchemaVersion      int                `json:"schemaVersion"`
	Artifact           Artifact           `json:"artifact"`
	Producer           Producer           `json:"producer"`
	SourceContract     SourceContract     `json:"sourceContract"`
	EditorGateContract EditorGateContract `json:"editorGateContract"`
	RequiredRoutes     []Route            `json:"requiredRoutes"`
	Files              []File             `json:"files"`
}

type Artifact struct {
	Name       string `json:"name"`
	BasePath   string `json:"basePath"`
	Entrypoint string `json:"entrypoint"`
	AppVersion string `json:"appVersion"`
}

type Producer struct {
	Repository       string `json:"repository"`
	SourceRevision   string `json:"sourceRevision"`
	SourceDirty      bool   `json:"sourceDirty"`
	SourceProduction bool   `json:"sourceProduction"`
}

type SourceContract struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

type EditorGateContract struct {
	Name               string             `json:"name"`
	Version            int                `json:"version"`
	Status             EditorGateStatus   `json:"status"`
	MutationHeader     string             `json:"mutationHeader"`
	MutationFormat     string             `json:"mutationHeaderFormat"`
	MutationRejections MutationRejections `json:"mutationRejections"`
}

type EditorGateStatus struct {
	Method          string   `json:"method"`
	Path            string   `json:"path"`
	ResponseVersion int      `json:"responseVersion"`
	ResponseKeys    []string `json:"responseKeys"`
}

type MutationRejections struct {
	Missing        int `json:"missing"`
	Malformed      int `json:"malformed"`
	StaleOrRunning int `json:"staleOrRunning"`
}

type Route struct {
	Method         string   `json:"method"`
	Path           string   `json:"path"`
	Authentication string   `json:"authentication"`
	ProducerProof  string   `json:"producerProof"`
	AllowedRoles   []string `json:"allowedRoles"`
}

type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

var expectedSourceContract = SourceContract{
	Name: "sekaitext-moe-loaded-producer-state", Version: 2,
}

var expectedEditorGateContract = EditorGateContract{
	Name:    "sekaitext-moe-editor-gate",
	Version: 3,
	Status: EditorGateStatus{
		Method:          "GET",
		Path:            "/api/editor-gate/status",
		ResponseVersion: 1,
		ResponseKeys: []string{
			"completedGeneration", "generation", "instanceId", "lastRun", "revision", "running", "version",
		},
	},
	MutationHeader: "X-Moe-Loaded-Producer-State",
	MutationFormat: "<base64url-instanceId>:<revision>:<completedGeneration>",
	// Missing applies to ProducerProofRequired routes only.
	MutationRejections: MutationRejections{
		Missing: 428, Malformed: 400, StaleOrRunning: 409,
	},
}

var (
	editorRoles = []string{"editor", "admin"}
	adminRoles  = []string{"admin"}
	noRoles     = []string{}
)

var requiredRoutes = []Route{
	{Method: "GET", Path: "/api/admin/lyrics-source-reviews", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: adminRoles},
	{Method: "GET", Path: "/api/admin/lyrics-source-reviews/detail", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: adminRoles},
	{Method: "GET", Path: "/api/auth/me", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/backup/status", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/catalog/characters", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/catalog/music", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/categories", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/category/snapshot", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/editor-gate/status", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/entries", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/event-associations", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/event-stories", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/event-story", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/event-story/episode-snapshot", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/lyrics", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/lyrics/detail", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/api/lyrics/source/search", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: adminRoles},
	{Method: "GET", Path: "/api/projection/status", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "GET", Path: "/sse", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "POST", Path: "/api/admin/lyrics-source-reviews/import", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: adminRoles},
	{Method: "POST", Path: "/api/auth/login", Authentication: "none", ProducerProof: ProducerProofNone, AllowedRoles: noRoles},
	{Method: "POST", Path: "/api/auth/refresh", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: editorRoles},
	{Method: "POST", Path: "/api/editor/v1/backup/push", Authentication: "bearer", ProducerProof: ProducerProofRequired, AllowedRoles: adminRoles},
	{Method: "POST", Path: "/api/editor/v1/lyrics/publish", Authentication: "bearer", ProducerProof: ProducerProofOptional, AllowedRoles: adminRoles},
	{Method: "POST", Path: "/api/editor/v1/lyrics/unpublish", Authentication: "bearer", ProducerProof: ProducerProofOptional, AllowedRoles: adminRoles},
	{Method: "POST", Path: "/api/lyrics/source/preview", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: adminRoles},
	{Method: "PUT", Path: "/api/admin/lyrics-source-reviews/candidate-selection", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: adminRoles},
	{Method: "PUT", Path: "/api/admin/lyrics-source-reviews/decision", Authentication: "bearer", ProducerProof: ProducerProofNone, AllowedRoles: adminRoles},
	{Method: "PUT", Path: "/api/editor/v1/category/batch", Authentication: "bearer", ProducerProof: ProducerProofOptional, AllowedRoles: editorRoles},
	{Method: "PUT", Path: "/api/editor/v1/entry", Authentication: "bearer", ProducerProof: ProducerProofOptional, AllowedRoles: editorRoles},
	{Method: "PUT", Path: "/api/editor/v1/event-story/update", Authentication: "bearer", ProducerProof: ProducerProofOptional, AllowedRoles: editorRoles},
	{Method: "PUT", Path: "/api/editor/v1/lyrics/save", Authentication: "bearer", ProducerProof: ProducerProofOptional, AllowedRoles: editorRoles},
}

// RequiredRoutes returns a copy of the server-owned direct-client capability contract.
func RequiredRoutes() []Route {
	routes := make([]Route, len(requiredRoutes))
	for index, route := range requiredRoutes {
		routes[index] = route
		routes[index].AllowedRoles = make([]string, len(route.AllowedRoles))
		copy(routes[index].AllowedRoles, route.AllowedRoles)
	}
	return routes
}

// Verify checks configuration, the externally locked manifest bytes, the exact
// producer/server contract, and every file in the closed-world inventory. It
// retains the mode-less external configuration used by paired artifact tooling.
func Verify(config Config) (*Manifest, error) {
	return verify(config, false)
}

// VerifyRuntime applies the server-runtime workspace policy. Production
// requires an explicitly disabled workspace; nonproduction runtime may omit the
// workspace entirely or explicitly disable it. External schema-v4 artifacts
// remain available only to verifier tooling through Verify, never to a running
// server process.
func VerifyRuntime(config Config) (*Manifest, error) {
	mode := config.Mode
	if config.ModeConfigured && mode != ModeDisabled && mode != ModeExternal {
		return nil, errors.New(`WORKSPACE_MODE must be exactly "disabled" or "external"`)
	}
	if config.Production {
		return verify(config, true)
	}
	if mode == ModeDisabled {
		return verify(config, false)
	}
	if mode == ModeExternal || config.ModeConfigured || config.RootConfigured || config.ManifestSHA256Configured || strings.TrimSpace(config.Root) != "" || strings.TrimSpace(config.ManifestSHA256) != "" {
		return nil, errors.New("external workspace is available only to verifier tooling")
	}
	return nil, nil
}

func verify(config Config, requireStandaloneProduction bool) (*Manifest, error) {
	mode := config.Mode
	root := strings.TrimSpace(config.Root)
	manifestDigest := strings.TrimSpace(config.ManifestSHA256)
	modeConfigured := config.ModeConfigured || mode != ""
	rootConfigured := config.RootConfigured || root != ""
	digestConfigured := config.ManifestSHA256Configured || manifestDigest != ""

	if requireStandaloneProduction {
		if !modeConfigured {
			return nil, errors.New(`WORKSPACE_MODE is required in production and must be exactly "disabled"`)
		}
		if mode != ModeDisabled {
			return nil, errors.New(`WORKSPACE_MODE must be exactly "disabled" in production`)
		}
	}
	if modeConfigured && mode != ModeDisabled && mode != ModeExternal {
		return nil, errors.New(`WORKSPACE_MODE must be exactly "disabled" or "external"`)
	}
	if mode == ModeDisabled {
		if rootConfigured || digestConfigured {
			return nil, errors.New("WORKSPACE_WEB_DIR and WORKSPACE_MANIFEST_SHA256 must both be unset when WORKSPACE_MODE=disabled")
		}
		return nil, nil
	}
	if mode == ModeExternal && (!rootConfigured || !digestConfigured) {
		return nil, errors.New("WORKSPACE_WEB_DIR and WORKSPACE_MANIFEST_SHA256 are required when WORKSPACE_MODE=external")
	}
	if !rootConfigured && !digestConfigured {
		if config.Production {
			return nil, errors.New("WORKSPACE_WEB_DIR and WORKSPACE_MANIFEST_SHA256 are required in production")
		}
		return nil, nil
	}
	if !rootConfigured || !digestConfigured {
		return nil, errors.New("WORKSPACE_WEB_DIR and WORKSPACE_MANIFEST_SHA256 must be configured together")
	}
	if root == "" {
		return nil, errors.New("WORKSPACE_WEB_DIR must be a non-empty path")
	}
	if !canonicalSHA64.MatchString(manifestDigest) {
		return nil, errors.New("WORKSPACE_MANIFEST_SHA256 must be 64 lowercase hexadecimal characters")
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, fmt.Errorf("inspect workspace root: %w", err)
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return nil, errors.New("workspace root must be a non-symlink directory")
	}
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open workspace root: %w", err)
	}
	defer rootHandle.Close()

	manifestBytes, err := readManifest(rootHandle)
	if err != nil {
		return nil, err
	}
	actualManifestDigest := sha256.Sum256(manifestBytes)
	if hex.EncodeToString(actualManifestDigest[:]) != manifestDigest {
		return nil, errors.New("workspace manifest SHA-256 does not match WORKSPACE_MANIFEST_SHA256")
	}
	if err := rejectDuplicateJSONKeys(manifestBytes); err != nil {
		return nil, fmt.Errorf("workspace manifest JSON: %w", err)
	}
	if err := validateJSONShape(manifestBytes); err != nil {
		return nil, fmt.Errorf("workspace manifest schema: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode workspace manifest: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := validateManifest(&manifest, config.Production); err != nil {
		return nil, err
	}
	if err := verifyInventory(root, rootHandle, manifest.Files); err != nil {
		return nil, err
	}
	return &manifest, nil
}

func readManifest(root *os.Root) ([]byte, error) {
	file, err := root.Open(ManifestFilename)
	if err != nil {
		return nil, fmt.Errorf("open workspace manifest: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat workspace manifest: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("workspace manifest must be a regular non-symlink file")
	}
	if info.Size() > MaxManifestBytes {
		return nil, fmt.Errorf("workspace manifest exceeds %d bytes", MaxManifestBytes)
	}
	contents, err := io.ReadAll(io.LimitReader(file, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read workspace manifest: %w", err)
	}
	if len(contents) > MaxManifestBytes {
		return nil, fmt.Errorf("workspace manifest exceeds %d bytes", MaxManifestBytes)
	}
	return contents, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("workspace manifest contains more than one JSON value")
		}
		return fmt.Errorf("workspace manifest trailing JSON: %w", err)
	}
	return nil
}

func rejectDuplicateJSONKeys(contents []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.UseNumber()
	if err := scanJSONValue(decoder); err != nil {
		return err
	}
	if token, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing token %v", token)
		}
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("unterminated JSON object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("unterminated JSON array")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func validateJSONShape(contents []byte) error {
	root, err := exactObject(contents, "manifest", []string{
		"schemaVersion", "artifact", "producer", "sourceContract", "editorGateContract", "requiredRoutes", "files",
	})
	if err != nil {
		return err
	}
	if _, err := exactObject(root["artifact"], "artifact", []string{"name", "basePath", "entrypoint", "appVersion"}); err != nil {
		return err
	}
	if _, err := exactObject(root["producer"], "producer", []string{"repository", "sourceRevision", "sourceDirty", "sourceProduction"}); err != nil {
		return err
	}
	if _, err := exactObject(root["sourceContract"], "sourceContract", []string{"name", "version"}); err != nil {
		return err
	}
	editorGate, err := exactObject(root["editorGateContract"], "editorGateContract", []string{
		"name", "version", "status", "mutationHeader", "mutationHeaderFormat", "mutationRejections",
	})
	if err != nil {
		return err
	}
	if _, err := exactObject(editorGate["status"], "editorGateContract.status", []string{"method", "path", "responseVersion", "responseKeys"}); err != nil {
		return err
	}
	if _, err := exactObject(editorGate["mutationRejections"], "editorGateContract.mutationRejections", []string{"missing", "malformed", "staleOrRunning"}); err != nil {
		return err
	}
	var routes []json.RawMessage
	if err := json.Unmarshal(root["requiredRoutes"], &routes); err != nil {
		return errors.New("requiredRoutes must be an array")
	}
	for index, route := range routes {
		if _, err := exactObject(route, fmt.Sprintf("requiredRoutes[%d]", index), []string{"method", "path", "authentication", "producerProof", "allowedRoles"}); err != nil {
			return err
		}
	}
	var files []json.RawMessage
	if err := json.Unmarshal(root["files"], &files); err != nil {
		return errors.New("files must be an array")
	}
	for index, file := range files {
		if _, err := exactObject(file, fmt.Sprintf("files[%d]", index), []string{"path", "size", "sha256"}); err != nil {
			return err
		}
	}
	return nil
}

func exactObject(contents []byte, name string, expected []string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(contents, &object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be an object", name)
	}
	if len(object) != len(expected) {
		return nil, fmt.Errorf("%s must contain exactly %s", name, strings.Join(expected, ", "))
	}
	for _, key := range expected {
		if _, ok := object[key]; !ok {
			return nil, fmt.Errorf("%s is missing exact field %q", name, key)
		}
	}
	return object, nil
}

func validateManifest(manifest *Manifest, production bool) error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported workspace schemaVersion %d", manifest.SchemaVersion)
	}
	if manifest.Artifact.Name != artifactName || manifest.Artifact.BasePath != artifactBasePath || manifest.Artifact.Entrypoint != artifactEntrypoint || !appVersion.MatchString(manifest.Artifact.AppVersion) {
		return errors.New("workspace artifact identity is unsupported")
	}
	if manifest.Producer.Repository != producerRepository || !canonicalSHA40.MatchString(manifest.Producer.SourceRevision) {
		return errors.New("workspace producer identity is unsupported")
	}
	if production && (manifest.Producer.SourceDirty || !manifest.Producer.SourceProduction) {
		return errors.New("production requires sourceDirty=false and sourceProduction=true")
	}
	if !reflect.DeepEqual(manifest.SourceContract, expectedSourceContract) {
		return errors.New("workspace source contract is unsupported")
	}
	if !reflect.DeepEqual(manifest.EditorGateContract, expectedEditorGateContract) {
		return errors.New("workspace editor gate contract is unsupported")
	}
	if err := validateRoutes(manifest.RequiredRoutes); err != nil {
		return err
	}
	if !reflect.DeepEqual(manifest.RequiredRoutes, requiredRoutes) {
		return errors.New("workspace required route capability does not match the server contract")
	}
	if err := validateFiles(manifest.Files); err != nil {
		return err
	}
	return nil
}

func validateRoutes(routes []Route) error {
	seen := make(map[string]struct{})
	previous := ""
	for index, route := range routes {
		if !oneOf(route.Method, "GET", "POST", "PUT", "DELETE") || route.Path == "" || route.Path[0] != '/' ||
			!oneOf(route.Authentication, "none", "bearer") ||
			!oneOf(strings.Join(route.AllowedRoles, ","), "", "editor,admin", "admin") ||
			(route.Authentication == "none") != (len(route.AllowedRoles) == 0) ||
			!oneOf(route.ProducerProof, ProducerProofNone, ProducerProofOptional, ProducerProofRequired) ||
			(route.ProducerProof != ProducerProofNone && route.Authentication != "bearer") {
			return fmt.Errorf("required route %d is invalid", index)
		}
		identity := route.Method + "\x00" + route.Path
		if _, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("duplicate required route %s %s", route.Method, route.Path)
		}
		key := identity + "\x00" + route.Authentication + "\x00" + route.ProducerProof + "\x00" + strings.Join(route.AllowedRoles, ",")
		if index > 0 && key <= previous {
			return errors.New("required routes are not in canonical authorization order")
		}
		seen[identity] = struct{}{}
		previous = key
	}
	return nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func validateFiles(files []File) error {
	if len(files) == 0 || len(files) > MaxFiles {
		return fmt.Errorf("workspace inventory file count must be between 1 and %d", MaxFiles)
	}
	var total int64
	previous := ""
	entrypointFound := false
	for index, file := range files {
		if !validInventoryPath(file.Path) || file.Path == ManifestFilename {
			return fmt.Errorf("inventory path %q is unsafe", file.Path)
		}
		if index > 0 && file.Path <= previous {
			return errors.New("workspace inventory paths are not sorted and duplicate-free")
		}
		if file.Size < 0 || file.Size > MaxFileBytes {
			return fmt.Errorf("inventory size for %q exceeds file bounds", file.Path)
		}
		if !canonicalSHA64.MatchString(file.SHA256) {
			return fmt.Errorf("inventory SHA-256 for %q is not canonical", file.Path)
		}
		if total > MaxTotalBytes-file.Size {
			return fmt.Errorf("workspace inventory exceeds %d total bytes", MaxTotalBytes)
		}
		total += file.Size
		previous = file.Path
		entrypointFound = entrypointFound || file.Path == artifactEntrypoint
	}
	if !entrypointFound {
		return errors.New("workspace inventory does not contain index.html")
	}
	return nil
}

func validInventoryPath(name string) bool {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func verifyInventory(rootPath string, root *os.Root, files []File) error {
	expected := make(map[string]File, len(files))
	expectedDirectories := make(map[string]struct{})
	for _, file := range files {
		expected[file.Path] = file
		for directory := path.Dir(file.Path); directory != "."; directory = path.Dir(directory) {
			expectedDirectories[directory] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(files))
	actualFiles := 0
	err := filepath.WalkDir(rootPath, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == rootPath {
			return nil
		}
		relative, err := filepath.Rel(rootPath, current)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace contains symlink %q", name)
		}
		if entry.IsDir() {
			if _, ok := expectedDirectories[name]; !ok {
				return fmt.Errorf("workspace contains extra directory %q", name)
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("workspace contains nonregular file %q", name)
		}
		if name == ManifestFilename {
			return nil
		}
		actualFiles++
		if actualFiles > MaxFiles {
			return fmt.Errorf("workspace contains more than %d files", MaxFiles)
		}
		expectedFile, ok := expected[name]
		if !ok {
			return fmt.Errorf("workspace contains extra file %q", name)
		}
		if info.Size() != expectedFile.Size {
			return fmt.Errorf("workspace size mismatch for %q", name)
		}
		file, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("securely open workspace file %q: %w", name, err)
		}
		digest := sha256.New()
		_, copyErr := io.Copy(digest, io.LimitReader(file, MaxFileBytes+1))
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return fmt.Errorf("hash workspace file %q: %w", name, errors.Join(copyErr, closeErr))
		}
		if hex.EncodeToString(digest.Sum(nil)) != expectedFile.SHA256 {
			return fmt.Errorf("workspace SHA-256 mismatch for %q", name)
		}
		seen[name] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("verify workspace inventory: %w", err)
	}
	for _, file := range files {
		if _, ok := seen[file.Path]; !ok {
			return fmt.Errorf("workspace inventory file %q is missing", file.Path)
		}
	}
	return nil
}
