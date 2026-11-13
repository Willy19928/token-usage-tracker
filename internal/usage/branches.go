package usage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"

	"tokenusage/internal/model"
	"tokenusage/internal/store"
)

// One conversation can be spread over several rollout files that share its
// session id: Codex writes a new file when a conversation is resumed, when an
// action is undone and when a checkpoint is restored. The first snapshot of
// such a file is either the restored state again (already counted) or a new
// request sent from an earlier state (new usage on a branch). A single
// session-wide high-water mark cannot tell these apart, so the ledger keeps
// the cumulative states it has counted (store.RecordSessionPoint) and checks
// each snapshot against them. Counters that restart every turn identify their
// requests by turn instead and are left to that logic.

func sessionScoped(cursor *store.FileCursor) bool {
	return cursor.SessionID != "" && cursor.Accounting.Scope != "turn" && !cursor.Accounting.LegacyHistory
}

// requestOf returns the request a snapshot reports: the state before it and
// its size. ok is false when last_token_usage cannot be one request of total.
func requestOf(total, last model.TokenUsage) (base model.TokenUsage, ok bool) {
	if last.IsZero() || !validTokenUsage(total) || !validTokenUsage(last) || !total.MonotonicFrom(last) {
		return model.TokenUsage{}, false
	}
	base = total.Sub(last)
	return base, validTokenUsage(base)
}

// reconcileSessionSnapshot handles a snapshot whose file does not simply
// continue from its own previous snapshot, and any snapshot that another file
// already counted. It reports whether it handled the snapshot; otherwise the
// ordinary cumulative logic runs.
func (s *Scanner) reconcileSessionSnapshot(ctx context.Context, env envelope, offset int64, path, home string,
	meta model.SessionInfo, cursor *store.FileCursor, result *fileScanResult,
	rawCurrent, current, last model.TokenUsage) (bool, error) {
	if !sessionScoped(cursor) {
		return false, nil
	}
	base, ok := requestOf(rawCurrent, last)
	if !ok {
		return false, nil
	}
	if err := s.ensureSessionPoints(ctx, cursor.SessionID); err != nil {
		return false, err
	}
	sessionID := cursor.SessionID
	detached := cursor.InheritedBaseline || cursor.Cumulative.IsZero() || !current.MonotonicFrom(cursor.Cumulative)

	known, err := s.Store.SessionSnapshotKnown(ctx, sessionID, rawCurrent, last)
	if err != nil {
		return false, err
	}
	if known {
		// The same request was counted from another file. Only usage between
		// this file's own previous snapshot and that request can still be new.
		if !detached && !cursor.Cumulative.Equal(base) && base.MonotonicFrom(cursor.Cumulative) {
			baseKnown, err := s.Store.SessionStateKnown(ctx, sessionID, base)
			if err != nil {
				return false, err
			}
			if !baseKnown {
				id := stableJSONLEventID(sessionID+"\x00gap", 0, base)
				if err := s.insertSessionEvent(ctx, env, offset, path, home, meta, cursor, result, id,
					base.Sub(cursor.Cumulative), model.ConfidenceExact); err != nil {
					return false, err
				}
				if err := s.Store.RecordSessionPoint(ctx, sessionID, base, model.TokenUsage{}); err != nil {
					return false, err
				}
			} else {
				result.Duplicates++
			}
		} else {
			result.Duplicates++
		}
		return true, s.followSnapshot(ctx, cursor, current)
	}
	if !detached {
		return false, nil
	}
	baseKnown, err := s.Store.SessionStateKnown(ctx, sessionID, base)
	if err != nil || !baseKnown {
		return false, err
	}
	// A new request sent from a state already counted: an undone action
	// re-sent, or a conversation continued from a restored checkpoint.
	id := stableJSONLEventID(sessionID+"\x00branch", 0, rawCurrent)
	if err := s.insertSessionEvent(ctx, env, offset, path, home, meta, cursor, result, id, last, model.ConfidenceExact); err != nil {
		return false, err
	}
	if err := s.Store.RecordSessionPoint(ctx, sessionID, rawCurrent, last); err != nil {
		return false, err
	}
	return true, s.followSnapshot(ctx, cursor, current)
}

// followSnapshot makes this file continue from its own latest snapshot. The
// session-wide high-water mark only rises.
func (s *Scanner) followSnapshot(ctx context.Context, cursor *store.FileCursor, current model.TokenUsage) error {
	cursor.Cumulative = current
	cursor.InheritedBaseline = false
	progress, segment, ok, err := s.Store.GetSessionProgress(ctx, cursor.SessionID)
	if err != nil {
		return err
	}
	if ok && progress.Total >= current.Total {
		if segment > cursor.Segment {
			cursor.Segment = segment
		}
		return nil
	}
	return s.Store.PutSessionProgress(ctx, cursor.SessionID, cursor.Segment, current, cursor.Accounting)
}

