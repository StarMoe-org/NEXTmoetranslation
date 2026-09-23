package lyricsextractionplan

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const recoveryTestCatalogPath = "/private/tmp/moesekai-lyrics-catalog-v18-20260731-704.db"

func TestCompiledRecoveryVersionsBindRenditionAwareV2Outputs(t *testing.T) {
	versions := CompiledRecoveryVersions()
	if versions.Composition != RecoveryCompositionVersionV2 ||
		versions.LineIDs != RecoveryLineIDVersionV2 ||
		versions.SongResult != RecoverySongResultVersionV2 {
		t.Fatalf("compiled recovery output versions are stale: %+v", versions)
	}
	if RecoveryCompositionVersionV1 == RecoveryCompositionVersionV2 ||
		RecoveryLineIDVersionV1 == RecoveryLineIDVersionV2 ||
		RecoverySongResultVersionV1 == RecoverySongResultVersionV2 {
		t.Fatal("historical and rendition-aware recovery output versions must remain distinct")
	}
}

func TestRecoveryPlanCanonicalRoundTripAndV1Separation(t *testing.T) {
	plan := syntheticRecoveryPlan(t)
	body, err := MarshalRecoveryCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := RecoveryCanonicalSHA256(plan)
	if err != nil {
		t.Fatal(err)
	}
	const wantDigest = "a10ba9a53967fecabbda713b9d85b66ab391b60cfd933a3a0993a993fe2f0be5"
	if digest != wantDigest {
		t.Fatalf("synthetic recovery canonical digest=%s want=%s", digest, wantDigest)
	}
	decoded, actual, err := CheckRecovery(body, digest)
	if err != nil || actual != digest || decoded.PlanID != plan.PlanID {
		t.Fatalf("recovery round trip digest=%q actual=%q err=%v", digest, actual, err)
	}
	if _, err := DecodeCanonical(body); err == nil {
		t.Fatal("plan-v1 decoder accepted recovery-v2 canonical bytes")
	}
	legacy := mustCanonicalPlan(t, syntheticPlan(t))
	if _, err := DecodeRecoveryCanonical(legacy); err == nil {
		t.Fatal("recovery-v2 decoder accepted plan-v1 canonical bytes")
	}
}

func TestRecoveryPlanValidatesHistoricalRubyVersionArtifactsWithoutAdmittingMixedAlgorithms(t *testing.T) {
	plan := syntheticRecoveryPlan(t)
	historical, err := historicalRecoveryVersionsV1(nil)
	if err != nil {
		t.Fatal(err)
	}
	plan.Versions = historical
	body, err := MarshalRecoveryCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(body)
	decoded, actual, err := CheckRecovery(body, hex.EncodeToString(digest[:]))
	if err != nil || actual != hex.EncodeToString(digest[:]) || !reflect.DeepEqual(decoded.Versions, historical) {
		t.Fatalf("historical recovery plan digest=%q versions=%+v err=%v", actual, decoded.Versions, err)
	}

	omitted := syntheticRecoveryPlan(t)
	for index := range omitted.Versions.Parsers {
		omitted.Versions.Parsers[index].RubyGeneratorVersion = ""
	}
	if err := ValidateRecovery(omitted); err != nil {
		t.Fatalf("pre-versioning immutable recovery plan was rejected: %v", err)
	}

	mixed := syntheticRecoveryPlan(t)
	mixed.Versions.Parsers[0].RubyGeneratorVersion = historicalRegisteredSekaipediaRuby
	if err := ValidateRecovery(mixed); err == nil {
		t.Fatal("mixed historical/current recovery ruby algorithms were accepted")
	}
}

