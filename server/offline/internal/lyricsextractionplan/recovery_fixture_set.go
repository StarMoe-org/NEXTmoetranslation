package lyricsextractionplan

import (
	"bytes"
	"fmt"
)

// RecoveryFixtureSet contains descriptor-verified reviewed fixtures. Its state
// is private so callers can obtain only defensive copies of identities/bytes.
type RecoveryFixtureSet struct {
	identities []RecoveryFixtureIdentityV2
	bodies     map[string][]byte
}

func (set RecoveryFixtureSet) Len() int {
	return len(set.identities)
}

func (set RecoveryFixtureSet) Identities() []RecoveryFixtureIdentityV2 {
	return append([]RecoveryFixtureIdentityV2(nil), set.identities...)
}

func (set RecoveryFixtureSet) Bytes(relativePath string) ([]byte, bool) {
	body, found := set.bodies[relativePath]
	if !found {
		return nil, false
	}
	return bytes.Clone(body), true
}

func (set RecoveryFixtureSet) All() map[string][]byte {
	result := make(map[string][]byte, len(set.bodies))
	for relativePath, body := range set.bodies {
		result[relativePath] = bytes.Clone(body)
	}
	return result
}

// LoadVerifiedRecoveryFixtureSet verifies the recovery plan and exact current
// source closure, then returns fixture bytes captured by the same pinned file
// descriptors that produced the compared size/SHA-256 identities.
func LoadVerifiedRecoveryFixtureSet(root string, plan RecoveryPlan) (RecoveryFixtureSet, error) {
	return loadVerifiedRecoveryFixtureSet(root, plan, nil)
}

func loadVerifiedRecoveryFixtureSet(root string, plan RecoveryPlan, hook sourceReadHook) (RecoveryFixtureSet, error) {
	if err := ValidateRecovery(plan); err != nil {
		return RecoveryFixtureSet{}, err
	}
	files, bodies, identities, err := deriveRecoverySourceClosure(root, hook, true)
	if err != nil {
		return RecoveryFixtureSet{}, fmt.Errorf("load verified recovery fixtures: %w", err)
	}
	if err := compareSourceIdentities(plan.SourceSnapshot.Files, files); err != nil {
		return RecoveryFixtureSet{}, fmt.Errorf("load verified recovery fixtures: %w", err)
	}
	ownedBodies := make(map[string][]byte, len(bodies))
	for relativePath, body := range bodies {
		ownedBodies[relativePath] = bytes.Clone(body)
	}
	return RecoveryFixtureSet{
		identities: append([]RecoveryFixtureIdentityV2(nil), identities...),
		bodies:     ownedBodies,
	}, nil
}
