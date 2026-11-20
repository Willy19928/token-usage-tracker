package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"

	"tokenusage/internal/model"
	"tokenusage/internal/store"
)

func (s *Scanner) backfillRelationship(ctx context.Context, path string, cursor store.FileCursor) error {
	checked, err := s.Store.RelationshipChecked(ctx, path)
	if err != nil || checked || cursor.SessionID == "" {
		return err
	}
	meta, found, err := s.ownSessionMeta(path, cursor)
	if err != nil || !found {
		return err
	}
	if err := s.Store.PutRelationship(ctx, cursor.SessionID, model.SpawnParent(meta.Source), meta.ForkedFromID); err != nil {
		return err
	}
	return s.Store.MarkRelationshipChecked(ctx, path)
}

// backfillProvider records the model provider of a file read before the
// provider was kept.
func (s *Scanner) backfillProvider(ctx context.Context, path string, cursor store.FileCursor) error {
	checked, err := s.Store.ProviderChecked(ctx, path)
	if err != nil || checked || cursor.SessionID == "" {
		return err
	}
	meta, found, err := s.ownSessionMeta(path, cursor)
	if err != nil || !found {
		return err
	}
	if err := s.Store.PutModelProvider(ctx, cursor.SessionID, meta.ModelProvider); err != nil {
		return err
	}
	return s.Store.MarkProviderChecked(ctx, path)
}

// ownSessionMeta reads the session metadata that owns a file, within the part
// already read. found is false when the file has none.
func (s *Scanner) ownSessionMeta(path string, cursor store.FileCursor) (sessionMetaPayload, bool, error) {
	f, err := openRollout(path)
	if err != nil {
		return sessionMetaPayload{}, false, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(io.LimitReader(f, cursor.Offset), 64<<10)
	for {
		record, n, complete, large, err := readSelectiveRecord(reader, s.MaxRelevantRecord)
		if err != nil && !errors.Is(err, io.EOF) {
			return sessionMetaPayload{}, false, err
		}
		if n == 0 || !complete {
			return sessionMetaPayload{}, false, nil
		}
		if large {
			continue
		}
		var env envelope
		var meta sessionMetaPayload
		if json.Unmarshal(record, &env) != nil || env.Type != "session_meta" {
			continue
		}
		if json.Unmarshal(env.Payload, &meta) != nil {
			return sessionMetaPayload{}, false, nil
		}
		// The first metadata owns the physical file, including copied fork prefixes.
		if firstNonEmpty(meta.ID, meta.SessionID) != cursor.SessionID {
			return sessionMetaPayload{}, false, nil
		}
		return meta, true, nil
	}
}
