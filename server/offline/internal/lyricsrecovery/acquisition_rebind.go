package lyricsrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/internal/model"
	"moesekai/server/offline/internal/lyricsacquisition"
)

// RebindAcquisitionSet derives current parser terminals from one exact immutable
// acquisition set, copies only those caller-pinned acquisitions into a fresh
// ledger without changing their content addresses, and binds the resulting set
// to the supplied recovery-plan runtime. It performs no provider I/O.
func RebindAcquisitionSet(
	ctx context.Context,
	sourceSet AcquisitionSet,
	sourceLedger *lyricsacquisition.Ledger,
	destinationLedger *lyricsacquisition.Ledger,
	runtime RuntimeConfig,
	identities []lyricssource.MusicIdentity,
) (AcquisitionSet, error) {
	return rebindAcquisitionSets(
		ctx, sourceSet, sourceLedger, nil, nil, nil, destinationLedger, runtime, identities,
	)
}

// RebindAcquisitionSetWithSupplement overlays one independently plan-bound,
// explicitly scoped acquisition set onto a complete historical source set.
// Songs absent from the supplement remain owned by the primary source. Songs
// present in it are replayed and copied only from the supplemental ledger.
// The operation performs no provider I/O.
func RebindAcquisitionSetWithSupplement(
	ctx context.Context,
	sourceSet AcquisitionSet,
	sourceLedger *lyricsacquisition.Ledger,
	supplementSet AcquisitionSet,
	supplementLedger *lyricsacquisition.Ledger,
	destinationLedger *lyricsacquisition.Ledger,
	runtime RuntimeConfig,
	identities []lyricssource.MusicIdentity,
) (AcquisitionSet, error) {
	return rebindAcquisitionSets(
		ctx, sourceSet, sourceLedger, &supplementSet, supplementLedger, nil,
		destinationLedger, runtime, identities,
	)
}

// RebindAcquisitionSetWithSekaipediaList replaces each historical Sekaipedia
// List observation while preserving every song-page and exact-public byte.
func RebindAcquisitionSetWithSekaipediaList(
	ctx context.Context,
	sourceSet AcquisitionSet,
	sourceLedger *lyricsacquisition.Ledger,
	sekaipediaList lyricsacquisition.Acquisition,
	destinationLedger *lyricsacquisition.Ledger,
	runtime RuntimeConfig,
	identities []lyricssource.MusicIdentity,
) (AcquisitionSet, error) {
	return rebindAcquisitionSets(
		ctx, sourceSet, sourceLedger, nil, nil, &sekaipediaList,
		destinationLedger, runtime, identities,
	)
}

// RebindAcquisitionSetWithSupplementAndSekaipediaList overlays supplemental
// songs while replacing each historical Sekaipedia List observation with a
// distinct deterministic observation of the exact List acquisition bound by
// the destination plan. Song-page and exact-public bytes remain byte-identical
// to their caller-pinned source ledgers, and no provider I/O is performed.
func RebindAcquisitionSetWithSupplementAndSekaipediaList(
	ctx context.Context,
	sourceSet AcquisitionSet,
	sourceLedger *lyricsacquisition.Ledger,
	supplementSet AcquisitionSet,
	supplementLedger *lyricsacquisition.Ledger,
	sekaipediaList lyricsacquisition.Acquisition,
	destinationLedger *lyricsacquisition.Ledger,
	runtime RuntimeConfig,
	identities []lyricssource.MusicIdentity,
) (AcquisitionSet, error) {
	return rebindAcquisitionSets(
		ctx, sourceSet, sourceLedger, &supplementSet, supplementLedger, &sekaipediaList,
		destinationLedger, runtime, identities,
	)
}

