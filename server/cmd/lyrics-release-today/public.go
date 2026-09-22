package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"moesekai/server/internal/lyricscontract"
	"moesekai/server/internal/lyricsperformers"
	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/lyricsstaging"
	"moesekai/server/internal/model"
	"moesekai/server/internal/store"
)

var publicForbiddenFields = map[string]struct{}{
	"romaji": {}, "romanization": {}, "romanized": {}, "raw": {}, "rawbytes": {},
	"indexevidencerefs": {}, "documentjson": {}, "fixedidentityjson": {}, "privatereview": {},
	"sourceurl": {}, "sourcesha1": {}, "revisiontimestamp": {}, "compositionrenditionkey": {},
	"versionreason": {}, "sourcenote": {}, "licensenote": {},
}

type publicResult struct {
	RootSHA256  string
	SongCount   int
	DetailCount int
}

func runCheckPublic(ctx context.Context, arguments []string) (publicResult, error) {
	var validationReceiptPath, rootPath, manifestPath, deploymentReceiptPath, importReceiptPath, baseURL, publicRoot string
	flags := flag.NewFlagSet("check-public", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&validationReceiptPath, "validation-receipt", "", "exact fresh-validation receipt")
	flags.StringVar(&rootPath, "root-manifest", "", "validated final root manifest")
	flags.StringVar(&manifestPath, "import-manifest", "", "validated import manifest")
	flags.StringVar(&deploymentReceiptPath, "deployment-receipt", "", "verified production deployment receipt")
	flags.StringVar(&importReceiptPath, "import-receipt", "", "durable import receipt")
	flags.StringVar(&baseURL, "base-url", "", "expected production HTTPS origin")
	flags.StringVar(&publicRoot, "public-root", "/files/translation/lyrics", "one exact public lyrics projection root")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return publicResult{}, errors.New("check-public requires explicit release, deployment, import, and HTTPS flags")
	}
	for _, path := range []string{validationReceiptPath, rootPath, manifestPath, deploymentReceiptPath, importReceiptPath} {
		if !canonicalAbsolutePath(path) {
			return publicResult{}, errors.New("check-public file paths must be explicit canonical absolute paths")
		}
	}
	if !validPublicRoot(publicRoot) {
		return publicResult{}, errors.New("public root is not one of the closed canonical or v2 locale lyrics roots")
	}
	parsedBase, err := validateHTTPSBaseURL(baseURL)
	if err != nil {
		return publicResult{}, err
	}
	bundle, err := loadValidatedReleaseBundle(validationReceiptPath, rootPath, manifestPath)
	if err != nil {
		return publicResult{}, err
	}
	_, _, importReceiptSHA256, err := loadBoundReleaseImportReceipt(importReceiptPath, bundle)
	if err != nil {
		return publicResult{}, err
	}
	deploymentBody, _, err := readPinnedRegular(deploymentReceiptPath, "deployment receipt", maxReceiptBytes, 0o600)
	if err != nil {
		return publicResult{}, err
	}
	var deployment deploymentReceipt
	if err := decodeStrictJSON(deploymentBody, &deployment, "deployment receipt"); err != nil {
		return publicResult{}, err
	}
	if err := validateDeploymentReceipt(deployment, bundle, importReceiptSHA256, baseURL); err != nil {
		return publicResult{}, err
	}
	catalogVocals, err := loadValidatedCatalogVocals(ctx, bundle.Validation)
	if err != nil {
		return publicResult{}, err
	}

	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	indexBody, err := getPublicAsset(ctx, client, parsedBase, publicRoot+"/index.json")
	if err != nil {
		return publicResult{}, err
	}
	var index store.PublicLyricsIndexDocument
	if err := decodeStrictJSON(indexBody, &index, "public lyrics index"); err != nil {
		return publicResult{}, err
	}
	if index.Version != 2 || len(index.Songs) != releaseCatalogTargetCount {
		return publicResult{}, errors.New("public lyrics index is not the complete v2 698-song release")
	}
	for position, song := range index.Songs {
		draft := bundle.Bindings.Manifest.Items[position]
		versions := publicVersions(draft.Document)
		if song.MusicID != draft.MusicID || song.Revision <= 0 || song.Title.Japanese != draft.JapaneseTitle ||
			!reflect.DeepEqual(song.AvailableVersions, versions) {
			return publicResult{}, fmt.Errorf("public lyrics index song %d drifted from the release manifest", draft.MusicID)
		}
		if _, err := canonicalTimestamp(song.UpdatedAt); err != nil {
			return publicResult{}, fmt.Errorf("public lyrics index music %d updatedAt is not canonical", draft.MusicID)
		}
	}

	for position, song := range index.Songs {
		if err := ctx.Err(); err != nil {
			return publicResult{}, err
		}
		path := fmt.Sprintf("%s/music_%d.json", publicRoot, song.MusicID)
		body, err := getPublicAsset(ctx, client, parsedBase, path)
		if err != nil {
			return publicResult{}, err
		}
		if err := rejectJSONKeys(body, publicForbiddenFields, "public lyrics detail"); err != nil {
			return publicResult{}, err
		}
		var detail store.PublicLyricsDetailDocument
		if err := decodeStrictJSON(body, &detail, "public lyrics detail"); err != nil {
			return publicResult{}, err
		}
		draft := bundle.Bindings.Manifest.Items[position]
		vocals, found := catalogVocals[draft.MusicID]
		if !found {
			return publicResult{}, fmt.Errorf("validated catalog is missing music %d vocal signals", draft.MusicID)
		}
		if err := validatePublicDetail(detail, song, draft, vocals); err != nil {
			return publicResult{}, err
		}
	}
	return publicResult{RootSHA256: bundle.Bindings.Root.RootSHA256, SongCount: len(index.Songs), DetailCount: len(index.Songs)}, nil
}

