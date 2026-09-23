package lyricsextractionplan

import (
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	// RecoverySourceSelectionPolicyV1 remains available for decoding historical
	// fixture manifests. Recovery source preparation and verification use v3.
	RecoverySourceSelectionPolicyV1   = "moesekai-recovery-source-selection-v1"
	RecoverySourceSelectionPolicyV3   = "moesekai-recovery-source-selection-v3"
	RecoverySourceSnapshotAlgorithmV2 = "sha256-ordered-recovery-source-file-identities-v2"

	RecoveryFixtureManifestSchemaVersionV1 = 1
	RecoveryFixtureManifestSchemaVersionV2 = 2
	RecoveryFixtureFormatMediaWikiPageV1   = "mediawiki-api-page-revision-v1"
	RecoveryFixtureFormatRawFileV1         = "raw-reviewed-test-fixture-v1"

	recoveryServerPackageRoot          = "server"
	recoveryCommandPackageRoot         = "server/cmd"
	recoveryInternalPackageRoot        = "server/internal"
	recoveryOfflinePackageRoot         = "server/offline"
	recoveryOfflineCommandPackageRoot  = "server/offline/cmd"
	recoveryOfflineInternalPackageRoot = "server/offline/internal"

	recoveryModulePath        = "server/go.mod"
	recoveryModuleSumPath     = "server/go.sum"
	recoveryOfflineModulePath = "server/offline/go.mod"
	recoveryOfflineModuleSum  = "server/offline/go.sum"
	recoveryOfflineModuleBind = "replace moesekai/server => ../"

	MaxRecoveryFixtureManifestBytes   = 64 << 10
	MaxRecoveryFixtureFiles           = 64
	MaxRecoveryFixtureManifestBytesV2 = 256 << 10
	MaxRecoveryFixtureFilesV2         = 128
	MaxRecoveryFixtureJSONDepth       = 64

	recoveryFixtureManifestPathV1 = "server/offline/internal/lyricsextractionplan/recovery_source_fixtures_v1.json"
	recoveryFixtureManifestPathV2 = "server/offline/internal/lyricsextractionplan/recovery_source_fixtures_v2.json"
)

// RecoverySourceSelectionPolicy is the immutable, routing-free source-layout
// contract shared by recovery snapshot preparation, verification, and fixture
// loading. Package roots describe reviewed build locations only; fixture paths
// and identities remain versioned data in the fixture manifest.
type RecoverySourceSelectionPolicy struct {
	Version                string
	SnapshotAlgorithm      string
	ServerRoot             string
	PackageRoots           []string
	RequiredPaths          []string
	BuildFileExtensions    []string
	FixtureManifestPath    string
	FixtureManifestVersion int
	AllowedFileModes       []uint32
	AllowedDirectoryModes  []uint32
	MaximumFiles           int
	MaximumFileBytes       int64
	MaximumAggregateBytes  int64
}

// CompiledRecoverySourceSelectionPolicy returns a fresh copy of the recovery
// exact-source closure v3 policy, which reviews the production module and the
// nested offline module in one closure.
func CompiledRecoverySourceSelectionPolicy() RecoverySourceSelectionPolicy {
	return RecoverySourceSelectionPolicy{
		Version:           RecoverySourceSelectionPolicyV3,
		SnapshotAlgorithm: RecoverySourceSnapshotAlgorithmV2,
		ServerRoot:        recoveryServerPackageRoot,
		PackageRoots: []string{
			recoveryServerPackageRoot,
			recoveryCommandPackageRoot,
			recoveryInternalPackageRoot,
			recoveryOfflinePackageRoot,
			recoveryOfflineCommandPackageRoot,
			recoveryOfflineInternalPackageRoot,
		},
		RequiredPaths: []string{
			recoveryModulePath,
			recoveryModuleSumPath,
			recoveryOfflineModulePath,
			recoveryOfflineModuleSum,
			recoveryFixtureManifestPathV2,
		},
		BuildFileExtensions: []string{
			".C", ".F", ".S", ".c", ".cc", ".cpp", ".cxx", ".f", ".f90", ".for",
			".go", ".h", ".hh", ".hpp", ".hxx", ".m", ".s", ".swig", ".swigcxx", ".sx", ".syso",
		},
		FixtureManifestPath:    recoveryFixtureManifestPathV2,
		FixtureManifestVersion: RecoveryFixtureManifestSchemaVersionV2,
		AllowedFileModes:       []uint32{0o600, 0o644},
		AllowedDirectoryModes:  []uint32{0o700, 0o750, 0o755},
		MaximumFiles:           MaxSourceSnapshotFiles,
		MaximumFileBytes:       MaxSourceFileBytes,
		MaximumAggregateBytes:  MaxSourceSnapshotBytes,
	}
}

