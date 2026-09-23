package lyricsstaging

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
)

func TestBuildDraftFromFixedArtifactsPreservesDistinctProviderRawReceipts(t *testing.T) {
	report, catalogIdentity, primaryFixed := validPreflightAndFixed(t)
	primaryCandidate := *report.UniqueComplete[0].Candidate
	primaryFixed.RevisionTimestamp = time.Unix(455, 123000000).UTC()
	primaryCandidate.RevisionTimestamp = primaryFixed.RevisionTimestamp.Format(time.RFC3339Nano)
	primaryEvidence := fixedRevisionEvidence(primaryCandidate.Provider, primaryFixed)
	primaryCandidate.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{{
		EvidenceID: primaryEvidence.EvidenceID, SHA256: primaryEvidence.SHA256,
	}}
	report.UniqueComplete[0].Candidate = &primaryCandidate
	primaryFixed.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, primaryCandidate.IndexEvidenceRefs...)
	primaryFixed.IndexEvidence = []lyricssource.IndexEvidence{primaryEvidence}

	base, err := BuildDraft(report.UniqueComplete[0], catalogIdentity, primaryFixed)
	if err != nil {
		t.Fatal(err)
	}
	secondaryRaw := []byte("== Version evidence ==\n萌娘百科固定版本")
	secondarySHA1 := sha1.Sum(secondaryRaw)
	secondaryCandidate := CandidateIdentity{
		Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
		PageID: 99, RevisionID: 100, SHA1: hex.EncodeToString(secondarySHA1[:]), Title: "合成試験曲/版本资料",
		CanonicalURL: canonicalMoegirlRevisionURL("合成試験曲/版本资料", 100), Categories: []string{"歌曲"},
		Section: "版本资料", RenditionKey: primaryCandidate.RenditionKey,
		VersionReason: model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid,
	}
	secondaryFixed := lyricssource.FixedRevision{
		Provider: secondaryCandidate.Provider, Origin: secondaryCandidate.Origin,
		PageID: secondaryCandidate.PageID, RevisionID: secondaryCandidate.RevisionID, SHA1: secondaryCandidate.SHA1,
		RevisionTimestamp: time.Unix(788, 456000000).UTC(),
		PageTitle:         secondaryCandidate.Title, CanonicalURL: secondaryCandidate.CanonicalURL,
		Categories: append([]string{}, secondaryCandidate.Categories...), FetchedAt: time.Unix(789, 123000000).UTC(),
		Wikitext: append([]byte{}, secondaryRaw...), Section: secondaryCandidate.Section,
		RenditionKey: secondaryCandidate.RenditionKey, VersionReason: secondaryCandidate.VersionReason,
	}
	secondaryCandidate.RevisionTimestamp = secondaryFixed.RevisionTimestamp.Format(time.RFC3339Nano)
	secondaryEvidence := fixedRevisionEvidence(secondaryCandidate.Provider, secondaryFixed)
	secondaryCandidate.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{{
		EvidenceID: secondaryEvidence.EvidenceID, SHA256: secondaryEvidence.SHA256,
	}}
	secondaryFixed.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, secondaryCandidate.IndexEvidenceRefs...)
	secondaryFixed.IndexEvidence = []lyricssource.IndexEvidence{secondaryEvidence}

	artifactKeys, err := ResolveArtifactRenditionKeys([]CandidateIdentity{primaryCandidate, secondaryCandidate})
	if err != nil {
		t.Fatal(err)
	}
	primaryIdentity := fixedIdentityFromArtifactWithKey(FixedArtifact{Candidate: primaryCandidate, Fixed: primaryFixed}, artifactKeys[0])
	secondaryIdentity := fixedIdentityFromArtifactWithKey(FixedArtifact{Candidate: secondaryCandidate, Fixed: secondaryFixed}, artifactKeys[1])
	document := base.Document
	document.FixedIdentities = []model.LyricsSourceFixedIdentity{primaryIdentity, secondaryIdentity}
	document.Provenance.FullText = model.LyricsSourceComponentRef{RenditionKey: primaryIdentity.RenditionKey}
	document.Provenance.PerformerSegmentation = &model.LyricsSourceComponentRef{RenditionKey: primaryIdentity.RenditionKey}
	document.Provenance.Ruby = &model.LyricsSourceComponentRef{RenditionKey: primaryIdentity.RenditionKey}
	document.Provenance.VersionEvidence = model.LyricsSourceComponentRef{RenditionKey: secondaryIdentity.RenditionKey}
	if err := model.ValidateLyricsSourceDocument(document); err != nil {
		t.Fatalf("multi-provider source document: %v", err)
	}
	primaryFixed.FixedIdentities = append([]model.LyricsSourceFixedIdentity{}, document.FixedIdentities...)
	primaryFixed.Document = &document

	receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{secondaryEvidence, primaryEvidence, primaryEvidence})
	if err != nil {
		t.Fatal(err)
	}
	report.UniqueComplete[0].FixedArtifactCandidates = []CandidateIdentity{primaryCandidate, secondaryCandidate}
	report.UniqueComplete[0].CompositionReason = model.LyricsSourceVersionReasonUntaggedGameSubset
	report.EvidenceReceipt = &receipt
	if err := ValidatePreflight(report); err != nil {
		t.Fatalf("preflight receipt: %v", err)
	}

	draft, err := BuildDraftFromFixedArtifacts(report.UniqueComplete[0], catalogIdentity, FixedArtifactBundle{
		PostFetchState:    PostFetchStateComplete,
		CompositionReason: model.LyricsSourceVersionReasonUntaggedGameSubset,
		Artifacts: []FixedArtifact{
			{Candidate: primaryCandidate, Fixed: primaryFixed},
			{Candidate: secondaryCandidate, Fixed: secondaryFixed},
		},
		Components: FixedArtifactComponentSelection{
			FullText: artifactKeys[0], PerformerSegmentation: artifactKeys[0], Ruby: artifactKeys[0],
			VersionEvidence: artifactKeys[1],
		},
		EvidenceReceipt: receipt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if draft.Document.ReasonCode != model.LyricsSourceVersionReasonUntaggedGameSubset ||
		draft.Document.FixedIdentities[0].CompositionRenditionKey != primaryCandidate.RenditionKey ||
		draft.Document.FixedIdentities[1].CompositionRenditionKey != primaryCandidate.RenditionKey ||
		draft.Document.FixedIdentities[0].RenditionKey == draft.Document.FixedIdentities[1].RenditionKey ||
		draft.Document.FixedIdentities[0].VersionReason == draft.Document.ReasonCode ||
		draft.Document.FixedIdentities[1].VersionReason == draft.Document.ReasonCode {
		t.Fatalf("logical/artifact/reason seams were not preserved: %+v", draft.Document)
	}
	artifactsByProvider := make(map[model.LyricsSourceProvider]Artifact, len(draft.Artifacts))
	for _, artifact := range draft.Artifacts {
		artifactsByProvider[artifact.Identity.Provider] = artifact
	}
	fandomArtifact := artifactsByProvider[model.LyricsSourceProviderVocaloidFandom]
	moegirlArtifact := artifactsByProvider[model.LyricsSourceProviderMoegirl]
	if len(draft.Artifacts) != 2 || len(artifactsByProvider) != 2 ||
		fandomArtifact.Identity.RenditionKey != artifactKeys[0] || moegirlArtifact.Identity.RenditionKey != artifactKeys[1] ||
		fandomArtifact.Identity.RevisionTimestamp != primaryCandidate.RevisionTimestamp ||
		moegirlArtifact.Identity.RevisionTimestamp != secondaryCandidate.RevisionTimestamp ||
		fandomArtifact.RawWikitextSHA256 == moegirlArtifact.RawWikitextSHA256 ||
		fandomArtifact.RawWikitextByteCount != len(primaryFixed.Wikitext) ||
		moegirlArtifact.RawWikitextByteCount != len(secondaryFixed.Wikitext) {
		t.Fatalf("plural fixed artifacts=%+v", draft.Artifacts)
	}
	body, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, primaryFixed.Wikitext) || bytes.Contains(body, secondaryFixed.Wikitext) ||
		bytes.Contains(body, []byte(`"evidenceReceipt"`)) || bytes.Contains(body, []byte(`"indexEvidence":`)) {
		t.Fatalf("staged draft leaked private raw/evidence bundle: %s", body)
	}
	reportDigest := sha256.Sum256([]byte("audited plural preflight"))
	manifest, err := NewManifest(report, hex.EncodeToString(reportDigest[:]), []Draft{draft})
	if err != nil {
		t.Fatalf("plural logical-rendition manifest: %v", err)
	}
	if len(manifest.Items) != 1 || len(manifest.Items[0].Artifacts) != 2 {
		t.Fatalf("plural manifest=%+v", manifest)
	}
}