func TestRecoveryPlanAcceptsSekaipediaOnlyAndRejectsGappedProviderChains(t *testing.T) {
	plan := syntheticRecoveryPlan(t)
	plan.Providers.Order = append([]Provider(nil), plan.Providers.Order[:1]...)
	plan.Providers.Configurations = append([]RecoveryProviderPlan(nil), plan.Providers.Configurations[:1]...)
	if err := ValidateRecovery(plan); err != nil {
		t.Fatalf("Sekaipedia-only recovery plan was rejected: %v", err)
	}

	for name, order := range map[string][]Provider{
		"Sekaipedia then Fandom": {ProviderSekaipedia, ProviderVocaloidFandom},
		"Moegirl only":           {ProviderMoegirl},
		"reordered full chain":   {ProviderMoegirl, ProviderSekaipedia, ProviderVocaloidFandom},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := syntheticRecoveryPlan(t)
			candidate.Providers.Order = append([]Provider(nil), order...)
			candidate.Providers.Configurations = make([]RecoveryProviderPlan, len(order))
			for index, provider := range order {
				for _, configured := range syntheticRecoveryPlan(t).Providers.Configurations {
					if configured.Provider == provider {
						candidate.Providers.Configurations[index] = configured
						break
					}
				}
			}
			if err := ValidateRecovery(candidate); err == nil {
				t.Fatal("gapped or reordered provider chain was accepted")
			}
		})
	}
}

func TestRecoveryPlanProviderScopesPartitionSongsWithoutFallback(t *testing.T) {
	plan := syntheticRecoveryPlan(t)
	plan.Providers.Order = append([]Provider(nil), plan.Providers.Order[:2]...)
	plan.Providers.Configurations = append([]RecoveryProviderPlan(nil), plan.Providers.Configurations[:2]...)
	plan.Providers.Configurations[0].MusicIDs = []int{2}
	plan.Providers.Configurations[0].SekaipediaTargets = append(
		[]RecoverySekaipediaPageTarget(nil), plan.Providers.Configurations[0].SekaipediaTargets[:1]...,
	)
	plan.Providers.Configurations[1].MusicIDs = []int{235}
	if err := ValidateRecovery(plan); err != nil {
		t.Fatalf("exact provider music-ID partition was rejected: %v", err)
	}
	sekaipedia, err := EffectiveRecoveryProviderOrder(plan.Providers, 2)
	if err != nil || !reflect.DeepEqual(sekaipedia, []Provider{ProviderSekaipedia}) {
		t.Fatalf("music 2 effective provider order=%v err=%v", sekaipedia, err)
	}
	moegirl, err := EffectiveRecoveryProviderOrder(plan.Providers, 235)
	if err != nil || !reflect.DeepEqual(moegirl, []Provider{ProviderMoegirl}) {
		t.Fatalf("music 235 effective provider order=%v err=%v", moegirl, err)
	}

	for name, mutate := range map[string]func(*RecoveryPlan){
		"overlap": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[1].MusicIDs = []int{2, 235}
		},
		"gap": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[1].MusicIDs = []int{2}
		},
		"mixed scoped and unscoped": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[1].MusicIDs = nil
		},
		"Sekaipedia target outside provider scope": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[0].SekaipediaTargets = append(
				candidate.Providers.Configurations[0].SekaipediaTargets,
				RecoverySekaipediaPageTarget{MusicID: 235, PageTitle: "Journey"},
			)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneRecoveryPlan(plan)
			mutate(&candidate)
			if err := ValidateRecovery(candidate); err == nil {
				t.Fatal("invalid provider music-ID scope was accepted")
			}
		})
	}
}