type sourceReadStage uint8

const (
	sourceReadAfterPathInspection sourceReadStage = iota + 1
	sourceReadAfterOpen
	sourceReadAfterFirstChunk
	sourceReadBeforeRevalidation
)

type sourceReadHook func(sourceReadStage, string) error

type sourceStatFingerprint struct {
	Mode      os.FileMode
	Size      int64
	ModTimeNS int64
	UID       uint64
	Nlink     uint64
	Device    uint64
	Inode     uint64
	CTimeSec  int64
	CTimeNSec int64
}

type sourcePathComponent struct {
	Path      string
	Info      os.FileInfo
	Directory bool
}

type sourcePathState struct {
	Path       string
	Components []sourcePathComponent
}

type verifiedSourceFile struct {
	Identity SourceFileIdentity
	Body     []byte
	State    sourcePathState
}

type sourceLayout struct {
	BuildFiles        []string
	FixtureCandidates []string
	Directories       map[string]sourcePathComponent
	Files             map[string]sourcePathComponent
}

type sourceSelection struct {
	Paths       []string
	Fixtures    map[string]RecoveryFixtureIdentityV2
	Directories map[string]sourcePathComponent
	LayoutFiles map[string]sourcePathComponent
}

type sourceTree struct {
	absoluteRoot string
	root         *os.Root
	rootInfo     os.FileInfo
	policy       RecoverySourceSelectionPolicy
}

// PrepareRecoverySourceSnapshot independently derives and hashes the current
// recovery source set under the v2 policy. Legacy plan-v1 snapshot encoding is
// intentionally not reused or modified.
func PrepareRecoverySourceSnapshot(root, capturedAt string) (SourceSnapshot, error) {
	if _, err := parseCanonicalTimestamp(capturedAt); err != nil {
		return SourceSnapshot{}, fmt.Errorf("recovery source snapshot capturedAt: %w", err)
	}
	files, _, _, err := deriveRecoverySourceClosure(root, nil, false)
	if err != nil {
		return SourceSnapshot{}, err
	}
	digest, err := RecoverySourceSnapshotSHA256(files)
	if err != nil {
		return SourceSnapshot{}, err
	}
	return SourceSnapshot{
		Algorithm:  RecoverySourceSnapshotAlgorithmV2,
		CapturedAt: capturedAt,
		Files:      files,
		SHA256:     digest,
	}, nil
}

func deriveRecoverySourceIdentities(root string, hook sourceReadHook) ([]SourceFileIdentity, error) {
	files, _, _, err := deriveRecoverySourceClosure(root, hook, false)
	return files, err
}