func TestValidatePreflightRetainsPluralPostFetchVersionConflictOutsideManifest(t *testing.T) {
	report, catalogIdentity, _ := validPreflightAndFixed(t)
	primary := *report.UniqueComplete[0].Candidate
	secondary := CandidateIdentity{
		Provider: model.LyricsSourceProviderMoegirl, Origin: model.LyricsSourceOriginMoegirl,
		PageID: 99, RevisionID: 100, SHA1: strings.Repeat("c", 40), Title: "合成試験曲/歌词",
		CanonicalURL: canonicalMoegirlRevisionURL("合成試験曲/歌词", 100), Categories: []string{"歌曲"},
		Section: "歌词", RenditionKey: primary.RenditionKey,
		VersionReason: model.LyricsSourceVersionReasonTaggedGameOnlyFullFromVocaloid,
		IndexEvidenceRefs: []model.LyricsSourceIndexEvidenceRef{{
			EvidenceID: "search:moegirl:99", SHA256: strings.Repeat("d", 64),
		}},
	}
	conflict := report.UniqueComplete[0]
	conflict.Candidate = nil
	conflict.FixedArtifactCandidates = []CandidateIdentity{primary, secondary}
	conflict.LineCount = 0
	conflict.PostFetchState = PostFetchStateVersionConflict
	conflict.CompositionReason = model.LyricsSourceVersionReasonVersionConflict
	conflict.ErrorCode = string(model.LyricsSourceVersionReasonVersionConflict)
	report.UniqueComplete = []PreflightItem{}
	report.Incomplete = []PreflightItem{conflict}
	report.Summary = PreflightSummary{Incomplete: 1}
	if err := ValidatePreflight(report); err != nil {
		t.Fatalf("plural post-fetch conflict report: %v", err)
	}
	if draft, err := BuildDraftFromFixedArtifacts(conflict, catalogIdentity, FixedArtifactBundle{
		PostFetchState: PostFetchStateVersionConflict,
	}); err == nil || !reflect.DeepEqual(draft, Draft{}) {
		t.Fatalf("conflict produced draft=%+v err=%v", draft, err)
	}
	reportDigest := sha256.Sum256([]byte("audited conflict preflight"))
	if manifest, err := NewManifest(report, hex.EncodeToString(reportDigest[:]), nil); err == nil || !reflect.DeepEqual(manifest, Manifest{}) {
		t.Fatalf("conflict produced manifest=%+v err=%v", manifest, err)
	}
}