func rebindAcquisitionSets(
	ctx context.Context,
	sourceSet AcquisitionSet,
	sourceLedger *lyricsacquisition.Ledger,
	supplementSet *AcquisitionSet,
	supplementLedger *lyricsacquisition.Ledger,
	sekaipediaList *lyricsacquisition.Acquisition,
	destinationLedger *lyricsacquisition.Ledger,
	runtime RuntimeConfig,
	identities []lyricssource.MusicIdentity,
) (AcquisitionSet, error) {
	if ctx == nil || sourceLedger == nil || destinationLedger == nil ||
		(supplementSet == nil) != (supplementLedger == nil) ||
		runtime.RecoveryPlanID == "" || !canonicalAcquisitionSetSHA256.MatchString(runtime.RecoveryPlanSHA256) ||
		runtime.PolicyVersion == "" {
		return AcquisitionSet{}, errors.New("lyrics recovery acquisition rebind input is invalid")
	}
	sourceRoot, err := sourceLedger.RootPath()
	if err != nil {
		return AcquisitionSet{}, err
	}
	destinationRoot, err := destinationLedger.RootPath()
	if err != nil {
		return AcquisitionSet{}, err
	}
	if sourceRoot == destinationRoot {
		return AcquisitionSet{}, errors.New("lyrics recovery acquisition rebind requires distinct ledgers")
	}
	if supplementLedger != nil {
		supplementRoot, err := supplementLedger.RootPath()
		if err != nil {
			return AcquisitionSet{}, err
		}
		if supplementRoot == sourceRoot || supplementRoot == destinationRoot {
			return AcquisitionSet{}, errors.New("lyrics recovery supplemental rebind requires three distinct ledgers")
		}
	}
	musicIDs, err := rebindMusicIDs(identities)
	if err != nil {
		return AcquisitionSet{}, err
	}
	primaryMusicIDs, supplementMusicIDs, orderedSupplementMusicIDs, err := partitionRebindMusicIDs(musicIDs, supplementSet)
	if err != nil {
		return AcquisitionSet{}, err
	}
	if len(primaryMusicIDs) == 0 {
		if err := ValidateAcquisitionSet(sourceSet); err != nil {
			return AcquisitionSet{}, fmt.Errorf("validate unused source acquisition set for rebind: %w", err)
		}
	} else if err := ValidateAcquisitionSetAuthorization(
		sourceSet, sourceSet.PlanID, sourceSet.PlanSHA256, primaryMusicIDs,
		runtime.Order, runtime.ProviderMusicIDs,
	); err != nil {
		return AcquisitionSet{}, fmt.Errorf("authorize source acquisition set for rebind: %w", err)
	}
	if supplementSet != nil {
		if err := ValidateAcquisitionSetAuthorization(
			*supplementSet, supplementSet.PlanID, supplementSet.PlanSHA256,
			orderedSupplementMusicIDs, runtime.Order, runtime.ProviderMusicIDs,
		); err != nil {
			return AcquisitionSet{}, fmt.Errorf("authorize supplemental acquisition set for rebind: %w", err)
		}
	}

	if sekaipediaList != nil {
		if err := validateRebindSekaipediaList(*sekaipediaList, runtime); err != nil {
			return AcquisitionSet{}, err
		}
	}
	reboundSongs := make([]SongAcquisitionSet, 0, len(identities))
	sourceLedgers := make(map[int]*lyricsacquisition.Ledger, len(identities))
	for _, identity := range identities {
		selectedSet := &sourceSet
		selectedLedger := sourceLedger
		if _, supplemented := supplementMusicIDs[identity.MusicID]; supplemented {
			selectedSet = supplementSet
			selectedLedger = supplementLedger
		}
		providers, err := selectedSet.OrderedProviders(identity.MusicID)
		if err != nil {
			return AcquisitionSet{}, err
		}
		replayLedger := selectedLedger
		if sekaipediaList != nil {
			providers, err = rebindSongAcquisitionsWithSekaipediaList(
				ctx, identity.MusicID, providers, selectedLedger, destinationLedger,
				*sekaipediaList, runtime,
			)
			if err != nil {
				return AcquisitionSet{}, err
			}
			replayLedger = destinationLedger
		}
		replayed, err := replaySong(
			ctx, identity.MusicID, identity, runtime.PolicyVersion, runtime,
			replayLedger, providers, false,
		)
		if err != nil {
			return AcquisitionSet{}, fmt.Errorf("derive music %d acquisition terminal: %w", identity.MusicID, err)
		}
		if len(replayed.Providers) != len(providers) {
			return AcquisitionSet{}, errors.New("lyrics recovery acquisition rebind changed the evaluated provider prefix")
		}
		reboundProviders := make([]ProviderAcquisitionSet, len(providers))
		for index, provider := range providers {
			outcome := replayed.Providers[index].Outcome
			if outcome.Provider != provider.Provider || outcome.Validate() != nil {
				return AcquisitionSet{}, errors.New("lyrics recovery acquisition rebind derived an invalid provider terminal")
			}
			reboundProviders[index] = ProviderAcquisitionSet{
				Provider:       provider.Provider,
				AcquisitionIDs: append([]lyricsacquisition.AcquisitionID(nil), provider.AcquisitionIDs...),
				Status:         outcome.Status,
				ReasonCode:     outcome.Diagnostic.ReasonCode,
				Phase:          outcome.Diagnostic.Phase,
				Counts:         outcome.Diagnostic.Counts,
			}
		}
		reboundSongs = append(reboundSongs, SongAcquisitionSet{
			MusicID: identity.MusicID, Providers: reboundProviders,
		})
		if sekaipediaList == nil {
			sourceLedgers[identity.MusicID] = selectedLedger
		}
	}

	rebound, err := NewAcquisitionSet(
		runtime.RecoveryPlanID, runtime.RecoveryPlanSHA256, runtime.Order, reboundSongs,
	)
	if err != nil {
		return AcquisitionSet{}, err
	}
	if err := ValidateAcquisitionSetAuthorization(
		rebound, runtime.RecoveryPlanID, runtime.RecoveryPlanSHA256, musicIDs,
		runtime.Order, runtime.ProviderMusicIDs,
	); err != nil {
		return AcquisitionSet{}, err
	}
	if sekaipediaList == nil {
		if err := copyReboundAcquisitionsFromSources(ctx, rebound, sourceLedgers, destinationLedger); err != nil {
			return AcquisitionSet{}, err
		}
	}
	if err := ValidateAcquisitionSetClosedReplay(ctx, rebound, destinationLedger, runtime, identities); err != nil {
		return AcquisitionSet{}, err
	}
	return cloneAcquisitionSet(rebound), nil
}