func deriveRecoverySourceClosure(
	root string,
	hook sourceReadHook,
	collectFixtures bool,
) ([]SourceFileIdentity, map[string][]byte, []RecoveryFixtureIdentityV2, error) {
	tree, err := openSourceTree(root)
	if err != nil {
		return nil, nil, nil, err
	}
	defer tree.Close()

	selected, err := selectRecoverySourcePaths(tree)
	if err != nil {
		return nil, nil, nil, err
	}
	files := make([]SourceFileIdentity, 0, len(selected.Paths))
	states := make([]sourcePathState, 0, len(selected.Paths))
	opened := make(map[[2]uint64]string, len(selected.Paths))
	fixtureBodies := make(map[string][]byte, len(selected.Fixtures))
	var aggregate int64
	for _, relativePath := range selected.Paths {
		_, fixture := selected.Fixtures[relativePath]
		collect := relativePath == tree.policy.FixtureManifestPath || fixture
		verified, err := tree.hashFile(relativePath, tree.policy.MaximumFileBytes, collect, hook)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("select recovery source %q: %w", relativePath, err)
		}
		if expected, ok := selected.Fixtures[relativePath]; ok {
			if err := validateRecoveryFixtureBodyV2(verified.Body, expected); err != nil {
				return nil, nil, nil, fmt.Errorf("validate recovery fixture %q: %w", relativePath, err)
			}
			if collectFixtures {
				fixtureBodies[relativePath] = bytes.Clone(verified.Body)
			}
		}
		if err := addSourceAggregate(&aggregate, verified.Identity.SizeBytes, tree.policy.MaximumAggregateBytes); err != nil {
			return nil, nil, nil, err
		}
		leaf := verified.State.Components[len(verified.State.Components)-1]
		fingerprint, err := sourceFingerprint(leaf.Info)
		if err != nil {
			return nil, nil, nil, err
		}
		key := [2]uint64{fingerprint.Device, fingerprint.Inode}
		if previous, aliased := opened[key]; aliased {
			return nil, nil, nil, fmt.Errorf("recovery source paths %q and %q alias the same inode", previous, relativePath)
		}
		opened[key] = relativePath
		files = append(files, verified.Identity)
		states = append(states, verified.State)
	}

	current, err := selectRecoverySourcePaths(tree)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("rederive recovery source set: %w", err)
	}
	if err := compareSelectedPaths(selected.Paths, current.Paths); err != nil {
		return nil, nil, nil, fmt.Errorf("recovery source set changed while hashing: %w", err)
	}
	if err := compareSelectedFixtures(selected.Fixtures, current.Fixtures); err != nil {
		return nil, nil, nil, fmt.Errorf("recovery fixture identities changed while hashing: %w", err)
	}
	if err := compareLayoutEntries("directory", selected.Directories, current.Directories); err != nil {
		return nil, nil, nil, fmt.Errorf("recovery source layout changed while hashing: %w", err)
	}
	if err := compareLayoutEntries("file", selected.LayoutFiles, current.LayoutFiles); err != nil {
		return nil, nil, nil, fmt.Errorf("recovery source layout changed while hashing: %w", err)
	}
	for _, state := range states {
		if err := tree.revalidatePathState(state); err != nil {
			return nil, nil, nil, fmt.Errorf("revalidate recovery source %q: %w", state.Path, err)
		}
	}
	if err := tree.revalidateRoot(); err != nil {
		return nil, nil, nil, err
	}

	fixtureIdentities := orderedRecoveryFixtureIdentities(selected.Fixtures)
	return files, fixtureBodies, fixtureIdentities, nil
}

func verifyRecoverySourceSnapshotIdentity(root string, declared SourceSnapshot, hook sourceReadHook) error {
	if _, err := validateRecoverySourceSnapshot(declared); err != nil {
		return err
	}
	current, err := deriveRecoverySourceIdentities(root, hook)
	if err != nil {
		return err
	}
	return compareSourceIdentities(declared.Files, current)
}

func compareSourceIdentities(declared, current []SourceFileIdentity) error {
	left, right := 0, 0
	for left < len(declared) && right < len(current) {
		switch {
		case declared[left].Path < current[right].Path:
			return fmt.Errorf("source snapshot declares missing or ineligible file %q", declared[left].Path)
		case declared[left].Path > current[right].Path:
			return fmt.Errorf("source snapshot omits current eligible file %q", current[right].Path)
		default:
			if declared[left].SizeBytes != current[right].SizeBytes || declared[left].SHA256 != current[right].SHA256 {
				return fmt.Errorf("source snapshot identity mismatch for %q", declared[left].Path)
			}
			left++
			right++
		}
	}
	if left < len(declared) {
		return fmt.Errorf("source snapshot declares missing or ineligible file %q", declared[left].Path)
	}
	if right < len(current) {
		return fmt.Errorf("source snapshot omits current eligible file %q", current[right].Path)
	}
	return nil
}

func compareSelectedPaths(declared, current []string) error {
	left, right := 0, 0
	for left < len(declared) && right < len(current) {
		switch {
		case declared[left] < current[right]:
			return fmt.Errorf("selected file disappeared: %q", declared[left])
		case declared[left] > current[right]:
			return fmt.Errorf("new eligible file appeared: %q", current[right])
		default:
			left++
			right++
		}
	}
	if left < len(declared) {
		return fmt.Errorf("selected file disappeared: %q", declared[left])
	}
	if right < len(current) {
		return fmt.Errorf("new eligible file appeared: %q", current[right])
	}
	return nil
}