func TestBuildDraftFromFixedArtifactsFailsClosedOnReceiptAndRawResolution(t *testing.T) {
	report, catalogIdentity, fixed := validPreflightAndFixed(t)
	candidate := *report.UniqueComplete[0].Candidate
	evidence := fixedRevisionEvidence(candidate.Provider, fixed)
	candidate.IndexEvidenceRefs = []model.LyricsSourceIndexEvidenceRef{{EvidenceID: evidence.EvidenceID, SHA256: evidence.SHA256}}
	report.UniqueComplete[0].Candidate = &candidate
	fixed.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, candidate.IndexEvidenceRefs...)
	fixed.IndexEvidence = []lyricssource.IndexEvidence{evidence}
	receipt, err := NewPrivateEvidenceReceipt([]lyricssource.IndexEvidence{evidence})
	if err != nil {
		t.Fatal(err)
	}
	bundle := FixedArtifactBundle{
		Artifacts:       []FixedArtifact{{Candidate: candidate, Fixed: fixed}},
		EvidenceReceipt: receipt,
	}
	if _, err := BuildDraftFromFixedArtifacts(report.UniqueComplete[0], catalogIdentity, bundle); err != nil {
		t.Fatalf("valid private bundle: %v", err)
	}
	resolver, err := NewPrivateEvidenceResolver(receipt)
	if err != nil {
		t.Fatal(err)
	}
	resolverBundle := cloneFixedArtifactBundle(bundle)
	resolverBundle.EvidenceReceipt = PrivateEvidenceReceipt{}
	resolverBundle.EvidenceResolver = resolver
	resolverItem := report.UniqueComplete[0]
	resolverCandidate := resolverBundle.Artifacts[0].Candidate
	resolverItem.Candidate = &resolverCandidate
	if _, err := BuildDraftFromFixedArtifacts(resolverItem, catalogIdentity, resolverBundle); err != nil {
		t.Fatalf("valid shared resolver bundle: %v", err)
	}
	resolverDrift := cloneFixedArtifactBundle(resolverBundle)
	resolverDrift.Artifacts[0].Fixed.IndexEvidence[0].Raw[0] ^= 1
	driftItem := report.UniqueComplete[0]
	driftCandidate := resolverDrift.Artifacts[0].Candidate
	driftItem.Candidate = &driftCandidate
	if _, err := BuildDraftFromFixedArtifacts(driftItem, catalogIdentity, resolverDrift); err == nil {
		t.Fatal("shared resolver accepted fixed evidence drift")
	}
	ambiguous := cloneFixedArtifactBundle(bundle)
	ambiguous.EvidenceResolver = resolver
	ambiguousItem := resolverItem
	ambiguousCandidate := ambiguous.Artifacts[0].Candidate
	ambiguousItem.Candidate = &ambiguousCandidate
	if _, err := BuildDraftFromFixedArtifacts(ambiguousItem, catalogIdentity, ambiguous); err == nil {
		t.Fatal("fixed artifact bundle accepted both receipt and resolver evidence inputs")
	}

	for name, mutate := range map[string]func(*FixedArtifactBundle){
		"missing evidence": func(value *FixedArtifactBundle) {
			value.EvidenceReceipt.IndexEvidence = []lyricssource.IndexEvidence{}
			value.EvidenceReceipt.ReceiptSHA256, _ = privateEvidenceReceiptDigest(value.EvidenceReceipt)
		},
		"fixed evidence drift": func(value *FixedArtifactBundle) {
			value.Artifacts[0].Fixed.IndexEvidence[0].Raw = append([]byte{}, value.Artifacts[0].Fixed.IndexEvidence[0].Raw...)
			value.Artifacts[0].Fixed.IndexEvidence[0].Raw[0] ^= 1
		},
		"fixed evidence revision timestamp drift": func(value *FixedArtifactBundle) {
			value.Artifacts[0].Fixed.IndexEvidence[0].RevisionTimestamp = "1970-01-01T00:07:34Z"
		},
		"raw revision drift": func(value *FixedArtifactBundle) {
			value.Artifacts[0].Fixed.Wikitext = append([]byte{}, value.Artifacts[0].Fixed.Wikitext...)
			value.Artifacts[0].Fixed.Wikitext[0] ^= 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			mutated := cloneFixedArtifactBundle(bundle)
			mutate(&mutated)
			if draft, err := BuildDraftFromFixedArtifacts(report.UniqueComplete[0], catalogIdentity, mutated); err == nil || !reflect.DeepEqual(draft, Draft{}) {
				t.Fatalf("draft=%+v err=%v", draft, err)
			}
		})
	}
}