// ValidateAcquisitionSetClosedReplay proves that a rebound set's regenerated
// terminals exactly match current parser outcomes while every request is served
// by the caller-pinned AcquisitionIDs. It performs no provider I/O.
func ValidateAcquisitionSetClosedReplay(
	ctx context.Context,
	set AcquisitionSet,
	ledger *lyricsacquisition.Ledger,
	runtime RuntimeConfig,
	identities []lyricssource.MusicIdentity,
) error {
	if ctx == nil || ledger == nil || runtime.PolicyVersion == "" {
		return errors.New("lyrics recovery closed rebind replay input is invalid")
	}
	musicIDs, err := rebindMusicIDs(identities)
	if err != nil {
		return err
	}
	if err := ValidateAcquisitionSetAuthorization(
		set,
		runtime.RecoveryPlanID,
		runtime.RecoveryPlanSHA256,
		musicIDs,
		runtime.Order,
		runtime.ProviderMusicIDs,
	); err != nil {
		return err
	}
	for _, identity := range identities {
		providers, err := set.OrderedProviders(identity.MusicID)
		if err != nil {
			return err
		}
		replayed, err := ReplaySong(
			ctx,
			identity.MusicID,
			identity,
			runtime.PolicyVersion,
			runtime,
			ledger,
			providers,
		)
		if err != nil {
			return fmt.Errorf("validate music %d rebound terminal: %w", identity.MusicID, err)
		}
		if replayed.MusicID != identity.MusicID || len(replayed.Providers) != len(providers) {
			return errors.New("lyrics recovery closed rebind replay changed the provider boundary")
		}
		for index, provider := range providers {
			if replayed.Providers[index].Outcome.Provider != provider.Provider {
				return errors.New("lyrics recovery closed rebind replay changed provider order")
			}
		}
	}
	return nil
}

