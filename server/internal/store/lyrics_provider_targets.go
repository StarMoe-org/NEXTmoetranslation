package store

import (
	"fmt"
	"time"
)

// Schema v36 holds the reviewed provider page-target and contributor-alias
// maps that earlier releases compiled into the binary. The registry validator
// requires the targets ordered by music ID, so every read orders them here.

// LyricsProviderPageTarget binds one catalog music ID to the reviewed provider
// page title. ResolvedPageTitle is empty when the List title already resolves.
type LyricsProviderPageTarget struct {
	MusicID           int
	PageTitle         string
	ResolvedPageTitle string
	UpdatedAt         int64
	UpdatedBy         string
}

// LyricsProviderContributorAlias maps one song-scoped catalog contributor to
// the identity the provider credits.
type LyricsProviderContributorAlias struct {
	MusicID             int
	CatalogContributor  string
	ProviderContributor string
	UpdatedAt           int64
	UpdatedBy           string
}

func (s *Store) LyricsProviderTargets(provider string) ([]LyricsProviderPageTarget, []LyricsProviderContributorAlias, error) {
	targetRows, err := s.db.Query(`SELECT music_id,page_title,resolved_page_title,updated_at,updated_by
		FROM lyrics_provider_page_targets WHERE provider=? ORDER BY music_id`, provider)
	if err != nil {
		return nil, nil, err
	}
	defer targetRows.Close()
	targets := []LyricsProviderPageTarget{}
	for targetRows.Next() {
		var target LyricsProviderPageTarget
		if err := targetRows.Scan(&target.MusicID, &target.PageTitle, &target.ResolvedPageTitle,
			&target.UpdatedAt, &target.UpdatedBy); err != nil {
			return nil, nil, err
		}
		targets = append(targets, target)
	}
	if err := targetRows.Err(); err != nil {
		return nil, nil, err
	}

	aliasRows, err := s.db.Query(`SELECT music_id,catalog_contributor,provider_contributor,updated_at,updated_by
		FROM lyrics_provider_contributor_aliases WHERE provider=? ORDER BY music_id,catalog_contributor`, provider)
	if err != nil {
		return nil, nil, err
	}
	defer aliasRows.Close()
	aliases := []LyricsProviderContributorAlias{}
	for aliasRows.Next() {
		var alias LyricsProviderContributorAlias
		if err := aliasRows.Scan(&alias.MusicID, &alias.CatalogContributor, &alias.ProviderContributor,
			&alias.UpdatedAt, &alias.UpdatedBy); err != nil {
			return nil, nil, err
		}
		aliases = append(aliases, alias)
	}
	if err := aliasRows.Err(); err != nil {
		return nil, nil, err
	}
	return targets, aliases, nil
}

// UpsertLyricsProviderTarget writes one song's page target and replaces that
// song's alias rows in a single transaction.
func (s *Store) UpsertLyricsProviderTarget(
	provider string,
	target LyricsProviderPageTarget,
	aliases []LyricsProviderContributorAlias,
	actor string,
) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO lyrics_provider_page_targets
		(provider,music_id,page_title,resolved_page_title,updated_at,updated_by) VALUES (?,?,?,?,?,?)
		ON CONFLICT(provider,music_id) DO UPDATE SET page_title=excluded.page_title,
		resolved_page_title=excluded.resolved_page_title, updated_at=excluded.updated_at,
		updated_by=excluded.updated_by`,
		provider, target.MusicID, target.PageTitle, target.ResolvedPageTitle, now, actor); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM lyrics_provider_contributor_aliases WHERE provider=? AND music_id=?`,
		provider, target.MusicID); err != nil {
		return err
	}
	for _, alias := range aliases {
		if _, err := tx.Exec(`INSERT INTO lyrics_provider_contributor_aliases
			(provider,music_id,catalog_contributor,provider_contributor,updated_at,updated_by) VALUES (?,?,?,?,?,?)`,
			provider, target.MusicID, alias.CatalogContributor, alias.ProviderContributor, now, actor); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.provider-target.upsert',?)`,
		now, actor, fmt.Sprintf("provider=%s musicId=%d pageTitle=%s aliases=%d",
			provider, target.MusicID, target.PageTitle, len(aliases))); err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteLyricsProviderTarget removes one song's page target and its aliases.
// It reports whether a target row existed.
func (s *Store) DeleteLyricsProviderTarget(provider string, musicID int, actor string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM lyrics_provider_page_targets WHERE provider=? AND music_id=?`, provider, musicID)
	if err != nil {
		return false, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if removed == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`DELETE FROM lyrics_provider_contributor_aliases WHERE provider=? AND music_id=?`,
		provider, musicID); err != nil {
		return false, err
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO audit_log(ts,user,action,detail) VALUES (?,?,'lyrics.provider-target.delete',?)`,
		now, actor, fmt.Sprintf("provider=%s musicId=%d", provider, musicID)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