func compareSelectedFixtures(declared, current map[string]RecoveryFixtureIdentityV2) error {
	if len(declared) != len(current) {
		return errors.New("fixture identity count changed")
	}
	for _, relativePath := range sortedSourceComponentKeysFromFixtures(declared) {
		currentIdentity, found := current[relativePath]
		if !found || declared[relativePath] != currentIdentity {
			return fmt.Errorf("fixture identity changed for %q", relativePath)
		}
	}
	return nil
}

func compareLayoutEntries(label string, declared, current map[string]sourcePathComponent) error {
	if len(declared) != len(current) {
		return fmt.Errorf("%s entry count changed", label)
	}
	paths := make([]string, 0, len(declared))
	for relativePath := range declared {
		paths = append(paths, relativePath)
	}
	sort.Strings(paths)
	for _, relativePath := range paths {
		actual, found := current[relativePath]
		if !found {
			return fmt.Errorf("%s entry disappeared: %q", label, relativePath)
		}
		if err := sameExactSourceInfo(declared[relativePath].Info, actual.Info); err != nil {
			return fmt.Errorf("%s entry %q changed: %w", label, relativePath, err)
		}
	}
	return nil
}

func selectRecoverySourcePaths(tree *sourceTree) (sourceSelection, error) {
	layout, err := tree.scanSourceLayout()
	if err != nil {
		return sourceSelection{}, err
	}
	selected := make(map[string]struct{}, len(layout.BuildFiles)+len(layout.FixtureCandidates)+len(tree.policy.RequiredPaths))
	for _, required := range tree.policy.RequiredPaths {
		selected[required] = struct{}{}
	}
	for _, buildFile := range layout.BuildFiles {
		selected[buildFile] = struct{}{}
	}

	moduleFile, err := tree.hashFile(recoveryModulePath, tree.policy.MaximumFileBytes, true, nil)
	if err != nil {
		return sourceSelection{}, fmt.Errorf("read recovery module file: %w", err)
	}
	if err := validateRecoveryModuleFile(moduleFile.Body); err != nil {
		return sourceSelection{}, err
	}

	offlineModuleFile, err := tree.hashFile(recoveryOfflineModulePath, tree.policy.MaximumFileBytes, true, nil)
	if err != nil {
		return sourceSelection{}, fmt.Errorf("read offline recovery module file: %w", err)
	}
	if err := validateRecoveryOfflineModuleFile(offlineModuleFile.Body); err != nil {
		return sourceSelection{}, err
	}

	manifestFile, err := tree.hashFile(tree.policy.FixtureManifestPath, MaxRecoveryFixtureManifestBytesV2, true, nil)
	if err != nil {
		return sourceSelection{}, fmt.Errorf("read recovery fixture manifest: %w", err)
	}
	manifest, err := DecodeRecoveryFixtureManifestV2(manifestFile.Body)
	if err != nil {
		return sourceSelection{}, err
	}
	fixtures := make(map[string]RecoveryFixtureIdentityV2, len(manifest.Fixtures))
	fixturePaths := make([]string, 0, len(manifest.Fixtures))
	for _, fixture := range manifest.Fixtures {
		selected[fixture.Path] = struct{}{}
		fixtures[fixture.Path] = fixture
		fixturePaths = append(fixturePaths, fixture.Path)
	}
	if err := compareSelectedPaths(fixturePaths, layout.FixtureCandidates); err != nil {
		return sourceSelection{}, fmt.Errorf("reviewed fixture manifest is not an exact testdata closure: %w", err)
	}

	for _, buildFile := range layout.BuildFiles {
		if path.Ext(buildFile) != ".go" {
			continue
		}
		file, err := tree.hashFile(buildFile, tree.policy.MaximumFileBytes, true, nil)
		if err != nil {
			return sourceSelection{}, fmt.Errorf("read Go source %q for embed directives: %w", buildFile, err)
		}
		patterns, err := goEmbedPatterns(buildFile, file.Body)
		if err != nil {
			return sourceSelection{}, err
		}
		for _, patternValue := range patterns {
			matches, err := tree.expandEmbedPattern(buildFile, patternValue)
			if err != nil {
				return sourceSelection{}, err
			}
			for _, match := range matches {
				selected[match] = struct{}{}
			}
		}
	}

	paths := make([]string, 0, len(selected))
	for relativePath := range selected {
		if !validDataPath(relativePath) {
			return sourceSelection{}, fmt.Errorf("source selection produced noncanonical path %q", relativePath)
		}
		paths = append(paths, relativePath)
	}
	sort.Strings(paths)
	if err := enforceSourceSelectionCount(len(paths), tree.policy.MaximumFiles); err != nil {
		return sourceSelection{}, err
	}
	return sourceSelection{
		Paths: paths, Fixtures: fixtures, Directories: layout.Directories, LayoutFiles: layout.Files,
	}, nil
}