func validateRebindSekaipediaList(
	acquired lyricsacquisition.Acquisition,
	runtime RuntimeConfig,
) error {
	authorities := runtime.Authorities[lyricssource.ProviderSekaipedia]
	if runtime.SekaipediaCanary == nil || len(authorities) != 1 ||
		acquired.AcquisitionID != lyricsacquisition.AcquisitionID(runtime.SekaipediaCanary.ListAcquisitionID) ||
		acquired.Request.Provider != string(lyricssource.ProviderSekaipedia) || !acquired.ReplayOnly ||
		acquired.RawResponseSHA256 != authorities[0].RawSHA256 ||
		lyricssource.VerifySekaipediaRevisionContent(acquired.RawResponse, authorities[0]) != nil {
		return errors.New("lyrics recovery rebind List acquisition does not exactly match the destination plan")
	}
	return nil
}

func rebindSongAcquisitionsWithSekaipediaList(
	ctx context.Context,
	musicID int,
	providers []ProviderAcquisitionSet,
	source *lyricsacquisition.Ledger,
	destination *lyricsacquisition.Ledger,
	list lyricsacquisition.Acquisition,
	runtime RuntimeConfig,
) ([]ProviderAcquisitionSet, error) {
	result := cloneProviderAcquisitionSets(providers)
	for providerIndex := range result {
		provider := &result[providerIndex]
		if provider.Provider != lyricssource.ProviderSekaipedia {
			for _, acquisitionID := range provider.AcquisitionIDs {
				if _, err := copyRebindAcquisition(ctx, source, destination, acquisitionID); err != nil {
					return nil, err
				}
			}
			continue
		}
		if len(provider.AcquisitionIDs) < 2 {
			return nil, errors.New("lyrics recovery Sekaipedia rebind lacks a List and song acquisition")
		}
		historicalList, err := source.ReplayByAcquisitionID(ctx, provider.AcquisitionIDs[0])
		if err != nil {
			return nil, err
		}
		if !rebindAcquisitionIsSekaipediaList(historicalList) {
			return nil, errors.New("lyrics recovery Sekaipedia rebind first acquisition is not the historical List")
		}
		currentList, err := commitRebindSekaipediaListObservation(ctx, musicID, list, destination, runtime)
		if err != nil {
			return nil, err
		}
		acquisitionIDs := make([]lyricsacquisition.AcquisitionID, 1, len(provider.AcquisitionIDs))
		acquisitionIDs[0] = currentList.AcquisitionID
		for _, acquisitionID := range provider.AcquisitionIDs[1:] {
			copied, err := copyRebindAcquisition(ctx, source, destination, acquisitionID)
			if err != nil {
				return nil, err
			}
			if rebindAcquisitionIsSekaipediaList(copied) {
				return nil, errors.New("lyrics recovery Sekaipedia rebind contains a second List acquisition")
			}
			acquisitionIDs = append(acquisitionIDs, copied.AcquisitionID)
		}
		provider.AcquisitionIDs = acquisitionIDs
	}
	return result, nil
}

