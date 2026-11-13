package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	"voice-timeline-renderer/internal/domain"
)

// Impulse responses live beside sources but never share their storage
// namespace: a room response and a source recording with the same identifier
// remain distinct read-only assets.
func (repo *Repository) SaveImpulse(asset domain.ImpulseAsset, wav []byte) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()

	final := repo.path("impulses", asset.ID)
	if _, err := os.Stat(final); err == nil {
		return domain.E(domain.KindConflict, "impulse_conflict", "impulse_id", "impulse response identifier already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot inspect impulse destination", err)
	}
	temp, err := os.MkdirTemp(repo.path("tmp"), "impulse-*")
	if err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot create impulse staging directory", err)
	}
	defer func() {
		_ = os.RemoveAll(temp)
	}()
	if err := writeBytes(filepath.Join(temp, "impulse.wav"), wav, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write impulse media", err)
	}
	if err := writeJSONAtomic(filepath.Join(temp, "metadata.json"), asset, 0o640); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot write impulse metadata", err)
	}
	if err := os.Rename(temp, final); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot commit impulse response", err)
	}
	if err := syncDirectory(repo.path("impulses")); err != nil {
		return domain.Wrap(domain.KindInternal, "storage_error", "", "cannot sync impulse index", err)
	}
	return nil
}

func (repo *Repository) GetImpulseMeta(id string) (domain.ImpulseAsset, error) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var asset domain.ImpulseAsset
	if err := readJSON(repo.path("impulses", id, "metadata.json"), &asset); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return domain.ImpulseAsset{}, domain.E(domain.KindNotFound, "impulse_not_found", "impulse_id", "impulse response does not exist")
		}
		return domain.ImpulseAsset{}, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read impulse metadata", err)
	}
	return asset, nil
}

func (repo *Repository) GetImpulse(id string) (domain.ImpulseAsset, []byte, error) {
	asset, err := repo.GetImpulseMeta(id)
	if err != nil {
		return domain.ImpulseAsset{}, nil, err
	}
	repo.mu.RLock()
	payload, readErr := os.ReadFile(repo.path("impulses", id, "impulse.wav"))
	repo.mu.RUnlock()
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return domain.ImpulseAsset{}, nil, domain.E(domain.KindNotFound, "impulse_not_found", "impulse_id", "impulse media does not exist")
		}
		return domain.ImpulseAsset{}, nil, domain.Wrap(domain.KindInternal, "storage_error", "", "cannot read impulse media", readErr)
	}
	digest := sha256.Sum256(payload)
	if hex.EncodeToString(digest[:]) != asset.Digest {
		return domain.ImpulseAsset{}, nil, domain.E(domain.KindInternal, "impulse_integrity_error", "impulse_id", "stored impulse digest does not match metadata")
	}
	return asset, payload, nil
}