// recordCounted remembers a snapshot that the ordinary logic counted or found
// already counted, with the state it was counted from.
func (s *Scanner) recordCounted(ctx context.Context, cursor *store.FileCursor, previous, rawCurrent, last model.TokenUsage) error {
	if !sessionScoped(cursor) {
		return nil
	}
	if _, ok := requestOf(rawCurrent, last); !ok {
		last = model.TokenUsage{}
	}
	if err := s.Store.RecordSessionPoint(ctx, cursor.SessionID, rawCurrent, last); err != nil {
		return err
	}
	return s.Store.RecordSessionPoint(ctx, cursor.SessionID, previous, model.TokenUsage{})
}

func (s *Scanner) insertSessionEvent(ctx context.Context, env envelope, offset int64, path, home string,
	meta model.SessionInfo, cursor *store.FileCursor, result *fileScanResult,
	id string, delta model.TokenUsage, confidence string) error {
	if delta.IsZero() {
		return nil
	}
	if !delta.NonNegative() {
		return &recordError{errors.New("計算出負 Token 增量: " + delta.String())}
	}
	timestamp, parseErr := parseTimestamp(env.Timestamp)
	if parseErr != nil {
		confidence = model.ConfidenceGapFallback
		if err := s.Store.AddWarning(ctx, "timestamp", path,
			"offset="+strconv.FormatInt(offset, 10)+": "+parseErr.Error()+"；事件保留為未歸屬時間"); err != nil {
			return err
		}
		result.Warnings++
	}
	event := model.UsageEvent{
		ID:          id,
		Timestamp:   timestamp,
		Segment:     cursor.Segment,
		ObservedAt:  s.Now(),
		MachineID:   s.Store.Machine().ID,
		SessionID:   cursor.SessionID,
		TurnID:      cursor.TurnID,
		Model:       firstNonEmpty(cursor.Model, meta.Model),
		Source:      firstNonEmpty(cursor.Source, meta.Source, meta.ThreadSource),
		AgentType:   firstNonEmpty(cursor.AgentType, meta.AgentType, "main"),
		ProjectPath: firstNonEmpty(cursor.ProjectPath, meta.ProjectPath),
		ThreadTitle: meta.Title,
		Usage:       delta,
		Provenance:  model.ProvenanceSessionJSONL,
		Confidence:  confidence,
		CodexHome:   home,
	}
	inserted, err := s.Store.InsertEvent(ctx, event, path)
	if err != nil {
		return err
	}
	if inserted {
		result.EventsInserted++
	} else {
		result.Duplicates++
	}
	cursor.LastEventID = event.ID
	return nil
}

// ensureSessionPoints records, once per session, the snapshots of the
// session's files that were read before session points existed.
func (s *Scanner) ensureSessionPoints(ctx context.Context, sessionID string) error {
	if s.pointsReady == nil {
		s.pointsReady = map[string]bool{}
	}
	if s.pointsReady[sessionID] {
		return nil
	}
	done, err := s.Store.SessionPointsBackfilled(ctx, sessionID)
	if err != nil {
		return err
	}
	if !done {
		files, err := s.Store.SessionFileCursors(ctx, sessionID)
		if err != nil {
			return err
		}
		for _, file := range files {
			if err := s.backfillFilePoints(ctx, sessionID, file); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := s.Store.MarkSessionPointsBackfilled(ctx, sessionID); err != nil {
			return err
		}
	}
	s.pointsReady[sessionID] = true
	return nil
}

func (s *Scanner) backfillFilePoints(ctx context.Context, sessionID string, file store.SessionFileCursor) error {
	reader, err := openRollout(file.Path)
	if err != nil {
		return err
	}
	defer reader.Close()
	buffered := bufio.NewReaderSize(io.LimitReader(reader, file.Offset), 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, consumed, complete, tooLarge, readErr := readSelectiveRecord(buffered, s.MaxRelevantRecord)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if consumed == 0 || !complete {
			return nil
		}
		if !tooLarge && len(record) > 0 {
			var env envelope
			var payload eventPayload
			var info tokenInfo
			if json.Unmarshal(record, &env) == nil && env.Type == "event_msg" &&
				json.Unmarshal(env.Payload, &payload) == nil && payload.Type == "token_count" &&
				len(payload.Info) > 0 && json.Unmarshal(payload.Info, &info) == nil {
				total, last := info.Total.usage(), info.Last.usage()
				if base, ok := requestOf(total, last); ok {
					if err := s.Store.RecordSessionPoint(ctx, sessionID, total, last); err != nil {
						return err
					}
					if err := s.Store.RecordSessionPoint(ctx, sessionID, base, model.TokenUsage{}); err != nil {
						return err
					}
				} else if validTokenUsage(total) {
					if err := s.Store.RecordSessionPoint(ctx, sessionID, total, model.TokenUsage{}); err != nil {
						return err
					}
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
	}
}
