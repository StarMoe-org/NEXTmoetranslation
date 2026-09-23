package lyricsimportreceipt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"moesekai/server/offline/internal/lyricsrootmanifest"
	"moesekai/server/offline/internal/lyricsstaging"
)

const (
	ValidationReceiptSchemaVersion = 1
	ValidationReceiptKind          = "moesekai-lyrics-release-validation-v1"
	MaxValidationReceiptBytes      = 4 << 20
	MaxCompactRootBytes            = 4 << 20

	maxValidationTreeFiles = 300_000
	maxValidationTreeBytes = int64(128 << 30)
)

type ValidationFileBinding struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	ByteCount int64  `json:"byteCount"`
}

type ValidationTreeBinding struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	FileCount int    `json:"fileCount"`
	ByteCount int64  `json:"byteCount"`
}

type ValidationPlanBinding struct {
	File                 ValidationFileBinding `json:"file"`
	PlanID               string                `json:"planId"`
	PlanSHA256           string                `json:"planSha256"`
	SourceRoot           string                `json:"sourceRoot"`
	SourceSnapshotSHA256 string                `json:"sourceSnapshotSha256"`
	SourceFileCount      int                   `json:"sourceFileCount"`
}

type ValidationCatalogBinding struct {
	File                  ValidationFileBinding `json:"file"`
	RecordCount           int                   `json:"recordCount"`
	IdentitySHA256        string                `json:"identitySha256"`
	MusicIDsSHA256        string                `json:"musicIdsSha256"`
	IdentityPolicyVersion string                `json:"identityPolicyVersion"`
}

type ValidationAcquisitionSetBinding struct {
	File      ValidationFileBinding `json:"file"`
	SetSHA256 string                `json:"setSha256"`
}

type ValidationEvidencePackBinding struct {
	Tree            ValidationTreeBinding `json:"tree"`
	PackSHA256      string                `json:"packSha256"`
	SelectionSHA256 string                `json:"selectionSha256"`
	EvidenceCount   int                   `json:"evidenceCount"`
	ShardCount      int                   `json:"shardCount"`
}

type ValidationRootBinding struct {
	File       ValidationFileBinding `json:"file"`
	RootID     string                `json:"rootId"`
	RootSHA256 string                `json:"rootSha256"`
}

type ValidationImportManifestBinding struct {
	File        ValidationFileBinding `json:"file"`
	BatchSHA256 string                `json:"batchSha256"`
	ItemCount   int                   `json:"itemCount"`
}

type ValidationImportEvidenceBinding struct {
	File          ValidationFileBinding `json:"file"`
	ReceiptSHA256 string                `json:"receiptSha256"`
	EvidenceCount int                   `json:"evidenceCount"`
}

type ValidationReceipt struct {
	SchemaVersion        int                             `json:"schemaVersion"`
	Kind                 string                          `json:"kind"`
	Plan                 ValidationPlanBinding           `json:"plan"`
	Catalog              ValidationCatalogBinding        `json:"catalog"`
	Ledger               ValidationTreeBinding           `json:"ledger"`
	AcquisitionSet       ValidationAcquisitionSetBinding `json:"acquisitionSet"`
	ProviderOutcomes     ValidationTreeBinding           `json:"providerOutcomes"`
	SongResults          ValidationTreeBinding           `json:"songResults"`
	EvidencePack         ValidationEvidencePackBinding   `json:"evidencePack"`
	RootManifest         ValidationRootBinding           `json:"rootManifest"`
	ImportManifest       ValidationImportManifestBinding `json:"importManifest"`
	ImportEvidence       ValidationImportEvidenceBinding `json:"importEvidence"`
	AcquisitionCount     int                             `json:"acquisitionCount"`
	ProviderOutcomeCount int                             `json:"providerOutcomeCount"`
	ReceiptPath          string                          `json:"receiptPath"`
	ReceiptSHA256        string                          `json:"receiptSha256"`
}

type BundleInputs struct {
	ValidationPath       string
	ValidationFileSHA256 string
	ValidationFileBytes  int64
	Validation           ValidationReceipt
	RootPath             string
	RootFileSHA256       string
	RootFileBytes        int64
	Root                 lyricsrootmanifest.Manifest
	ManifestPath         string
	ManifestFileSHA256   string
	ManifestFileBytes    int64
	Manifest             lyricsstaging.Manifest
	EvidencePath         string
	EvidenceFileSHA256   string
	EvidenceFileBytes    int64
	Evidence             lyricsstaging.PrivateEvidenceReceipt
	ImportReceiptPath    string
}