func (tree *sourceTree) scanSourceLayout() (sourceLayout, error) {
	if err := validateRecoverySourceSelectionPolicy(tree.policy); err != nil {
		return sourceLayout{}, err
	}
	if err := rejectRecoveryWorkspaceFiles(tree.absoluteRoot); err != nil {
		return sourceLayout{}, err
	}
	allowedRoots := make(map[string]struct{}, len(tree.policy.PackageRoots))
	for _, packageRoot := range tree.policy.PackageRoots {
		allowedRoots[packageRoot] = struct{}{}
	}
	layout := sourceLayout{
		Directories: make(map[string]sourcePathComponent),
		Files:       make(map[string]sourcePathComponent),
	}
	inodePaths := make(map[[2]uint64]string)
	err := fs.WalkDir(tree.root.FS(), tree.policy.ServerRoot, func(relativePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !validDataPath(relativePath) {
			return fmt.Errorf("source tree contains noncanonical path %q", relativePath)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("source tree contains symlink %q", relativePath)
		}
		if entry.IsDir() {
			if entry.Name() == "vendor" {
				return fmt.Errorf("source tree contains forbidden vendor directory %q", relativePath)
			}
			component, err := tree.captureDirectory(relativePath)
			if err != nil {
				return err
			}
			layout.Directories[relativePath] = component
			return nil
		}

		info, err := tree.lstatExact(relativePath)
		if err != nil {
			return err
		}
		if err := validateSourceLayoutFileInfo(info); err != nil {
			return fmt.Errorf("source layout file %q: %w", relativePath, err)
		}
		fingerprint, err := sourceFingerprint(info)
		if err != nil {
			return err
		}
		key := [2]uint64{fingerprint.Device, fingerprint.Inode}
		if previous, duplicate := inodePaths[key]; duplicate {
			return fmt.Errorf("source layout paths %q and %q alias the same inode", previous, relativePath)
		}
		inodePaths[key] = relativePath
		layout.Files[relativePath] = sourcePathComponent{Path: relativePath, Info: info}

		base := path.Base(relativePath)
		switch base {
		case "go.work", "go.work.sum":
			return fmt.Errorf("source tree contains forbidden workspace file %q", relativePath)
		case "go.mod", "go.sum":
			if relativePath != recoveryServerPackageRoot+"/"+base && relativePath != recoveryOfflinePackageRoot+"/"+base {
				return fmt.Errorf("source tree contains nested module metadata %q", relativePath)
			}
		}

		extension := path.Ext(base)
		if sourceBuildExtensionAllowed(extension, tree.policy.BuildFileExtensions) {
			packageRoot, reviewed := recoveryReviewedPackageRoot(path.Dir(relativePath))
			if !reviewed {
				return fmt.Errorf("source-shaped artifact %q is outside the reviewed package roots", relativePath)
			}
			if _, allowed := allowedRoots[packageRoot]; !allowed {
				return fmt.Errorf("source-shaped artifact %q is outside the reviewed package roots", relativePath)
			}
			layout.BuildFiles = append(layout.BuildFiles, relativePath)
		}
		if packageRoot, fixture := recoveryFixturePackageRoot(relativePath); fixture {
			if _, allowed := allowedRoots[packageRoot]; !allowed {
				return fmt.Errorf("fixture-shaped artifact %q is outside the reviewed package roots", relativePath)
			}
			layout.FixtureCandidates = append(layout.FixtureCandidates, relativePath)
		}
		return nil
	})
	if err != nil {
		return sourceLayout{}, fmt.Errorf("scan recovery source tree: %w", err)
	}
	sort.Strings(layout.BuildFiles)
	sort.Strings(layout.FixtureCandidates)
	return layout, nil
}