func fixedRevisionEvidence(provider model.LyricsSourceProvider, fixed lyricssource.FixedRevision) lyricssource.IndexEvidence {
	rawDigest := sha256.Sum256(fixed.Wikitext)
	rawSHA256 := hex.EncodeToString(rawDigest[:])
	fetchedAt := fixed.FetchedAt.UTC().Format(time.RFC3339Nano)
	origin := model.LyricsSourceOriginVocaloidFandom
	baseID := "fetch:vocaloid-fandom:" + strconv.Itoa(fixed.PageID)
	switch provider {
	case model.LyricsSourceProviderMoegirl:
		origin = model.LyricsSourceOriginMoegirl
		baseID = "search:moegirl:" + strconv.Itoa(fixed.PageID)
	case model.LyricsSourceProviderSekaipedia:
		origin = model.LyricsSourceOriginSekaipedia
		baseID = "revision:sekaipedia:" + strconv.Itoa(fixed.PageID) + ":" + strconv.Itoa(fixed.RevisionID)
	}
	evidenceID := lyricssource.MediaWikiRevisionAcquisitionEvidenceID(provider, baseID, fetchedAt, rawSHA256)
	evidence := lyricssource.IndexEvidence{
		EvidenceID: evidenceID, SHA256: rawSHA256,
		Kind: lyricssource.IndexEvidenceKindMediaWikiRevision, Provider: provider, Origin: origin,
		PageID: fixed.PageID, RevisionID: fixed.RevisionID, MediaWikiSHA1: fixed.SHA1, Title: fixed.PageTitle,
		CanonicalURL: fixed.CanonicalURL, Categories: append([]string{}, fixed.Categories...),
		FetchedAt: fetchedAt, Raw: append([]byte{}, fixed.Wikitext...),
		RawSHA256: rawSHA256,
	}
	if provider == model.LyricsSourceProviderSekaipedia && !fixed.RevisionTimestamp.IsZero() {
		evidence.RevisionTimestamp = fixed.RevisionTimestamp.UTC().Format(time.RFC3339Nano)
	}
	return evidence
}