func ValidationReceiptDigest(receipt ValidationReceipt) (string, error) {
	receipt.ReceiptSHA256 = ""
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(ValidationReceiptKind + "\x00"))
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func MarshalValidationReceipt(receipt ValidationReceipt) ([]byte, error) {
	if err := ValidateValidationReceipt(receipt, receipt.ReceiptPath); err != nil {
		return nil, err
	}
	return marshalCanonicalJSON(receipt, MaxValidationReceiptBytes, "release validation receipt")
}

func DecodeValidationReceipt(body []byte, path string) (ValidationReceipt, error) {
	var receipt ValidationReceipt
	if err := decodeStrictCanonicalJSON(body, &receipt, MaxValidationReceiptBytes, "release validation receipt"); err != nil {
		return ValidationReceipt{}, err
	}
	if err := ValidateValidationReceipt(receipt, path); err != nil {
		return ValidationReceipt{}, err
	}
	return receipt, nil
}

func ValidateValidationReceipt(receipt ValidationReceipt, path string) error {
	if receipt.SchemaVersion != ValidationReceiptSchemaVersion || receipt.Kind != ValidationReceiptKind ||
		receipt.ReceiptPath != path || !canonicalAbsolutePath(receipt.ReceiptPath) ||
		!validValidationFile(receipt.Plan.File) || !boundedText(receipt.Plan.PlanID, 256) ||
		receipt.Plan.PlanSHA256 != receipt.Plan.File.SHA256 || !canonicalAbsolutePath(receipt.Plan.SourceRoot) ||
		!canonicalSHA256.MatchString(receipt.Plan.SourceSnapshotSHA256) || receipt.Plan.SourceFileCount <= 0 ||
		!validValidationFile(receipt.Catalog.File) || receipt.Catalog.RecordCount <= 0 ||
		!canonicalSHA256.MatchString(receipt.Catalog.IdentitySHA256) ||
		!canonicalSHA256.MatchString(receipt.Catalog.MusicIDsSHA256) ||
		!boundedText(receipt.Catalog.IdentityPolicyVersion, 256) || !validValidationTree(receipt.Ledger) ||
		!validValidationFile(receipt.AcquisitionSet.File) || !canonicalSHA256.MatchString(receipt.AcquisitionSet.SetSHA256) ||
		!validValidationTree(receipt.ProviderOutcomes) || !validValidationTree(receipt.SongResults) ||
		!validValidationTree(receipt.EvidencePack.Tree) || !canonicalSHA256.MatchString(receipt.EvidencePack.PackSHA256) ||
		!canonicalSHA256.MatchString(receipt.EvidencePack.SelectionSHA256) || receipt.EvidencePack.EvidenceCount <= 0 ||
		receipt.EvidencePack.ShardCount <= 0 || receipt.EvidencePack.ShardCount+1 != receipt.EvidencePack.Tree.FileCount ||
		!validValidationFile(receipt.RootManifest.File) || !compactID.MatchString(receipt.RootManifest.RootID) ||
		!canonicalSHA256.MatchString(receipt.RootManifest.RootSHA256) || !validValidationFile(receipt.ImportManifest.File) ||
		!canonicalSHA256.MatchString(receipt.ImportManifest.BatchSHA256) || receipt.ImportManifest.ItemCount <= 0 ||
		!validValidationFile(receipt.ImportEvidence.File) || !canonicalSHA256.MatchString(receipt.ImportEvidence.ReceiptSHA256) ||
		receipt.ImportEvidence.EvidenceCount <= 0 || receipt.ImportEvidence.EvidenceCount != receipt.EvidencePack.EvidenceCount ||
		receipt.AcquisitionCount <= 0 || receipt.ProviderOutcomeCount <= 0 ||
		receipt.ProviderOutcomeCount != receipt.ProviderOutcomes.FileCount || !canonicalSHA256.MatchString(receipt.ReceiptSHA256) {
		return errors.New("release validation receipt does not satisfy the closed immutable bundle contract")
	}
	paths := []string{
		receipt.Plan.File.Path, receipt.Plan.SourceRoot, receipt.Catalog.File.Path, receipt.Ledger.Path,
		receipt.AcquisitionSet.File.Path, receipt.ProviderOutcomes.Path, receipt.SongResults.Path,
		receipt.EvidencePack.Tree.Path, receipt.RootManifest.File.Path, receipt.ImportManifest.File.Path,
		receipt.ImportEvidence.File.Path, receipt.ReceiptPath,
	}
	seen := make(map[string]struct{}, len(paths))
	for _, candidate := range paths {
		if _, duplicate := seen[candidate]; duplicate {
			return errors.New("release validation receipt aliases distinct bundle inputs")
		}
		seen[candidate] = struct{}{}
	}
	for _, directory := range []string{
		receipt.Plan.SourceRoot, receipt.Ledger.Path, receipt.ProviderOutcomes.Path,
		receipt.SongResults.Path, receipt.EvidencePack.Tree.Path,
	} {
		for _, candidate := range paths {
			if candidate != directory && pathInsideDirectory(directory, candidate) {
				return errors.New("release validation receipt nests a distinct bundle input inside a bound tree")
			}
		}
	}
	digest, err := ValidationReceiptDigest(receipt)
	if err != nil || digest != receipt.ReceiptSHA256 {
		return errors.New("release validation receipt digest does not match")
	}
	return nil
}