func validateRecoverySourceSelectionPolicy(policy RecoverySourceSelectionPolicy) error {
	if policy.Version != RecoverySourceSelectionPolicyV3 || policy.SnapshotAlgorithm != RecoverySourceSnapshotAlgorithmV2 ||
		policy.ServerRoot != "server" || policy.FixtureManifestPath != recoveryFixtureManifestPathV2 ||
		policy.FixtureManifestVersion != RecoveryFixtureManifestSchemaVersionV2 ||
		policy.MaximumFiles <= 0 || policy.MaximumFileBytes <= 0 || policy.MaximumAggregateBytes <= 0 {
		return errors.New("compiled recovery source-selection policy is invalid")
	}
	if len(policy.PackageRoots) == 0 || !sort.StringsAreSorted(policy.PackageRoots) ||
		len(policy.BuildFileExtensions) == 0 || !sort.StringsAreSorted(policy.BuildFileExtensions) ||
		len(policy.RequiredPaths) == 0 || !sort.StringsAreSorted(policy.RequiredPaths) {
		return errors.New("compiled recovery source-selection policy ordering is invalid")
	}
	expectedPackageRoots := []string{
		recoveryServerPackageRoot,
		recoveryCommandPackageRoot,
		recoveryInternalPackageRoot,
		recoveryOfflinePackageRoot,
		recoveryOfflineCommandPackageRoot,
		recoveryOfflineInternalPackageRoot,
	}
	if len(policy.PackageRoots) != len(expectedPackageRoots) {
		return errors.New("compiled recovery package-root policy is invalid")
	}
	for index, packageRoot := range policy.PackageRoots {
		if packageRoot != expectedPackageRoots[index] {
			return errors.New("compiled recovery package-root policy is invalid")
		}
	}
	for index, extension := range policy.BuildFileExtensions {
		if extension == "" || extension[0] != '.' || strings.Contains(extension, "/") ||
			index > 0 && extension == policy.BuildFileExtensions[index-1] {
			return errors.New("compiled recovery build-extension policy is invalid")
		}
	}
	return nil
}

func rejectRecoveryWorkspaceFiles(root string) error {
	cursor := root
	for {
		for _, name := range []string{"go.work", "go.work.sum"} {
			candidate := filepath.Join(cursor, name)
			if _, err := os.Lstat(candidate); err == nil {
				return fmt.Errorf("recovery source ancestry contains forbidden workspace file %q", candidate)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect recovery workspace file %q: %w", candidate, err)
			}
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			break
		}
		cursor = parent
	}
	return nil
}

func validateSourceLayoutFileInfo(info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source layout entries must be direct regular files")
	}
	fingerprint, err := sourceFingerprint(info)
	if err != nil {
		return err
	}
	if fingerprint.UID != uint64(os.Geteuid()) {
		return errors.New("source layout file is not owned by the effective user")
	}
	if fingerprint.Nlink != 1 {
		return errors.New("source layout file must have exactly one filesystem link")
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("source layout file mode %04o is unsupported", info.Mode().Perm())
	}
	return nil
}

func sourceBuildExtensionAllowed(extension string, allowed []string) bool {
	index := sort.SearchStrings(allowed, extension)
	return index < len(allowed) && allowed[index] == extension
}

// recoveryReviewedPackageRoot maps one exact Go package directory to the
// generic reviewed namespace that authorizes it. Recovery source code may live
// at either module root or in exactly one package directory below that module's
// cmd or internal root. Deeper source-shaped cache/generated trees are not
// implicitly admitted by the namespace policy.
func recoveryReviewedPackageRoot(packageDirectory string) (string, bool) {
	switch packageDirectory {
	case recoveryServerPackageRoot:
		return recoveryServerPackageRoot, true
	case recoveryOfflinePackageRoot:
		return recoveryOfflinePackageRoot, true
	}
	parent := path.Dir(packageDirectory)
	switch parent {
	case recoveryCommandPackageRoot, recoveryInternalPackageRoot,
		recoveryOfflineCommandPackageRoot, recoveryOfflineInternalPackageRoot:
		return parent, true
	default:
		return packageDirectory, false
	}
}

