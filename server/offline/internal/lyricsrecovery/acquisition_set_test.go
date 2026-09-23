package lyricsrecovery

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"moesekai/server/internal/lyricsprovideroutcome"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

func TestAcquisitionSetStrictCanonicalTerminalAndTamperChecks(t *testing.T) {
	set := testClosedAcquisitionSet(t)
	body, err := MarshalAcquisitionSet(set)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAcquisitionSet(body)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := MarshalAcquisitionSet(decoded)
	if err != nil || !bytes.Equal(body, roundTrip) {
		t.Fatalf("acquisition set canonical round trip drifted: err=%v", err)
	}

	hostile := map[string][]byte{
		"duplicate": bytes.Replace(body, []byte(`"schemaVersion":2`), []byte(`"schemaVersion":2,"schemaVersion":2`), 1),
		"unknown":   bytes.Replace(body, []byte(`{"schemaVersion"`), []byte(`{"unknown":1,"schemaVersion"`), 1),
		"trailing":  append(append([]byte{}, body...), []byte("\n{}")...),
		"utf8":      append(append([]byte{}, body...), 0xff),
		"depth":     []byte(`{"x":[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[[1]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]]}`),
	}
	for name, candidate := range hostile {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeAcquisitionSet(candidate); err == nil {
				t.Fatal("hostile acquisition set was accepted")
			}
		})
	}
	if _, err := DecodeAcquisitionSet(bytes.Repeat([]byte{'x'}, MaxAcquisitionSetBytes+1)); err == nil {
		t.Fatal("oversized acquisition set was accepted")
	}

	tampered := set
	tampered.PlanID = "tampered-plan"
	if err := ValidateAcquisitionSet(tampered); err == nil {
		t.Fatal("tampered acquisition set retained a valid digest")
	}

	invalidTerminal := cloneAcquisitionSet(set)
	invalidTerminal.Songs[0].Providers[0].Status = lyricsprovideroutcome.StatusCandidate
	if err := ValidateAcquisitionSet(invalidTerminal); err == nil {
		t.Fatal("invalid candidate terminal was accepted")
	}
	duplicateID := cloneAcquisitionSet(set)
	duplicateID.Songs[0].Providers[0].AcquisitionIDs = []lyricsacquisition.AcquisitionID{
		lyricsacquisition.AcquisitionID(strings.Repeat("1", 64)),
		lyricsacquisition.AcquisitionID(strings.Repeat("1", 64)),
	}
	duplicateID.SetSHA256 = ""
	if err := validateAcquisitionSet(duplicateID, false); err == nil {
		t.Fatal("duplicate exact acquisition IDs were accepted")
	}
	uppercase := cloneAcquisitionSet(set)
	uppercase.SetSHA256 = ""
	uppercase.PlanSHA256 = strings.Repeat("A", 64)
	if err := validateAcquisitionSet(uppercase, false); err == nil {
		t.Fatal("noncanonical uppercase plan SHA-256 was accepted")
	}

	crossSong := cloneAcquisitionSet(set)
	crossSong.SetSHA256 = ""
	crossSong.Songs[0].Providers[0].AcquisitionIDs = []lyricsacquisition.AcquisitionID{
		lyricsacquisition.AcquisitionID(strings.Repeat("1", 64)),
	}
	second := crossSong.Songs[0]
	second.MusicID = 3
	second.Providers = cloneProviderAcquisitionSets(second.Providers)
	crossSong.Songs = append(crossSong.Songs, second)
	if err := validateAcquisitionSet(crossSong, false); err == nil {
		t.Fatal("one exact acquisition ID was authorized for multiple songs")
	}
}