func TestRecoveryPlanExactPublicMoegirlUsesCompleteZhURLAndNoICUAuthority(t *testing.T) {
	plan := syntheticRecoveryPlan(t)
	plan.Providers.Order = []Provider{ProviderSekaipedia, ProviderMoegirlPublicExact}
	sekaipedia := plan.Providers.Configurations[0]
	sekaipedia.MusicIDs = []int{2}
	sekaipedia.SekaipediaTargets = append(
		[]RecoverySekaipediaPageTarget(nil), sekaipedia.SekaipediaTargets[:1]...,
	)
	exact := RecoveryProviderPlan{
		Provider: ProviderMoegirlPublicExact, Mode: ProviderModeActive,
		CrawlDelayMillis: CompiledSafetyFloors().ProviderCrawlDelayMillis,
		CacheTTLMillis:   CompiledSafetyFloors().ProviderCacheTTLMillis,
		MusicIDs:         []int{235}, Authorities: []FixedAuthority{},
		ContributorAliases: []RecoveryContributorAlias{},
		ExactPublicTargets: []RecoveryExactPublicPageTarget{{
			MusicID:   235,
			PageURL:   "https://zh.moegirl.org.cn/%E4%BA%BF%E5%B9%B4%E7%88%B1%E6%81%8B",
			PageTitle: "亿年爱恋", JapaneseTitle: "一億年恋してる",
			PageID: 649688, RevisionID: 8500224, FetchedAt: "2026-07-31T23:59:00Z",
			RawHTML: RecoveryFileBinding{
				Path: "/private/tmp/moesekai-exact-public/response.html", SizeBytes: 128236,
				SHA256: strings.Repeat("a", 64),
			},
			ExtractionReport: RecoveryFileBinding{
				Path: "/private/tmp/moesekai-exact-public/report.json", SizeBytes: 6344,
				SHA256: strings.Repeat("b", 64),
			},
		}},
	}
	plan.Providers.Configurations = []RecoveryProviderPlan{sekaipedia, exact}
	versions, err := CompiledScopedRecoveryVersions(plan.Providers.Order)
	if err != nil {
		t.Fatal(err)
	}
	plan.Versions = versions
	if err := ValidateRecovery(plan); err != nil {
		t.Fatalf("exact complete public URL plan was rejected: %v", err)
	}
	order, err := EffectiveRecoveryProviderOrder(plan.Providers, 235)
	if err != nil || !reflect.DeepEqual(order, []Provider{ProviderMoegirlPublicExact}) {
		t.Fatalf("exact public provider order=%v err=%v", order, err)
	}

	for name, mutate := range map[string]func(*RecoveryPlan){
		"ICU URL": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[1].ExactPublicTargets[0].PageURL = "https://moegirl.icu/api.php"
		},
		"API authority": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[1].Authorities = []FixedAuthority{syntheticRecoveryPlan(t).Providers.Configurations[1].Authorities[0]}
		},
		"guessed title": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[1].ExactPublicTargets[0].PageTitle = "一億年恋してる"
		},
		"missing complete target": func(candidate *RecoveryPlan) {
			candidate.Providers.Configurations[1].ExactPublicTargets = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneRecoveryPlan(plan)
			mutate(&candidate)
			if err := ValidateRecovery(candidate); err == nil {
				t.Fatal("invalid exact public Moegirl plan was accepted")
			}
		})
	}
}