func recoveryFixturePackageRoot(relativePath string) (string, bool) {
	segments := strings.Split(relativePath, "/")
	for index, segment := range segments {
		if segment == "testdata" && index > 0 && index < len(segments)-1 {
			packageDirectory := strings.Join(segments[:index], "/")
			if packageRoot, reviewed := recoveryReviewedPackageRoot(packageDirectory); reviewed {
				return packageRoot, true
			}
			return packageDirectory, true
		}
	}
	return "", false
}

func validateRecoveryModuleFile(body []byte) error {
	if err := validateRecoveryModuleBody(body, recoveryModulePath); err != nil {
		return err
	}
	if len(recoveryModuleReplaceDirectives(body)) > 0 {
		return errors.New("server/go.mod contains a forbidden replace directive")
	}
	return nil
}

// validateRecoveryOfflineModuleFile admits exactly the one replace directive
// that binds the nested offline module to the production module beside it.
func validateRecoveryOfflineModuleFile(body []byte) error {
	if err := validateRecoveryModuleBody(body, recoveryOfflineModulePath); err != nil {
		return err
	}
	directives := recoveryModuleReplaceDirectives(body)
	if len(directives) != 1 {
		return fmt.Errorf("server/offline/go.mod declares %d replace directives, want exactly %q",
			len(directives), recoveryOfflineModuleBind)
	}
	if directives[0] != recoveryOfflineModuleBind {
		return fmt.Errorf("server/offline/go.mod contains an unauthorized replace directive %q", directives[0])
	}
	return nil
}

func validateRecoveryModuleBody(body []byte, relativePath string) error {
	if len(body) == 0 || len(body) > MaxSourceFileBytes || !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return fmt.Errorf("%s is empty or malformed", relativePath)
	}
	return nil
}

func recoveryModuleReplaceDirectives(body []byte) []string {
	var directives []string
	for _, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if comment := strings.Index(trimmed, "//"); comment >= 0 {
			trimmed = strings.TrimSpace(trimmed[:comment])
		}
		if strings.HasPrefix(trimmed, "replace") &&
			(len(trimmed) == len("replace") || trimmed[len("replace")] == '(' ||
				trimmed[len("replace")] == ' ' || trimmed[len("replace")] == '\t' || trimmed[len("replace")] == '\r') {
			directives = append(directives, trimmed)
		}
	}
	return directives
}

func goEmbedPatterns(relativePath string, body []byte) ([]string, error) {
	parsed, err := parser.ParseFile(token.NewFileSet(), relativePath, body, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse Go source %q for embed directives: %w", relativePath, err)
	}
	var patterns []string
	for _, group := range parsed.Comments {
		for _, comment := range group.List {
			if !strings.HasPrefix(comment.Text, "//go:embed") {
				continue
			}
			if len(comment.Text) == len("//go:embed") || comment.Text[len("//go:embed")] != ' ' && comment.Text[len("//go:embed")] != '\t' {
				return nil, fmt.Errorf("Go source %q contains malformed go:embed directive", relativePath)
			}
			directivePatterns, err := splitEmbedPatterns(strings.TrimSpace(comment.Text[len("//go:embed"):]))
			if err != nil {
				return nil, fmt.Errorf("Go source %q contains malformed go:embed directive: %w", relativePath, err)
			}
			patterns = append(patterns, directivePatterns...)
		}
	}
	return patterns, nil
}

func splitEmbedPatterns(value string) ([]string, error) {
	if value == "" {
		return nil, errors.New("embed pattern list is empty")
	}
	var patterns []string
	for value != "" {
		value = strings.TrimLeft(value, " \t")
		if value == "" {
			break
		}
		var patternValue string
		if value[0] == '"' || value[0] == '`' {
			quoted, err := strconv.QuotedPrefix(value)
			if err != nil {
				return nil, err
			}
			patternValue, err = strconv.Unquote(quoted)
			if err != nil {
				return nil, err
			}
			value = value[len(quoted):]
		} else {
			end := strings.IndexAny(value, " \t")
			if end < 0 {
				patternValue, value = value, ""
			} else {
				patternValue, value = value[:end], value[end:]
			}
		}
		if err := validateEmbedPattern(patternValue); err != nil {
			return nil, err
		}
		patterns = append(patterns, patternValue)
	}
	if len(patterns) == 0 {
		return nil, errors.New("embed pattern list is empty")
	}
	return patterns, nil
}