func TestAcquisitionSetRejectsEmptyReorderedDuplicatedAndExtraProviders(t *testing.T) {
	order := testProviderOrder()
	terminal := func(provider model.LyricsSourceProvider) ProviderAcquisitionSet {
		return ProviderAcquisitionSet{
			Provider: provider, AcquisitionIDs: []lyricsacquisition.AcquisitionID{},
			Status: lyricsprovideroutcome.StatusNoMatch, ReasonCode: lyricsprovideroutcome.ReasonNoSearchHits,
			Phase: lyricsprovideroutcome.PhaseResolveTargets, Counts: lyricsprovideroutcome.Counts{NoMatch: 1},
		}
	}
	for name, providers := range map[string][]ProviderAcquisitionSet{
		"empty":            {},
		"reordered":        {terminal(order[1]), terminal(order[0])},
		"duplicated":       {terminal(order[0]), terminal(order[0])},
		"unknown provider": {terminal(model.LyricsSourceProvider("unknown"))},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewAcquisitionSet("test-recovery-v2", strings.Repeat("a", 64), order, []SongAcquisitionSet{{
				MusicID: 2, Providers: providers,
			}}); err == nil {
				t.Fatal("invalid evaluated provider prefix was accepted")
			}
		})
	}
	if _, err := NewAcquisitionSet("test-recovery-v2", strings.Repeat("a", 64), order[:2], []SongAcquisitionSet{{
		MusicID: 2, Providers: []ProviderAcquisitionSet{terminal(order[0]), terminal(order[1]), terminal(order[2])},
	}}); err == nil {
		t.Fatal("provider outside the immutable plan order was accepted")
	}
}

func TestAcquisitionSetScopedSelectionRequiresExactPlanAuthorization(t *testing.T) {
	order := testProviderOrder()[:2]
	terminal := ProviderAcquisitionSet{
		Provider: order[1], AcquisitionIDs: []lyricsacquisition.AcquisitionID{},
		Status: lyricsprovideroutcome.StatusNoMatch, ReasonCode: lyricsprovideroutcome.ReasonNoSearchHits,
		Phase: lyricsprovideroutcome.PhaseResolveTargets, Counts: lyricsprovideroutcome.Counts{NoMatch: 1},
	}
	set, err := NewAcquisitionSet(
		"test-recovery-v2", strings.Repeat("a", 64), order,
		[]SongAcquisitionSet{{MusicID: 795, Providers: []ProviderAcquisitionSet{terminal}}},
	)
	if err != nil {
		t.Fatalf("scoped non-prefix selection was rejected structurally: %v", err)
	}
	if err := ValidateAcquisitionSetAuthorization(
		set, set.PlanID, set.PlanSHA256, []int{795}, order, nil,
	); err == nil {
		t.Fatal("scoped non-prefix selection was accepted without provider music-ID authorization")
	}
	scopes := map[model.LyricsSourceProvider][]int{order[0]: {2}, order[1]: {795}}
	if err := ValidateAcquisitionSetAuthorization(
		set, set.PlanID, set.PlanSHA256, []int{795}, order, scopes,
	); err != nil {
		t.Fatalf("exact scoped provider assignment was rejected: %v", err)
	}
}

func TestAcquisitionSetAuthorizationRequiresExactPlanScopeAndProviderOrder(t *testing.T) {
	set := testClosedAcquisitionSet(t)
	order := testProviderOrder()
	if err := ValidateAcquisitionSetAuthorization(set, set.PlanID, set.PlanSHA256, []int{2}, order, nil); err != nil {
		t.Fatal(err)
	}
	for name, musicIDs := range map[string][]int{
		"subset": {}, "superset": {2, 3}, "wrong": {3}, "duplicate": {2, 2},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateAcquisitionSetAuthorization(set, set.PlanID, set.PlanSHA256, musicIDs, order, nil); err == nil {
				t.Fatal("non-exact acquisition authorization scope was accepted")
			}
		})
	}
	if err := ValidateAcquisitionSetAuthorization(set, "other-plan", set.PlanSHA256, []int{2}, order, nil); err == nil {
		t.Fatal("cross-plan acquisition set was accepted")
	}
	if err := ValidateAcquisitionSetAuthorization(set, set.PlanID, set.PlanSHA256, []int{2}, []model.LyricsSourceProvider{
		order[1], order[0], order[2],
	}, nil); err == nil {
		t.Fatal("reordered immutable provider plan was accepted")
	}
}