func TestRecoveryPlanRejectsHostileJSONBoundaries(t *testing.T) {
	body, err := MarshalRecoveryCanonical(syntheticRecoveryPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	deep := []byte(`{"adversarial":` + strings.Repeat("[", MaxPlanJSONDepth+1) + "0" + strings.Repeat("]", MaxPlanJSONDepth+1) + `}`)
	for name, mutated := range map[string][]byte{
		"unknown":         bytes.Replace(body, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"command":"run"`), 1),
		"duplicate":       bytes.Replace(body, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1),
		"trailing value":  append(append([]byte(nil), body...), []byte(`{}`)...),
		"trailing space":  append(append([]byte(nil), body...), ' '),
		"invalid UTF-8":   append([]byte{0xff}, body...),
		"excessive depth": deep,
		"oversized":       bytes.Repeat([]byte{' '}, MaxPlanBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeRecoveryCanonical(mutated); err == nil {
				t.Fatal("hostile recovery plan JSON was accepted")
			}
			if _, err := DecodeRecoveryCanonicalForInspection(mutated); err == nil {
				t.Fatal("inspection decoder weakened hostile recovery plan JSON boundaries")
			}
		})
	}
}

func TestRecoveryPlanAliasesAreSekaipediaOnlyScopedBoundedAndCanonical(t *testing.T) {
	for name, mutate := range map[string]func(*RecoveryPlan){
		"wrong provider": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[1].ContributorAliases = append(
				plan.Providers.Configurations[1].ContributorAliases,
				RecoveryContributorAlias{MusicID: 2, CatalogContributor: "catalog", ProviderContributor: "provider"},
			)
		},
		"outside scope": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].ContributorAliases[0].MusicID = 3
		},
		"duplicate": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].ContributorAliases = append(
				plan.Providers.Configurations[0].ContributorAliases,
				plan.Providers.Configurations[0].ContributorAliases[0],
			)
		},
		"same identity": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].ContributorAliases[0].ProviderContributor = "みきとP"
		},
		"unbounded": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].ContributorAliases[0].ProviderContributor = strings.Repeat("x", MaxRecoveryAliasBytes+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticRecoveryPlan(t)
			mutate(&plan)
			if err := ValidateRecovery(plan); err == nil {
				t.Fatal("invalid recovery contributor alias was accepted")
			}
		})
	}
}

func TestRecoveryPlanSekaipediaTargetsAreExactScopedAndCanonical(t *testing.T) {
	if err := ValidateRecovery(syntheticRecoveryPlan(t)); err != nil {
		t.Fatalf("exact Sekaipedia target map was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*RecoveryPlan){
		"missing scope target": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].SekaipediaTargets = plan.Providers.Configurations[0].SekaipediaTargets[:1]
		},
		"duplicate page title": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].SekaipediaTargets[1].PageTitle = "Roki"
		},
		"Japanese-title URL guess": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].SekaipediaTargets[0].PageTitle = "ロキ#guess"
		},
		"wrong provider": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[1].SekaipediaTargets = []RecoverySekaipediaPageTarget{{MusicID: 2, PageTitle: "Roki"}, {MusicID: 235, PageTitle: "Journey"}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticRecoveryPlan(t)
			mutate(&plan)
			if err := ValidateRecovery(plan); err == nil {
				t.Fatal("invalid Sekaipedia target map was accepted")
			}
		})
	}
}

func TestRecoveryPlanAcceptsReviewedFutureSekaipediaAuthorityWithoutChangingV1(t *testing.T) {
	plan := syntheticRecoveryPlan(t)
	future := recoveryAuthority(t, ProviderSekaipedia, 9001, 9002, "Future song index", strings.Repeat("a", 40), strings.Repeat("c", 64), strings.Repeat("b", 64))
	plan.Providers.Configurations[0].Authorities = []FixedAuthority{future}
	plan.SekaipediaCanary.List = RecoverySekaipediaCanaryRevision{
		AcquisitionID: strings.Repeat("d", 64),
		PageID:        future.PageID, RevisionID: future.RevisionID, RevisionTimestamp: future.RevisionTimestamp,
		SHA1: future.SHA1, ContentSHA256: future.ContentSHA256, RawResponseSHA256: future.RawSHA256,
	}
	if err := ValidateRecovery(plan); err != nil {
		t.Fatalf("future reviewed recovery authority was rejected: %v", err)
	}
	legacy := syntheticPlan(t)
	if err := Validate(legacy); err != nil {
		t.Fatalf("plan-v1 canonical meaning drifted: %v", err)
	}
}

func TestRecoveryPlanRejectsArbitraryAndCrossProviderAuthorities(t *testing.T) {
	for name, mutate := range map[string]func(*RecoveryPlan){
		"cross-provider canonical URL": func(plan *RecoveryPlan) {
			authority := &plan.Providers.Configurations[0].Authorities[0]
			authority.CanonicalURL = FixedAuthorityCanonicalURL(
				ProviderMoegirl, authority.Title, authority.RevisionID,
			)
		},
		"cross-provider evidence ID": func(plan *RecoveryPlan) {
			authority := &plan.Providers.Configurations[0].Authorities[0]
			authority.EvidenceID, _ = FixedAuthorityEvidenceID(
				ProviderMoegirl, authority.Role, authority.PageID, authority.RevisionID, authority.Title,
			)
		},
		"arbitrary capture profile": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].Authorities[0].CaptureProfile = CaptureProfileMediaWikiRevisionContentV1
		},
		"missing recovery content identity": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[0].Authorities[0].ContentSHA256 = ""
		},
		"fandom fixed authority": func(plan *RecoveryPlan) {
			plan.Providers.Configurations[2].Authorities = []FixedAuthority{
				recoveryAuthority(t, ProviderSekaipedia, 9001, 9002, "Arbitrary index", strings.Repeat("a", 40), strings.Repeat("c", 64), strings.Repeat("b", 64)),
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticRecoveryPlan(t)
			mutate(&plan)
			if err := ValidateRecovery(plan); err == nil {
				t.Fatal("arbitrary or cross-provider authority was accepted")
			}
		})
	}
}

func TestRecoveryPlanRequiresCanonicalPrivateTmpPaths(t *testing.T) {
	for name, mutate := range map[string]func(*RecoveryPlan){
		"relative catalog":   func(plan *RecoveryPlan) { plan.Catalog.Path = "inputs/catalog.db" },
		"production catalog": func(plan *RecoveryPlan) { plan.Catalog.Path = "/private/tmp/production/catalog.db" },
		"catalog alias":      func(plan *RecoveryPlan) { plan.Catalog.Path = "/private/tmp/a/../catalog.db" },
		"database output":    func(plan *RecoveryPlan) { plan.Outputs.RootManifest = "/private/tmp/recovery/root.sqlite" },
		"nested output":      func(plan *RecoveryPlan) { plan.Outputs.SongResults = plan.Outputs.ProviderOutcomes + "/songs" },
	} {
		t.Run(name, func(t *testing.T) {
			plan := syntheticRecoveryPlan(t)
			mutate(&plan)
			if err := ValidateRecovery(plan); err == nil {
				t.Fatal("invalid recovery path was accepted")
			}
		})
	}
}

func TestRecoveryPlanSekaipediaCanaryIsExactPlanDataAndLegacyOptional(t *testing.T) {
	plan := syntheticRecoveryPlan(t)
	if err := ValidateRecovery(plan); err != nil {
		t.Fatal(err)
	}
	canonical, err := MarshalRecoveryCanonical(plan)
	if err != nil {
		t.Fatal(err)
	}
	exactField := []byte(`"acquisitionId":"` + HistoricalSekaipediaListAcquisitionID + `",`)
	if bytes.Count(canonical, exactField) != 1 {
		t.Fatalf("canonical plan does not carry the exact historical List replay ID once: %s", canonical)
	}
	historicalPlan := cloneRecoveryPlan(plan)
	historicalPlan.SekaipediaCanary.List.AcquisitionID = ""
	historicalPlan.SekaipediaCanary.List.ContentSHA256 = ""
	historicalPlan.Providers.Configurations[0].Authorities[0].ContentSHA256 = ""
	legacyCanary, err := json.Marshal(historicalPlan)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(legacyCanary, []byte(`"acquisitionId"`)) ||
		bytes.Contains(legacyCanary, []byte(`"contentSha256":"`+plan.Providers.Configurations[0].Authorities[0].ContentSHA256+`"`)) {
		t.Fatalf("historical inspection fixture retained a new List authority field: %s", legacyCanary)
	}
	if _, err := DecodeRecoveryCanonical(legacyCanary); err == nil {
		t.Fatal("operational decoder accepted a historical plan without an exact replay acquisition ID")
	}
	inspected, err := DecodeRecoveryCanonicalForInspection(legacyCanary)
	if err != nil || inspected.SekaipediaCanary == nil || inspected.SekaipediaCanary.List.AcquisitionID != "" {
		t.Fatalf("historical recovery plan inspection=%+v err=%v", inspected.SekaipediaCanary, err)
	}
	legacyDigest := sha256.Sum256(legacyCanary)
	if checked, actual, err := CheckRecoveryForInspection(legacyCanary, hex.EncodeToString(legacyDigest[:])); err != nil ||
		actual != hex.EncodeToString(legacyDigest[:]) || checked.PlanID != plan.PlanID {
		t.Fatalf("historical recovery plan inspection digest=%q checked=%+v err=%v", actual, checked, err)
	}
	if err := ValidateRecoveryForInspection(inspected); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRecovery(inspected); err == nil {
		t.Fatal("inspection-only historical plan became operationally valid")
	}
	if _, err := MarshalRecoveryCanonical(inspected); err == nil {
		t.Fatal("inspection-only historical plan was re-encoded as newly generated immutable plan data")
	}
	withoutCanary := plan
	withoutCanary.SekaipediaCanary = nil
	body, err := MarshalRecoveryCanonical(withoutCanary)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"sekaipediaCanary"`)) {
		t.Fatal("offline-compatible plan encoded an absent live-canary identity")
	}

	for name, mutate := range map[string]func(*RecoveryPlan){
		"list missing exact replay acquisition ID": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.List.AcquisitionID = ""
		},
		"list uppercase exact replay acquisition ID": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.List.AcquisitionID = strings.ToUpper(candidate.SekaipediaCanary.List.AcquisitionID)
		},
		"list revision drift": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.List.RevisionID++
		},
		"list raw drift": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.List.RawResponseSHA256 = strings.Repeat("0", 64)
		},
		"list missing content identity": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.List.ContentSHA256 = ""
		},
		"song music mismatch": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.Songs[0].MusicID = 235
		},
		"song noncanonical timestamp": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.Songs[0].RevisionTimestamp = "2026-07-15T07:59:12+00:00"
		},
		"song missing raw identity": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.Songs[0].RawResponseSHA256 = ""
		},
		"song missing content identity": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.Songs[0].ContentSHA256 = ""
		},
		"song count mismatch": func(candidate *RecoveryPlan) {
			candidate.SekaipediaCanary.Songs = append(candidate.SekaipediaCanary.Songs, candidate.SekaipediaCanary.Songs[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneRecoveryPlan(plan)
			mutate(&candidate)
			if err := ValidateRecovery(candidate); err == nil {
				t.Fatal("drifted exact Sekaipedia canary plan was accepted")
			}
		})
	}
}

