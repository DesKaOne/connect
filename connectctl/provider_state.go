package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/urnetwork/connect"
)

// Provider state deliberately persists identifiers, never JWTs or proxy
// credentials. Reusing the client_id prevents creating dashboard duplicates
// on every reconnect/restart. Keep this file if you want stable clients.
type providerIdentity struct {
	ClientID string `json:"client_id,omitempty"`
	InstanceID string `json:"instance_id"`
}

type providerIdentities struct {
	mu sync.Mutex
	path string
	records map[string]providerIdentity
}

func openProviderIdentities(path string) (*providerIdentities, error) {
	store := &providerIdentities{path: path, records: make(map[string]providerIdentity)}
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(contents, &store.records); err != nil {
		return nil, fmt.Errorf("invalid stored provider IDs: %w", err)
	}
	if store.records == nil {
		store.records = make(map[string]providerIdentity)
	}
	return store, nil
}

func defaultProviderIdentityPath() string {
	return filepath.Join(".urnetwork", "connectctl_providers.json")
}

func (s *providerIdentities) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// Windows does not atomically replace an existing destination.
		// Keep the previous file intact on failure.
		os.Remove(tmp)
		return fmt.Errorf("replace provider ID store: %w", err)
	}
	return nil
}

func (s *providerIdentities) loadOrCreate(index int) (providerIdentity, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strconv.Itoa(index)
	record := s.records[key]
	if record.InstanceID == "" {
		record.InstanceID = connect.NewId().String()
		s.records[key] = record
		if err := s.saveLocked(); err != nil {
			return providerIdentity{}, err
		}
	} else if _, err := connect.ParseId(record.InstanceID); err != nil {
		return providerIdentity{}, fmt.Errorf("stored instance %d: %w", index, err)
	}
	if record.ClientID != "" {
		if _, err := connect.ParseId(record.ClientID); err != nil {
			return providerIdentity{}, fmt.Errorf("stored client %d: %w", index, err)
		}
	}
	return record, nil
}

func (s *providerIdentities) recordClient(index int, expected providerIdentity, clientID connect.Id) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strconv.Itoa(index)
	now := s.records[key]
	if now.InstanceID != expected.InstanceID {
		return errors.New("concurrent provider state change")
	}
	if now.ClientID != "" && now.ClientID != clientID.String() {
		return errors.New("server returned a different client ID for this slot")
	}
	now.ClientID = clientID.String()
	s.records[key] = now
	return s.saveLocked()
}