func ValidateBundleInputs(inputs BundleInputs) (Binding, error) {
	if !canonicalAbsolutePath(inputs.ValidationPath) || !canonicalSHA256.MatchString(inputs.ValidationFileSHA256) || inputs.ValidationFileBytes <= 0 ||
		!canonicalAbsolutePath(inputs.RootPath) || !canonicalSHA256.MatchString(inputs.RootFileSHA256) || inputs.RootFileBytes <= 0 ||
		!canonicalAbsolutePath(inputs.ManifestPath) || !canonicalSHA256.MatchString(inputs.ManifestFileSHA256) || inputs.ManifestFileBytes <= 0 ||
		!canonicalAbsolutePath(inputs.EvidencePath) || !canonicalSHA256.MatchString(inputs.EvidenceFileSHA256) || inputs.EvidenceFileBytes <= 0 ||
		!canonicalAbsolutePath(inputs.ImportReceiptPath) {
		return Binding{}, errors.New("validated import bundle paths and file identities are invalid")
	}
	if err := ValidateValidationReceipt(inputs.Validation, inputs.ValidationPath); err != nil {
		return Binding{}, err
	}
	validationBody, err := MarshalValidationReceipt(inputs.Validation)
	if err != nil {
		return Binding{}, err
	}
	rootBody, err := lyricsrootmanifest.MarshalCanonical(inputs.Root)
	if err != nil {
		return Binding{}, err
	}
	manifestBody, err := lyricsstaging.MarshalManifest(inputs.Manifest)
	if err != nil {
		return Binding{}, err
	}
	evidenceBody, err := lyricsstaging.MarshalPrivateEvidenceReceipt(inputs.Evidence)
	if err != nil {
		return Binding{}, err
	}
	for _, file := range []struct {
		body     []byte
		expected string
		bytes    int64
		label    string
	}{
		{validationBody, inputs.ValidationFileSHA256, inputs.ValidationFileBytes, "release validation receipt"},
		{rootBody, inputs.RootFileSHA256, inputs.RootFileBytes, "compact root manifest"},
		{manifestBody, inputs.ManifestFileSHA256, inputs.ManifestFileBytes, "import manifest"},
		{evidenceBody, inputs.EvidenceFileSHA256, inputs.EvidenceFileBytes, "import evidence receipt"},
	} {
		if err := verifyCanonicalFileIdentity(file.body, file.expected, file.bytes, file.label); err != nil {
			return Binding{}, err
		}
	}
	validation := inputs.Validation
	root := inputs.Root
	manifest := inputs.Manifest
	evidence := inputs.Evidence
	if inputs.RootPath != validation.RootManifest.File.Path ||
		inputs.RootFileSHA256 != validation.RootManifest.File.SHA256 || inputs.RootFileBytes != validation.RootManifest.File.ByteCount ||
		root.RootID != validation.RootManifest.RootID || root.RootSHA256 != validation.RootManifest.RootSHA256 ||
		root.Plan.PlanID != validation.Plan.PlanID || root.Plan.SHA256 != validation.Plan.PlanSHA256 ||
		root.Catalog.SourceSHA256 != validation.Catalog.File.SHA256 || root.Catalog.RecordCount != validation.Catalog.RecordCount ||
		root.Catalog.IdentitySHA256 != validation.Catalog.IdentitySHA256 ||
		root.Catalog.MusicIDsSHA256 != validation.Catalog.MusicIDsSHA256 ||
		root.Catalog.IdentityPolicyVersion != validation.Catalog.IdentityPolicyVersion ||
		root.Coverage.UniqueAcquisitionCount != validation.AcquisitionCount ||
		root.Coverage.ProviderOutcomeRefCount != validation.ProviderOutcomeCount ||
		root.Coverage.UniqueEvidenceCount != validation.EvidencePack.EvidenceCount ||
		root.EvidencePack.PackSHA256 != validation.EvidencePack.PackSHA256 ||
		root.EvidencePack.SelectionSHA256 != validation.EvidencePack.SelectionSHA256 ||
		root.EvidencePack.ItemCount != validation.EvidencePack.EvidenceCount ||
		root.EvidencePack.ShardCount != validation.EvidencePack.ShardCount {
		return Binding{}, errors.New("compact root does not match the exact immutable validation receipt")
	}
	if inputs.ManifestPath != validation.ImportManifest.File.Path ||
		inputs.ManifestFileSHA256 != validation.ImportManifest.File.SHA256 ||
		inputs.ManifestFileBytes != validation.ImportManifest.File.ByteCount ||
		manifest.BatchSHA256 != validation.ImportManifest.BatchSHA256 ||
		len(manifest.Items) != validation.ImportManifest.ItemCount || len(root.Songs) != len(manifest.Items) {
		return Binding{}, errors.New("import manifest does not match the exact immutable validation receipt and compact root")
	}
	for index, item := range manifest.Items {
		if item.MusicID != root.Songs[index].MusicID {
			return Binding{}, fmt.Errorf("import manifest item %d does not follow the compact-root music-ID order", index)
		}
	}
	if inputs.EvidencePath != validation.ImportEvidence.File.Path ||
		inputs.EvidenceFileSHA256 != validation.ImportEvidence.File.SHA256 ||
		inputs.EvidenceFileBytes != validation.ImportEvidence.File.ByteCount ||
		evidence.ReceiptSHA256 != validation.ImportEvidence.ReceiptSHA256 ||
		len(evidence.IndexEvidence) != validation.ImportEvidence.EvidenceCount {
		return Binding{}, errors.New("import evidence receipt does not match the exact immutable validation receipt")
	}
	return Binding{
		ValidationReceiptSHA256: validation.ReceiptSHA256,
		RootManifestSHA256:      inputs.RootFileSHA256,
		RootID:                  root.RootID,
		RootSHA256:              root.RootSHA256,
		ManifestSchemaVersion:   manifest.SchemaVersion,
		ManifestSHA256:          inputs.ManifestFileSHA256,
		BatchSHA256:             manifest.BatchSHA256,
		EvidenceReceiptPath:     inputs.EvidencePath,
		EvidenceReceiptSHA256:   evidence.ReceiptSHA256,
		ReceiptPath:             inputs.ImportReceiptPath,
	}, nil
}

func verifyCanonicalFileIdentity(body []byte, expectedSHA256 string, expectedBytes int64, label string) error {
	digest := sha256.Sum256(body)
	if int64(len(body)) != expectedBytes || hex.EncodeToString(digest[:]) != expectedSHA256 {
		return fmt.Errorf("%s canonical bytes do not match the pinned file SHA-256 and byte count", label)
	}
	return nil
}

func validValidationFile(binding ValidationFileBinding) bool {
	return canonicalAbsolutePath(binding.Path) && canonicalSHA256.MatchString(binding.SHA256) && binding.ByteCount > 0
}

func validValidationTree(binding ValidationTreeBinding) bool {
	return canonicalAbsolutePath(binding.Path) && canonicalSHA256.MatchString(binding.SHA256) &&
		binding.FileCount > 0 && binding.FileCount <= maxValidationTreeFiles &&
		binding.ByteCount > 0 && binding.ByteCount <= maxValidationTreeBytes
}

func pathInsideDirectory(directory, candidate string) bool {
	relative, err := filepath.Rel(directory, candidate)
	return err == nil && relative != "." && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && !filepath.IsAbs(relative)
}