func TestRecoveryPlanCanInspectPinnedHistoricalPlanWithoutMakingItOperational(t *testing.T) {
	path := os.Getenv("MOESEKAI_RECOVERY_PLAN_INSPECTION_TEST_PATH")
	expected := os.Getenv("MOESEKAI_RECOVERY_PLAN_INSPECTION_TEST_SHA256")
	if path == "" && expected == "" {
		t.Skip("pinned historical recovery plan inspection not requested")
	}
	if path == "" || !canonicalSHA256.MatchString(expected) {
		t.Fatal("historical recovery plan inspection requires an exact path and lowercase SHA-256 pin")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, actual, err := CheckRecoveryForInspection(body, expected)
	if err != nil {
		var raw RecoveryPlan
		decodeErr := json.Unmarshal(body, &raw)
		canonical, encodeErr := json.Marshal(raw)
		difference := -1
		for index := 0; index < len(body) && index < len(canonical); index++ {
			if body[index] != canonical[index] {
				difference = index
				break
			}
		}
		if difference < 0 && len(body) != len(canonical) {
			difference = min(len(body), len(canonical))
		}
		start := max(0, difference-80)
		bodyEnd := min(len(body), difference+160)
		canonicalEnd := min(len(canonical), difference+160)
		t.Fatalf("historical recovery plan inspection actual=%q err=%v rawDecode=%v reencode=%v firstDifference=%d bodyBytes=%d canonicalBytes=%d bodyWindow=%q canonicalWindow=%q",
			actual, err, decodeErr, encodeErr, difference, len(body), len(canonical), body[start:bodyEnd], canonical[start:canonicalEnd])
	}
	if actual != expected || plan.SekaipediaCanary == nil || plan.SekaipediaCanary.List.AcquisitionID != "" {
		t.Fatalf("historical recovery plan inspection actual=%q canary=%+v", actual, plan.SekaipediaCanary)
	}
	if _, _, err := CheckRecovery(body, expected); err == nil {
		t.Fatal("pinned historical recovery plan became operationally decodable")
	}
	if err := ValidateRecovery(plan); err == nil {
		t.Fatal("pinned historical recovery plan became operationally valid")
	}
	if _, err := MarshalRecoveryCanonical(plan); err == nil {
		t.Fatal("pinned historical recovery plan was accepted by the new immutable-plan encoder")
	}
}

func syntheticRecoveryPlan(t *testing.T) RecoveryPlan {
	t.Helper()
	floors := CompiledSafetyFloors()
	sekaipedia := recoveryAuthority(
		t, ProviderSekaipedia, 268, 335193, "List of songs",
		"b216a827f88c59f5e954a120027832fe9cd74413",
		"aaddff2922548aab7e522124ff2bad86427501930d549c9d94c9b4e473c35f92",
		"c21e31c36f8e7d7534af1617d5b737a1662decd40c34c9e7d4aab71b103ef8dd",
	)
	moegirlBody := []byte("* [[Other#Other|Other]]\n")
	moegirlSHA := sha1.Sum(moegirlBody)
	moegirl := recoveryAuthority(t, ProviderMoegirl, 488279, 8073049,
		"世界计划 彩色舞台 feat. 初音未来/歌曲", hex.EncodeToString(moegirlSHA[:]), "", "")
	sourceFiles := []SourceFileIdentity{{
		Path: "server/internal/lyricsrecovery/config.go", SizeBytes: 1, SHA256: strings.Repeat("f", 64),
	}}
	sourceSnapshotSHA, err := RecoverySourceSnapshotSHA256(sourceFiles)
	if err != nil {
		t.Fatal(err)
	}
	return RecoveryPlan{
		SchemaVersion: RecoverySchemaVersionV2, CanonicalEncoding: RecoveryCanonicalEncodingV2,
		DigestAlgorithm: RecoveryDigestAlgorithmV2, PlanID: "recovery-test-plan", CreatedAt: "2026-08-02T00:00:00Z",
		Catalog: RecoveryCatalogBinding{
			Path: recoveryTestCatalogPath, SizeBytes: 1_150_976,
			SourceSHA256:  "58626dcd03a8bc06ffa1e1c8fba3cfa6dea0560fb471abd802829b4a7d6dd7f4",
			SchemaVersion: CatalogSchemaVersion, RuntimeSchemaVersion: MaximumCatalogRuntimeSchema, RecordCount: 704,
			IdentityPolicyVersion: CompiledEffectiveVersions().Policies.CatalogIdentity,
			IdentitySHA256:        "a17efa8a7c5e6c533d2502f01fccd7c5ddf9cd68bb28a489b7f7f6552e127fe2",
			MusicIDsSHA256:        "510da78c96ff21ac6f200dbfc3054be326c081d3fd0876d12ae3557d49188fa1",
		},
		SourceSnapshot: SourceSnapshot{
			Algorithm: RecoverySourceSnapshotAlgorithmV2, CapturedAt: "2026-08-02T00:00:00Z",
			Files: sourceFiles, SHA256: sourceSnapshotSHA,
		},
		Scope: RecoveryScopeBinding{
			Kind: RecoveryScopePartial, ScopeID: "catalog-704", MusicIDs: []int{2, 235},
			SupersedesRootID: "parent-root", SupersedesRootSHA256: strings.Repeat("d", 64),
		},
		Providers: RecoveryProviderConfiguration{
			Order: []Provider{ProviderSekaipedia, ProviderMoegirl, ProviderVocaloidFandom},
			Configurations: []RecoveryProviderPlan{
				{Provider: ProviderSekaipedia, Mode: ProviderModeActive, CrawlDelayMillis: floors.ProviderCrawlDelayMillis,
					CacheTTLMillis: floors.ProviderCacheTTLMillis, Authorities: []FixedAuthority{sekaipedia},
					ContributorAliases: []RecoveryContributorAlias{{MusicID: 2, CatalogContributor: "みきとP", ProviderContributor: "MikitoP"}},
					SekaipediaTargets:  []RecoverySekaipediaPageTarget{{MusicID: 2, PageTitle: "Roki"}, {MusicID: 235, PageTitle: "Journey"}}},
				{Provider: ProviderMoegirl, Mode: ProviderModeActive, CrawlDelayMillis: floors.ProviderCrawlDelayMillis,
					CacheTTLMillis: floors.ProviderCacheTTLMillis, Authorities: []FixedAuthority{moegirl}, ContributorAliases: []RecoveryContributorAlias{}},
				{Provider: ProviderVocaloidFandom, Mode: ProviderModeActive, CrawlDelayMillis: floors.ProviderCrawlDelayMillis,
					CacheTTLMillis: floors.ProviderCacheTTLMillis, Authorities: []FixedAuthority{}, ContributorAliases: []RecoveryContributorAlias{}},
			},
		},
		Versions: CompiledRecoveryVersions(),
		Execution: RecoveryExecutionSettings{
			MaxAttempts: 1, RequestTimeoutMillis: 30_000, RetryDelayMillis: floors.RetryDelayMillis,
			ProviderResponseBytes:    CompiledHardCeilings().ProviderResponseBytes,
			MaxActualNetworkInFlight: RecoveryMaxActualInFlight, MediaWikiMaxlag: RecoveryRequiredMaxlag,
			LiveCanaryMusicIDs: []int{2},
		},
		SekaipediaCanary: &RecoverySekaipediaCanaryPlan{
			List: RecoverySekaipediaCanaryRevision{
				AcquisitionID: HistoricalSekaipediaListAcquisitionID,
				PageID:        sekaipedia.PageID, RevisionID: sekaipedia.RevisionID,
				RevisionTimestamp: sekaipedia.RevisionTimestamp, SHA1: sekaipedia.SHA1,
				ContentSHA256: sekaipedia.ContentSHA256, RawResponseSHA256: sekaipedia.RawSHA256,
			},
			Songs: []RecoverySekaipediaCanarySong{{
				MusicID: 2, CatalogTitle: "ロキ", ProviderTitle: "Roki", PageID: 398, RevisionID: 330574,
				RevisionTimestamp: "2026-07-15T07:59:12Z", SHA1: "29198603574701b81b34198e63343930abd3d9a2",
				ContentSHA256:     "3f57e7a5cfabf6d9997a2392d8f52fe40b13b95af1312c3f8857e13f405c3ebd",
				RawResponseSHA256: "cc44c089e8704019f390c15084a4882c366c0c0ba30c082496f8f6387e662360",
			}},
		},
		Outputs: RequiredRecoveryOutputs([6]string{
			"/private/tmp/recovery-v2-test/ledger", "/private/tmp/recovery-v2-test/acquisition-set.json",
			"/private/tmp/recovery-v2-test/provider-outcomes", "/private/tmp/recovery-v2-test/song-results",
			"/private/tmp/recovery-v2-test/evidence-pack", "/private/tmp/recovery-v2-test/root.json",
		}),
		Deployment: RequiredDeploymentPolicy(),
	}
}

func recoveryAuthority(t *testing.T, provider Provider, pageID, revisionID int, title, contentSHA1, contentSHA256, rawSHA256 string) FixedAuthority {
	t.Helper()
	authority := FixedAuthority{
		Disposition: AuthorityActive, Role: AuthorityRoleSongIndex, PageID: pageID, RevisionID: revisionID,
		SHA1: contentSHA1, ContentSHA256: contentSHA256, RawSHA256: rawSHA256, Title: title,
		CanonicalURL: FixedAuthorityCanonicalURL(provider, title, revisionID),
	}
	if provider == ProviderSekaipedia {
		authority.CaptureProfile = CaptureProfileMediaWikiAPIRevisionResponseV1
		authority.RevisionTimestamp = "2026-07-27T16:29:13Z"
	} else {
		authority.CaptureProfile = CaptureProfileMediaWikiRevisionContentV1
	}
	var err error
	authority.EvidenceID, err = FixedAuthorityEvidenceID(provider, authority.Role, pageID, revisionID, title)
	if err != nil {
		t.Fatal(err)
	}
	return authority
}