func validPublicRoot(value string) bool {
	switch value {
	case "/files/translation/lyrics",
		"/files/v2/ja-JP/translation/lyrics",
		"/files/v2/zh-CN/translation/lyrics",
		"/files/v2/en-US/translation/lyrics":
		return true
	default:
		return false
	}
}

func getPublicAsset(ctx context.Context, client *http.Client, base *url.URL, path string) ([]byte, error) {
	requestURL := *base
	requestURL.Path = path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, model.PublicLyricsMaxArtifactBytes+1))
	if err != nil || len(body) > model.PublicLyricsMaxArtifactBytes {
		return nil, fmt.Errorf("GET %s returned an unreadable or oversized public artifact", path)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "application/json") {
		return nil, fmt.Errorf("GET %s did not return an exact JSON success", path)
	}
	return body, nil
}

func validatePublicDetail(
	detail store.PublicLyricsDetailDocument,
	index store.PublicLyricsIndexSong,
	draft lyricsstaging.Draft,
	vocals []model.CatalogVocalSignal,
) error {
	document := draft.Document
	expectedPerformers, err := authoritativePublicPerformerIDs(document, vocals)
	if err != nil {
		return fmt.Errorf("public lyrics detail music %d performer authority: %w", draft.MusicID, err)
	}
	if detail.Version != 2 || detail.MusicID != draft.MusicID || detail.Revision != index.Revision ||
		detail.UpdatedAt != index.UpdatedAt || detail.Attribution != "" ||
		!reflect.DeepEqual(detail.AvailableVersions, publicVersions(document)) ||
		!reflect.DeepEqual(detail.Attributions, publicAttributions(document)) || len(detail.Lines) != len(document.Full.Lines) {
		return fmt.Errorf("public lyrics detail music %d header drifted from the release manifest", draft.MusicID)
	}
	for lineIndex, line := range detail.Lines {
		source := document.Full.Lines[lineIndex]
		if line.ID != source.ID || line.Order != lineIndex || line.Japanese != source.Text ||
			line.StanzaBreakBefore != source.StanzaBreakBefore || len(line.Segments) != len(source.Segments) {
			return fmt.Errorf("public lyrics detail music %d Full line %d drifted", draft.MusicID, lineIndex+1)
		}
		for segmentIndex, segment := range line.Segments {
			sourceSegment := source.Segments[segmentIndex]
			if segment.Text != sourceSegment.Text || segment.PerformerIDs == nil ||
				!reflect.DeepEqual(segment.PerformerIDs, expectedPerformers[lineIndex][segmentIndex]) ||
				segment.Ruby == nil || len(segment.Ruby) != len(sourceSegment.Ruby) {
				return fmt.Errorf("public lyrics detail music %d Full segment drifted", draft.MusicID)
			}
			for rubyIndex, ruby := range segment.Ruby {
				if ruby.Text != sourceSegment.Ruby[rubyIndex].Text || ruby.Reading != sourceSegment.Ruby[rubyIndex].Reading {
					return fmt.Errorf("public lyrics detail music %d ruby drifted", draft.MusicID)
				}
			}
		}
	}
	if document.GameProjection == nil {
		if detail.GameProjection != nil {
			return fmt.Errorf("public lyrics detail music %d exposed an unauthorized Game projection", draft.MusicID)
		}
	} else if detail.GameProjection == nil || detail.GameProjection.ReasonCode != document.ReasonCode ||
		!reflect.DeepEqual(detail.GameProjection.LineIDs, document.GameProjection.LineIDs) {
		return fmt.Errorf("public lyrics detail music %d Game projection drifted", draft.MusicID)
	}
	return nil
}