func commitRebindSekaipediaListObservation(
	ctx context.Context,
	musicID int,
	list lyricsacquisition.Acquisition,
	destination *lyricsacquisition.Ledger,
	runtime RuntimeConfig,
) (lyricsacquisition.Acquisition, error) {
	fetchedAt, err := time.Parse(time.RFC3339Nano, list.FetchedAt)
	if err != nil || fetchedAt.Location() != time.UTC {
		return lyricsacquisition.Acquisition{}, errors.New("lyrics recovery rebind List fetchedAt is invalid")
	}
	fetchedAt = fetchedAt.Add(time.Duration(musicID) * time.Nanosecond)
	capture, err := lyricssource.CaptureRecoveryHTTPResponse(
		model.LyricsSourceProviderSekaipedia,
		runtime.Authorities[lyricssource.ProviderSekaipedia],
		lyricssource.RecoveryHTTPResponse{
			Action: "page", CanonicalRequestURL: list.Request.CanonicalRequestIdentity,
			FetchedAt: fetchedAt, Raw: append([]byte(nil), list.RawResponse...),
		},
	)
	if err != nil {
		return lyricsacquisition.Acquisition{}, err
	}
	return destination.Commit(ctx, recordInputFromCapture(capture))
}

func copyRebindAcquisition(
	ctx context.Context,
	source *lyricsacquisition.Ledger,
	destination *lyricsacquisition.Ledger,
	acquisitionID lyricsacquisition.AcquisitionID,
) (lyricsacquisition.Acquisition, error) {
	acquired, err := source.ReplayByAcquisitionID(ctx, acquisitionID)
	if err != nil {
		return lyricsacquisition.Acquisition{}, err
	}
	committed, err := destination.Commit(ctx, recordInputFromAcquisition(acquired))
	if err != nil {
		return lyricsacquisition.Acquisition{}, err
	}
	if committed.AcquisitionID != acquired.AcquisitionID {
		return lyricsacquisition.Acquisition{}, errors.New("lyrics recovery rebind copied acquisition changed content address")
	}
	replayed, err := destination.ReplayByAcquisitionID(ctx, committed.AcquisitionID)
	if err != nil {
		return lyricsacquisition.Acquisition{}, err
	}
	if !reflect.DeepEqual(replayed, acquired) {
		return lyricsacquisition.Acquisition{}, errors.New("lyrics recovery rebind copied acquisition changed immutable identity")
	}
	return replayed, nil
}

func rebindAcquisitionIsSekaipediaList(acquired lyricsacquisition.Acquisition) bool {
	if acquired.Request.Provider != string(lyricssource.ProviderSekaipedia) || len(acquired.RawResponse) == 0 {
		return false
	}
	var response struct {
		Query struct {
			Pages []struct {
				PageID int    `json:"pageid"`
				Title  string `json:"title"`
			} `json:"pages"`
		} `json:"query"`
	}
	return json.Unmarshal(acquired.RawResponse, &response) == nil && len(response.Query.Pages) == 1 &&
		response.Query.Pages[0].PageID == 268 && response.Query.Pages[0].Title == "List of songs"
}

func partitionRebindMusicIDs(
	musicIDs []int,
	supplementSet *AcquisitionSet,
) ([]int, map[int]struct{}, []int, error) {
	supplementMusicIDs := make(map[int]struct{})
	if supplementSet == nil {
		return append([]int(nil), musicIDs...), supplementMusicIDs, nil, nil
	}
	allowed := make(map[int]struct{}, len(musicIDs))
	for _, musicID := range musicIDs {
		allowed[musicID] = struct{}{}
	}
	orderedSupplementMusicIDs := make([]int, len(supplementSet.Songs))
	for index, song := range supplementSet.Songs {
		if _, exists := allowed[song.MusicID]; !exists {
			return nil, nil, nil, errors.New("lyrics recovery supplement contains a song outside the destination plan scope")
		}
		orderedSupplementMusicIDs[index] = song.MusicID
		supplementMusicIDs[song.MusicID] = struct{}{}
	}
	primaryMusicIDs := make([]int, 0, len(musicIDs)-len(supplementMusicIDs))
	for _, musicID := range musicIDs {
		if _, supplemented := supplementMusicIDs[musicID]; !supplemented {
			primaryMusicIDs = append(primaryMusicIDs, musicID)
		}
	}
	return primaryMusicIDs, supplementMusicIDs, orderedSupplementMusicIDs, nil
}