func validateEmbedPattern(value string) error {
	patternValue := strings.TrimPrefix(value, "all:")
	if patternValue == "" || len(patternValue) > MaxPathBytes || !utf8.ValidString(patternValue) ||
		strings.HasPrefix(patternValue, "/") || strings.HasSuffix(patternValue, "/") || strings.Contains(patternValue, "\\") {
		return fmt.Errorf("invalid embed pattern %q", value)
	}
	for _, segment := range strings.Split(patternValue, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("invalid embed pattern %q", value)
		}
	}
	if _, err := path.Match(patternValue, "source-selection-probe"); err != nil {
		return fmt.Errorf("invalid embed pattern %q: %w", value, err)
	}
	return nil
}

func (tree *sourceTree) expandEmbedPattern(goFile, declaredPattern string) ([]string, error) {
	includeHidden := strings.HasPrefix(declaredPattern, "all:")
	patternValue := strings.TrimPrefix(declaredPattern, "all:")
	packageDirectory := path.Dir(goFile)
	matchedDirectories := make(map[string]struct{})
	selected := make(map[string]struct{})
	err := fs.WalkDir(tree.root.FS(), packageDirectory, func(relativePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if relativePath == packageDirectory {
			return nil
		}
		local := strings.TrimPrefix(relativePath, packageDirectory+"/")
		if local == relativePath || local == "" {
			return fmt.Errorf("embed walk path %q is outside package %q", relativePath, packageDirectory)
		}
		if !includeHidden && embedPathHidden(local) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("go:embed pattern %q in %q encounters symlink %q", declaredPattern, goFile, relativePath)
		}
		matched, err := path.Match(patternValue, local)
		if err != nil {
			return err
		}
		underMatchedDirectory := false
		for directory := range matchedDirectories {
			if strings.HasPrefix(local, directory+"/") {
				underMatchedDirectory = true
				break
			}
		}
		if entry.IsDir() {
			if matched {
				matchedDirectories[local] = struct{}{}
			}
			return nil
		}
		if matched || underMatchedDirectory {
			if !validDataPath(relativePath) {
				return fmt.Errorf("go:embed selected noncanonical source path %q", relativePath)
			}
			selected[relativePath] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("expand go:embed pattern %q in %q: %w", declaredPattern, goFile, err)
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("go:embed pattern %q in %q matches no files", declaredPattern, goFile)
	}
	matches := make([]string, 0, len(selected))
	for relativePath := range selected {
		matches = append(matches, relativePath)
	}
	sort.Strings(matches)
	return matches, nil
}

func embedPathHidden(relativePath string) bool {
	for _, segment := range strings.Split(relativePath, "/") {
		if strings.HasPrefix(segment, ".") || strings.HasPrefix(segment, "_") {
			return true
		}
	}
	return false
}

func enforceSourceSelectionCount(count, maximum int) error {
	if count <= 0 || count > maximum {
		return fmt.Errorf("recovery source selection file count %d is outside the bound 1..%d", count, maximum)
	}
	return nil
}

func addSourceAggregate(total *int64, size, maximum int64) error {
	if total == nil || *total < 0 || *total > maximum || size < 0 || size > maximum-*total {
		return fmt.Errorf("recovery source selection exceeds aggregate byte ceiling %d", maximum)
	}
	*total += size
	return nil
}

func sortedSourceComponentKeysFromFixtures(fixtures map[string]RecoveryFixtureIdentityV2) []string {
	paths := make([]string, 0, len(fixtures))
	for relativePath := range fixtures {
		paths = append(paths, relativePath)
	}
	sort.Strings(paths)
	return paths
}

func orderedRecoveryFixtureIdentities(fixtures map[string]RecoveryFixtureIdentityV2) []RecoveryFixtureIdentityV2 {
	paths := sortedSourceComponentKeysFromFixtures(fixtures)
	identities := make([]RecoveryFixtureIdentityV2, len(paths))
	for index, relativePath := range paths {
		identities[index] = fixtures[relativePath]
	}
	return identities
}
