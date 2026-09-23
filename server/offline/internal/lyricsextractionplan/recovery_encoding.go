package lyricsextractionplan

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

func MarshalRecoveryCanonical(plan RecoveryPlan) ([]byte, error) {
	if err := ValidateRecovery(plan); err != nil {
		return nil, err
	}
	body, err := json.Marshal(plan)
	if err != nil {
		return nil, fmt.Errorf("encode canonical lyrics recovery plan: %w", err)
	}
	if len(body) == 0 || len(body) > MaxPlanBytes || !utf8.Valid(body) {
		return nil, errors.New("canonical lyrics recovery plan exceeds its encoded boundary")
	}
	return body, nil
}

func RecoveryCanonicalSHA256(plan RecoveryPlan) (string, error) {
	body, err := MarshalRecoveryCanonical(plan)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

func DecodeRecoveryCanonical(body []byte) (RecoveryPlan, error) {
	return decodeRecoveryCanonical(body, false)
}

// DecodeRecoveryCanonicalForInspection preserves strict canonical JSON,
// duplicate/unknown-field, trailing-byte, and semantic checks while admitting
// only the historical recovery-plan-v2 Sekaipedia List shape that predates its
// exact replay AcquisitionID. Operational use must call DecodeRecoveryCanonical.
func DecodeRecoveryCanonicalForInspection(body []byte) (RecoveryPlan, error) {
	return decodeRecoveryCanonical(body, true)
}

func decodeRecoveryCanonical(body []byte, inspection bool) (RecoveryPlan, error) {
	var plan RecoveryPlan
	if err := inspectPlanJSON(body); err != nil {
		return RecoveryPlan{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&plan); err != nil {
		return RecoveryPlan{}, fmt.Errorf("decode lyrics recovery plan: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return RecoveryPlan{}, fmt.Errorf("decode lyrics recovery plan trailing bytes: %w", err)
		}
		return RecoveryPlan{}, errors.New("decode lyrics recovery plan: trailing JSON value")
	}
	canonical, err := json.Marshal(plan)
	if err != nil {
		return RecoveryPlan{}, err
	}
	if !bytes.Equal(body, canonical) {
		return RecoveryPlan{}, errors.New("lyrics recovery plan bytes are not canonical recovery-plan-v2 JSON")
	}
	if inspection {
		err = ValidateRecoveryForInspection(plan)
	} else {
		err = ValidateRecovery(plan)
	}
	if err != nil {
		return RecoveryPlan{}, err
	}
	return cloneRecoveryPlan(plan), nil
}

func CheckRecovery(body []byte, expectedSHA256 string) (RecoveryPlan, string, error) {
	return checkRecovery(body, expectedSHA256, false)
}

// CheckRecoveryForInspection binds historical inspection-only bytes to an
// external digest without making the decoded plan operationally valid.
func CheckRecoveryForInspection(body []byte, expectedSHA256 string) (RecoveryPlan, string, error) {
	return checkRecovery(body, expectedSHA256, true)
}

func checkRecovery(body []byte, expectedSHA256 string, inspection bool) (RecoveryPlan, string, error) {
	if !canonicalSHA256.MatchString(expectedSHA256) {
		return RecoveryPlan{}, "", errors.New("expected recovery plan digest must be a canonical lowercase SHA-256")
	}
	var (
		plan RecoveryPlan
		err  error
	)
	if inspection {
		plan, err = DecodeRecoveryCanonicalForInspection(body)
	} else {
		plan, err = DecodeRecoveryCanonical(body)
	}
	if err != nil {
		return RecoveryPlan{}, "", err
	}
	digest := sha256.Sum256(body)
	actual := hex.EncodeToString(digest[:])
	if actual != expectedSHA256 {
		return RecoveryPlan{}, actual, errors.New("lyrics recovery plan does not match the expected digest")
	}
	return plan, actual, nil
}

func cloneRecoveryPlan(plan RecoveryPlan) RecoveryPlan {
	plan.SourceSnapshot.Files = append([]SourceFileIdentity{}, plan.SourceSnapshot.Files...)
	plan.Scope.MusicIDs = append([]int{}, plan.Scope.MusicIDs...)
	plan.Providers.Order = append([]Provider{}, plan.Providers.Order...)
	plan.Providers.Configurations = append([]RecoveryProviderPlan{}, plan.Providers.Configurations...)
	for index := range plan.Providers.Configurations {
		configured := &plan.Providers.Configurations[index]
		configured.MusicIDs = append([]int{}, configured.MusicIDs...)
		configured.Authorities = append([]FixedAuthority{}, configured.Authorities...)
		configured.ContributorAliases = append([]RecoveryContributorAlias{}, configured.ContributorAliases...)
		configured.SekaipediaTargets = append([]RecoverySekaipediaPageTarget{}, configured.SekaipediaTargets...)
		for targetIndex := range configured.SekaipediaTargets {
			if configured.SekaipediaTargets[targetIndex].FixedRevision != nil {
				fixed := *configured.SekaipediaTargets[targetIndex].FixedRevision
				configured.SekaipediaTargets[targetIndex].FixedRevision = &fixed
			}
		}
		configured.ExactPublicTargets = append([]RecoveryExactPublicPageTarget{}, configured.ExactPublicTargets...)
	}
	plan.Versions.Parsers = append([]RecoveryParserVersion{}, plan.Versions.Parsers...)
	plan.Execution.LiveCanaryMusicIDs = append([]int{}, plan.Execution.LiveCanaryMusicIDs...)
	if plan.SekaipediaCanary != nil {
		canary := *plan.SekaipediaCanary
		canary.Songs = append([]RecoverySekaipediaCanarySong{}, canary.Songs...)
		plan.SekaipediaCanary = &canary
	}
	return plan
}