func rebindMusicIDs(identities []lyricssource.MusicIdentity) ([]int, error) {
	if len(identities) == 0 || len(identities) > 10_000 {
		return nil, errors.New("lyrics recovery rebind identities are empty or outside their bound")
	}
	result := make([]int, len(identities))
	lastMusicID := 0
	for index, identity := range identities {
		if identity.MusicID <= lastMusicID {
			return nil, errors.New("lyrics recovery rebind identities are not strictly ordered")
		}
		result[index] = identity.MusicID
		lastMusicID = identity.MusicID
	}
	return result, nil
}

func copyReboundAcquisitions(
	ctx context.Context,
	set AcquisitionSet,
	source *lyricsacquisition.Ledger,
	destination *lyricsacquisition.Ledger,
) error {
	sources := make(map[int]*lyricsacquisition.Ledger, len(set.Songs))
	for _, song := range set.Songs {
		sources[song.MusicID] = source
	}
	return copyReboundAcquisitionsFromSources(ctx, set, sources, destination)
}

func copyReboundAcquisitionsFromSources(
	ctx context.Context,
	set AcquisitionSet,
	sources map[int]*lyricsacquisition.Ledger,
	destination *lyricsacquisition.Ledger,
) error {
	if ctx == nil || destination == nil || len(sources) != len(set.Songs) {
		return errors.New("lyrics recovery acquisition copy source map is invalid")
	}
	for _, song := range set.Songs {
		source := sources[song.MusicID]
		if source == nil {
			return errors.New("lyrics recovery acquisition copy source is missing a song ledger")
		}
		for _, provider := range song.Providers {
			for _, acquisitionID := range provider.AcquisitionIDs {
				acquired, err := source.ReplayByAcquisitionID(ctx, acquisitionID)
				if err != nil {
					return fmt.Errorf("replay source acquisition %s: %w", acquisitionID, err)
				}
				if acquired.Request.Provider != string(provider.Provider) {
					return errors.New("lyrics recovery source acquisition provider conflicts with its exact set")
				}
				committed, err := destination.Commit(ctx, recordInputFromAcquisition(acquired))
				if err != nil {
					return fmt.Errorf("copy acquisition %s: %w", acquisitionID, err)
				}
				if committed.AcquisitionID != acquisitionID {
					return errors.New("lyrics recovery copied acquisition changed content address")
				}
				replayed, err := destination.ReplayByAcquisitionID(ctx, acquisitionID)
				if err != nil {
					return fmt.Errorf("replay copied acquisition %s: %w", acquisitionID, err)
				}
				if !reflect.DeepEqual(acquired, replayed) {
					return errors.New("lyrics recovery copied acquisition changed immutable identity")
				}
			}
		}
	}
	return nil
}

func recordInputFromAcquisition(acquired lyricsacquisition.Acquisition) lyricsacquisition.RecordInput {
	observed := make([]lyricsacquisition.ObservedRevision, len(acquired.ObservedRevisions))
	copy(observed, acquired.ObservedRevisions)
	return lyricsacquisition.RecordInput{
		Request:           acquired.Request,
		FetchedAt:         acquired.FetchedAt,
		RawResponse:       append([]byte(nil), acquired.RawResponse...),
		RawResponseSHA256: acquired.RawResponseSHA256,
		Evidence: lyricsacquisition.EvidenceProjection{
			EvidenceID: acquired.Evidence.EvidenceID,
			Raw:        append([]byte(nil), acquired.Evidence.Raw...),
			RawSHA256:  acquired.Evidence.RawSHA256,
		},
		EvidenceEnvelope:       append([]byte(nil), acquired.EvidenceEnvelope...),
		EvidenceEnvelopeSHA256: acquired.EvidenceEnvelopeSHA256,
		ObservedRevisions:      observed,
	}
}