func canonicalMoegirlRevisionURL(title string, revisionID int) string {
	canonical := url.URL{Scheme: "https", Host: "moegirl.icu", Path: "/index.php"}
	query := canonical.Query()
	query.Set("oldid", strconv.Itoa(revisionID))
	query.Set("title", title)
	canonical.RawQuery = query.Encode()
	return canonical.String()
}

func cloneFixedArtifactBundle(bundle FixedArtifactBundle) FixedArtifactBundle {
	result := bundle
	result.EvidenceReceipt.IndexEvidence = make([]lyricssource.IndexEvidence, len(bundle.EvidenceReceipt.IndexEvidence))
	for index, evidence := range bundle.EvidenceReceipt.IndexEvidence {
		result.EvidenceReceipt.IndexEvidence[index] = clonePrivateIndexEvidence(evidence)
	}
	result.Artifacts = make([]FixedArtifact, len(bundle.Artifacts))
	for index, artifact := range bundle.Artifacts {
		result.Artifacts[index] = artifact
		result.Artifacts[index].Candidate.Categories = append([]string{}, artifact.Candidate.Categories...)
		result.Artifacts[index].Candidate.IndexEvidenceRefs = append([]model.LyricsSourceIndexEvidenceRef{}, artifact.Candidate.IndexEvidenceRefs...)
		result.Artifacts[index].Fixed.Wikitext = append([]byte{}, artifact.Fixed.Wikitext...)
		result.Artifacts[index].Fixed.IndexEvidence = make([]lyricssource.IndexEvidence, len(artifact.Fixed.IndexEvidence))
		for evidenceIndex, evidence := range artifact.Fixed.IndexEvidence {
			result.Artifacts[index].Fixed.IndexEvidence[evidenceIndex] = clonePrivateIndexEvidence(evidence)
		}
	}
	return result
}