func TestAcquisitionSetAuthorizationAcceptsExactVariablePlanPrefixes(t *testing.T) {
	order := testProviderOrder()
	noMatch := func(provider model.LyricsSourceProvider) ProviderAcquisitionSet {
		return ProviderAcquisitionSet{
			Provider: provider, AcquisitionIDs: []lyricsacquisition.AcquisitionID{},
			Status: lyricsprovideroutcome.StatusNoMatch, ReasonCode: lyricsprovideroutcome.ReasonNoSearchHits,
			Phase: lyricsprovideroutcome.PhaseResolveTargets, Counts: lyricsprovideroutcome.Counts{NoMatch: 1},
		}
	}
	candidate := func(provider model.LyricsSourceProvider, digit string) ProviderAcquisitionSet {
		return ProviderAcquisitionSet{
			Provider: provider,
			AcquisitionIDs: []lyricsacquisition.AcquisitionID{
				lyricsacquisition.AcquisitionID(strings.Repeat(digit, 64)),
			},
			Status: lyricsprovideroutcome.StatusCandidate, ReasonCode: lyricsprovideroutcome.ReasonCandidate,
			Phase:  lyricsprovideroutcome.PhaseFinalize,
			Counts: lyricsprovideroutcome.Counts{Acquisitions: 1, Candidates: 1},
		}
	}
	songs := []SongAcquisitionSet{
		{MusicID: 2, Providers: []ProviderAcquisitionSet{candidate(order[0], "1")}},
		{MusicID: 3, Providers: []ProviderAcquisitionSet{noMatch(order[0]), candidate(order[1], "2")}},
		{MusicID: 4, Providers: []ProviderAcquisitionSet{
			noMatch(order[0]), candidate(order[1], "3"), candidate(order[2], "4"),
		}},
	}
	set, err := NewAcquisitionSet("test-recovery-v2", strings.Repeat("a", 64), order, songs)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateAcquisitionSetAuthorization(
		set, set.PlanID, set.PlanSHA256, []int{2, 3, 4}, order, nil,
	); err != nil {
		t.Fatalf("exact variable evaluated prefixes were rejected: %v", err)
	}
	for index, want := range []int{1, 2, 3} {
		if got := len(set.Songs[index].Providers); got != want {
			t.Fatalf("song %d provider prefix length=%d want=%d", set.Songs[index].MusicID, got, want)
		}
	}
}

func TestAcquisitionSetPrivatePublicationRejectsOverwriteModeAndSymlink(t *testing.T) {
	set := testClosedAcquisitionSet(t)
	root := privateRecoveryTempDir(t)
	path := filepath.Join(root, "acquisition-set.json")
	if err := PublishAcquisitionSet(path, set); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
		t.Fatalf("published acquisition set identity is invalid: info=%v err=%v", info, err)
	}
	if _, err := OpenAcquisitionSet(path); err != nil {
		t.Fatal(err)
	}
	if err := PublishAcquisitionSet(path, set); !errors.Is(err, ErrAlreadyPublished) {
		t.Fatalf("no-overwrite publication error=%v", err)
	}

	link := filepath.Join(root, "acquisition-set-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAcquisitionSet(link); err == nil {
		t.Fatal("symlink acquisition set was accepted")
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenAcquisitionSet(path); err == nil {
		t.Fatal("wrong-mode acquisition set was accepted")
	}
}

func testClosedAcquisitionSet(t *testing.T) AcquisitionSet {
	t.Helper()
	order := testProviderOrder()
	closed := make([]ProviderAcquisitionSet, len(order))
	for index, provider := range order {
		closed[index] = ProviderAcquisitionSet{
			Provider: provider, AcquisitionIDs: []lyricsacquisition.AcquisitionID{},
			Status: lyricsprovideroutcome.StatusNoMatch, ReasonCode: lyricsprovideroutcome.ReasonNoSearchHits,
			Phase: lyricsprovideroutcome.PhaseResolveTargets, Counts: lyricsprovideroutcome.Counts{NoMatch: 1},
		}
	}
	set, err := NewAcquisitionSet("test-recovery-v2", strings.Repeat("a", 64), order, []SongAcquisitionSet{{
		MusicID: 2, Providers: closed,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func testProviderOrder() []model.LyricsSourceProvider {
	return []model.LyricsSourceProvider{
		model.LyricsSourceProviderSekaipedia,
		model.LyricsSourceProviderMoegirl,
		model.LyricsSourceProviderVocaloidFandom,
	}
}