func publicVersions(document model.LyricsSourceDocument) []string {
	if document.GameProjection != nil {
		return []string{"full", "game"}
	}
	return []string{"full"}
}

func publicAttributions(document model.LyricsSourceDocument) []store.PublicLyricsAttribution {
	used := make(map[string]struct{})
	for _, renditionKey := range releaseComponentRefs(document) {
		used[renditionKey] = struct{}{}
	}
	result := make([]store.PublicLyricsAttribution, 0, len(used))
	seen := make(map[string]struct{})
	for _, identity := range document.FixedIdentities {
		if _, found := used[identity.RenditionKey]; !found {
			continue
		}
		licenseName, licenseURL := publicLicense(identity.Provider)
		item := store.PublicLyricsAttribution{
			Provider: identity.Provider, Title: identity.Title, RevisionID: identity.RevisionID,
			RevisionURL: identity.CanonicalURL, LicenseName: licenseName, LicenseURL: licenseURL,
		}
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%s\x00%s\x00%s", item.Provider, item.Title,
			item.RevisionID, item.RevisionURL, item.LicenseName, item.LicenseURL)
		if _, duplicate := seen[key]; !duplicate {
			seen[key] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}

func publicLicense(provider model.LyricsSourceProvider) (string, string) {
	switch provider {
	case model.LyricsSourceProviderVocaloidFandom:
		return "CC BY-SA 3.0", "https://creativecommons.org/licenses/by-sa/3.0/"
	case model.LyricsSourceProviderMoegirl:
		return "CC BY-NC-SA 3.0", "https://creativecommons.org/licenses/by-nc-sa/3.0/"
	case model.LyricsSourceProviderSekaipedia:
		return "CC BY-SA 4.0", "https://creativecommons.org/licenses/by-sa/4.0/"
	default:
		return "", ""
	}
}

func loadValidatedCatalogVocals(
	ctx context.Context,
	receipt releaseValidationReceipt,
) (result map[int][]model.CatalogVocalSignal, returnErr error) {
	binding := receipt.Catalog.File
	if binding.Path != releaseCatalogPath || binding.SHA256 != releaseCatalogSHA256 ||
		binding.ByteCount <= 0 || receipt.Catalog.RecordCount != releaseCatalogTargetCount ||
		receipt.Catalog.MusicIDsSHA256 != releaseCatalogMusicIDsSHA256 {
		return nil, errors.New("validation receipt does not bind the reviewed 698 catalog")
	}
	database, err := openReadOnlySQLite(ctx, binding.Path, "validated release catalog", 0o444)
	if err != nil {
		return nil, err
	}
	defer func() { returnErr = errors.Join(returnErr, database.close()) }()
	if database.initialSHA256 != binding.SHA256 || database.pinned.info.Size() != binding.ByteCount {
		return nil, errors.New("validated release catalog bytes do not match the validation receipt")
	}
	if err := verifySQLiteIntegrity(ctx, database.db); err != nil {
		return nil, err
	}
	rows, err := database.db.QueryContext(ctx, `SELECT music_id,vocal_signals_json FROM catalog_music ORDER BY music_id`)
	if err != nil {
		return nil, fmt.Errorf("query validated release catalog vocals: %w", err)
	}
	ids := make([]int, 0, releaseCatalogTargetCount)
	result = make(map[int][]model.CatalogVocalSignal, releaseCatalogTargetCount)
	lastMusicID := 0
	for rows.Next() {
		var musicID int
		var body string
		if err := rows.Scan(&musicID, &body); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if musicID <= lastMusicID {
			_ = rows.Close()
			return nil, errors.New("validated release catalog music IDs are not strictly ordered")
		}
		var vocals []model.CatalogVocalSignal
		if err := decodeStrictJSON([]byte(body), &vocals, "validated catalog vocal signals"); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("music %d catalog vocal signals: %w", musicID, err)
		}
		if vocals == nil {
			vocals = []model.CatalogVocalSignal{}
		}
		ids = append(ids, musicID)
		result[musicID] = vocals
		lastMusicID = musicID
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	musicIDsSHA256, err := lyricscontract.OrderedMusicIDsSHA256(ids)
	if err != nil || len(ids) != receipt.Catalog.RecordCount || musicIDsSHA256 != receipt.Catalog.MusicIDsSHA256 {
		return nil, errors.New("validated release catalog rows do not match the receipt's exact ordered 698 scope")
	}
	if err := database.verifyUnchanged(); err != nil {
		return nil, err
	}
	if err := rejectSQLiteSidecars(binding.Path, "validated release catalog"); err != nil {
		return nil, err
	}
	return result, nil
}

func authoritativePublicPerformerIDs(
	document model.LyricsSourceDocument,
	vocals []model.CatalogVocalSignal,
) ([][][]int, error) {
	if err := lyricscontract.ValidatePersistedPerformerMetadata(document.Full); err != nil {
		return nil, errors.New("authoritative Full contains unsafe persisted performer metadata")
	}
	policy := lyricssource.PerformerSegmentationPolicyFromCatalogVocals(vocals)
	kind := document.Full.Version.Kind
	if kind != "original" && kind != "sekai" && kind != "vocaloid" {
		return nil, errors.New("authoritative Full has an invalid rendition kind")
	}
	if policy == lyricssource.PerformerSegmentationDisabled && kind == "sekai" {
		return nil, errors.New("catalog-disabled music cannot publish a SEKAI Full rendition")
	}
	if policy != lyricssource.PerformerSegmentationDisabled &&
		policy != lyricssource.PerformerSegmentationSekaiEligible {
		return nil, errors.New("authoritative Full has an invalid catalog rendition policy")
	}
	authoritativeStructured := document.PrivateReview != nil &&
		document.PrivateReview.PerformerSegmentationEvidence ==
			model.LyricsSourcePerformerSegmentationEvidenceAuthoritativeCompleteStructured
	if document.Provenance.PerformerSegmentation == nil {
		if len(document.Full.Performers) != 0 {
			return nil, errors.New("performer-free Full unexpectedly declares performers")
		}
	} else if kind != "sekai" && !authoritativeStructured {
		return nil, errors.New("non-SEKAI Full lacks authoritative structured performer evidence")
	}

	declared := make(map[string]int, len(document.Full.Performers))
	seenNumeric := make(map[int]struct{}, len(document.Full.Performers))
	for _, performer := range document.Full.Performers {
		if _, duplicate := declared[performer.PerformerID]; duplicate {
			return nil, errors.New("authoritative Full repeats a performer identity")
		}
		performerID, mapped := mappedPersistedPerformerID(performer.PerformerID)
		if mapped {
			if _, duplicate := seenNumeric[performerID]; duplicate {
				return nil, errors.New("authoritative Full aliases one numeric performer more than once")
			}
			seenNumeric[performerID] = struct{}{}
			declared[performer.PerformerID] = performerID
		} else {
			declared[performer.PerformerID] = 0
		}
	}

	result := make([][][]int, len(document.Full.Lines))
	for lineIndex, line := range document.Full.Lines {
		result[lineIndex] = make([][]int, len(line.Segments))
		for segmentIndex, segment := range line.Segments {
			expected := make([]int, 0, len(segment.PerformerIDs))
			seen := make(map[int]struct{}, len(segment.PerformerIDs))
			for _, sourceID := range segment.PerformerIDs {
				performerID, found := declared[sourceID]
				if !found {
					return nil, errors.New("authoritative segment references an undeclared performer")
				}
				if performerID == 0 {
					continue
				}
				if _, duplicate := seen[performerID]; !duplicate {
					seen[performerID] = struct{}{}
					expected = append(expected, performerID)
				}
			}
			if document.Provenance.PerformerSegmentation == nil && len(expected) != 0 {
				return nil, errors.New("authoritative segment has unproven performer segmentation")
			}
			result[lineIndex][segmentIndex] = expected
		}
	}
	return result, nil
}

func mappedPersistedPerformerID(value string) (int, bool) {
	const prefix = "歌唱者-"
	if len(value) == len(prefix)+2 && strings.HasPrefix(value, prefix) {
		performerID, err := strconv.Atoi(value[len(prefix):])
		if err == nil && performerID >= 1 && performerID <= 26 && fmt.Sprintf("%s%02d", prefix, performerID) == value {
			return performerID, true
		}
	}
	if performer, found := lyricsperformers.BySourceID(value); found {
		return performer.NumericID, true
	}
	return 0, false
}
