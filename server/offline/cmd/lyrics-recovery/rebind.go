package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"moesekai/server/internal/lyricssource"
	"moesekai/server/offline/internal/lyricsacquisition"
	"moesekai/server/offline/internal/lyricsextractionplan"
	"moesekai/server/offline/internal/lyricsrecovery"
)

func runRebind(
	ctx context.Context,
	output io.Writer,
	options options,
	plan lyricsextractionplan.RecoveryPlan,
	planSHA string,
	catalog *checkedRecoveryCatalog,
	runtime lyricsrecovery.RuntimeConfig,
) (returnErr error) {
	sourceBody, err := readPinnedPrivateFile(
		options.rebindSourceAcquisitionSetPath, lyricsrecovery.MaxAcquisitionSetBytes,
	)
	if err != nil {
		return err
	}
	sourceSet, err := lyricsrecovery.DecodeAcquisitionSet(sourceBody)
	if err != nil {
		return err
	}
	if sourceSet.PlanID == plan.PlanID && sourceSet.PlanSHA256 == planSHA {
		return errors.New("offline rebind source already binds the destination recovery plan")
	}
	var supplementSet *lyricsrecovery.AcquisitionSet
	if options.rebindSupplementAcquisitionSetPath != "" {
		supplementBody, err := readPinnedPrivateFile(
			options.rebindSupplementAcquisitionSetPath, lyricsrecovery.MaxAcquisitionSetBytes,
		)
		if err != nil {
			return err
		}
		decoded, err := lyricsrecovery.DecodeAcquisitionSet(supplementBody)
		if err != nil {
			return err
		}
		if decoded.PlanID == plan.PlanID && decoded.PlanSHA256 == planSHA {
			return errors.New("offline rebind supplement already binds the destination recovery plan")
		}
		supplementSet = &decoded
	}

	identities := make([]lyricssource.MusicIdentity, 0, len(plan.Scope.MusicIDs))
	for _, musicID := range plan.Scope.MusicIDs {
		identity, err := catalog.MusicIdentity(ctx, musicID)
		if err != nil {
			return err
		}
		identities = append(identities, identity)
	}

	sourceLedger, err := openCheckedRecoveryLedger(ctx, options.rebindSourceLedgerPath)
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, sourceLedger.Close()) }()
	if err := sourceLedger.verify(); err != nil {
		return err
	}
	var supplementLedger *checkedRecoveryLedger
	if supplementSet != nil {
		supplementLedger, err = openCheckedRecoveryLedger(ctx, options.rebindSupplementLedgerPath)
		if err != nil {
			return err
		}
		defer func() { returnErr = errors.Join(returnErr, supplementLedger.Close()) }()
		if err := supplementLedger.verify(); err != nil {
			return err
		}
	}

	var listAcquisition *lyricsacquisition.Acquisition
	if options.sekaipediaListReplayLedgerPath != "" {
		if plan.SekaipediaCanary == nil ||
			options.sekaipediaListReplayAcquisitionID != plan.SekaipediaCanary.List.AcquisitionID {
			return errors.New("offline rebind List acquisition does not exactly match the immutable plan")
		}
		runtime, err = lyricsrecovery.WithSekaipediaCanaryPlan(runtime, plan)
		if err != nil {
			return err
		}
		acquired, err := readExactAcquisitionReplaySource(
			ctx,
			options.sekaipediaListReplayLedgerPath,
			exactReplayRuntimeCopyPath(options.ledgerPath),
			lyricsacquisition.AcquisitionID(options.sekaipediaListReplayAcquisitionID),
		)
		if err != nil {
			return err
		}
		listAcquisition = &acquired
	}

	destinationLedger, err := lyricsacquisition.CreateLedger(ctx, options.ledgerPath)
	if err != nil {
		return err
	}
	destinationOpen := true
	defer func() {
		if destinationOpen {
			returnErr = errors.Join(returnErr, destinationLedger.Close())
		}
	}()
	var rebound lyricsrecovery.AcquisitionSet
	switch {
	case supplementSet != nil && listAcquisition != nil:
		rebound, err = lyricsrecovery.RebindAcquisitionSetWithSupplementAndSekaipediaList(
			ctx, sourceSet, sourceLedger.ledger, *supplementSet, supplementLedger.ledger,
			*listAcquisition, destinationLedger, runtime, identities,
		)
	case supplementSet != nil:
		rebound, err = lyricsrecovery.RebindAcquisitionSetWithSupplement(
			ctx, sourceSet, sourceLedger.ledger, *supplementSet, supplementLedger.ledger,
			destinationLedger, runtime, identities,
		)
	case listAcquisition != nil:
		rebound, err = lyricsrecovery.RebindAcquisitionSetWithSekaipediaList(
			ctx, sourceSet, sourceLedger.ledger, *listAcquisition, destinationLedger, runtime, identities,
		)
	default:
		rebound, err = lyricsrecovery.RebindAcquisitionSet(
			ctx, sourceSet, sourceLedger.ledger, destinationLedger, runtime, identities,
		)
	}
	if err != nil {
		return err
	}
	if err := sourceLedger.verify(); err != nil {
		return err
	}
	if supplementLedger != nil {
		if err := supplementLedger.verify(); err != nil {
			return err
		}
	}
	if err := destinationLedger.Close(); err != nil {
		return err
	}
	destinationOpen = false

	reopened, err := openCheckedRecoveryLedger(ctx, options.ledgerPath)
	if err != nil {
		return err
	}
	if err := lyricsrecovery.ValidateAcquisitionSetClosedReplay(
		ctx, rebound, reopened.ledger, runtime, identities,
	); err != nil {
		_ = reopened.Close()
		return err
	}
	if err := reopened.Close(); err != nil {
		return err
	}
	if err := lyricsrecovery.PublishAcquisitionSet(options.acquisitionSetPath, rebound); err != nil {
		return err
	}
	supplementSongs := 0
	if supplementSet != nil {
		supplementSongs = len(supplementSet.Songs)
	}
	_, err = fmt.Fprintf(
		output,
		"PASS mode=rebind songs=%d acquisitions=%d supplementSongs=%d sourceSetSha256=%s setSha256=%s planSha256=%s network=HOLD publication=acquisition-set-only migration=HOLD\n",
		len(rebound.Songs), acquisitionReferenceCount(rebound), supplementSongs,
		sourceSet.SetSHA256, rebound.SetSHA256, planSHA,
	)
	return err
}

func acquisitionReferenceCount(set lyricsrecovery.AcquisitionSet) int {
	count := 0
	for _, song := range set.Songs {
		for _, provider := range song.Providers {
			count += len(provider.AcquisitionIDs)
		}
	}
	return count
}
