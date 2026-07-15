package localusage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/burnban/burnban/sourceadapter"
)

// Cursor compatibility was checked by static inspection of Cursor 3.11.13,
// official stable build 3f21b08f0b436a07be29fbfe00b304fa15553353, on
// 2026-07-13. That build persists ordered headers in composerData:<id> and each
// full message at bubbleId:<composer-id>:<bubble-id> in cursorDiskKV. Burnban
// requires that exact key/header/message binding. The older embedded
// conversation layout remains supported only when it supplies an exact
// model/time/token association. The private schema has no trustworthy cache or
// reasoning decomposition, so emitted usage is deliberately partial and
// unknown layouts are skipped rather than inferred.
const (
	cursorMalformedWarning   = "one or more Cursor composer usage records were malformed or unsupported"
	cursorOversizedWarning   = "one or more Cursor composer records exceeded the record size limit"
	cursorAssociationWarning = "one or more Cursor usage records lacked an exact model or timestamp association"
	cursorMaxSourceIDBytes   = 256
	cursorMaxTimestampBytes  = 64
	cursorComposerBatchSize  = 32
)

type cursorTurn struct {
	model string
	at    time.Time
}

type cursorComposer struct {
	rowID    int64
	layout   int
	sourceID string
	digest   [sha256.Size]byte
}

const (
	cursorLayoutLegacy  = 1
	cursorLayoutCurrent = 2
)

// DefaultCursorDB returns Cursor's per-user global state database without
// consulting or starting the application.
func DefaultCursorDB(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "Cursor", "User", "globalStorage", "state.vscdb")
		}
		return filepath.Join(home, "AppData", "Roaming", "Cursor", "User", "globalStorage", "state.vscdb")
	default:
		config := os.Getenv("XDG_CONFIG_HOME")
		if config == "" {
			config = filepath.Join(home, ".config")
		}
		return filepath.Join(config, "Cursor", "User", "globalStorage", "state.vscdb")
	}
}

// ScanCursor projects only tightly bounded, type-checked usage metadata. Raw
// composer and usage identifiers are hashed inside the reader and never enter
// normalized events; prompt, response, file, terminal, and tool fields are
// never returned from SQLite to the Go scanner.
func ScanCursor(path string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanCursor(path, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanCursor(path string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	limits = normalizeScanLimits(limits)
	result := ScanResult{}
	stats, ready, err := preflightSQLiteSource(path, limits)
	if err != nil {
		return result, fmt.Errorf("cursor database: %w", err)
	}
	result.Stats = stats
	if !ready {
		return result, nil
	}

	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
	if err != nil {
		return result, fmt.Errorf("cursor database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), limits.MaxDuration)
	defer cancel()

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, fmt.Errorf("cursor database: %w", err)
	}
	defer tx.Rollback()
	var hasTable int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='cursorDiskKV'`).Scan(&hasTable); err != nil {
		return result, fmt.Errorf("cursor schema: %w", err)
	}
	if hasTable != 1 {
		return result, fmt.Errorf("cursor database has no supported cursorDiskKV table")
	}

	composers, err := cursorComposers(ctx, tx, limits, &result.Stats)
	if err != nil {
		return result, fmt.Errorf("cursor composer metadata: %w", err)
	}
	seen := map[[sha256.Size]byte]struct{}{}
	sessions := map[[sha256.Size]byte]struct{}{}
	for _, layout := range []int{cursorLayoutCurrent, cursorLayoutLegacy} {
		selected := make([]cursorComposer, 0, len(composers))
		for _, composer := range composers {
			if composer.layout == layout {
				selected = append(selected, composer)
			}
		}
		for start := 0; start < len(selected); start += cursorComposerBatchSize {
			end := min(start+cursorComposerBatchSize, len(selected))
			var limitReached bool
			if layout == cursorLayoutCurrent {
				limitReached, err = scanCursorCurrentBatch(ctx, tx, selected[start:end], since, limits, &result.Stats, seen, sessions, emit)
			} else {
				limitReached, err = scanCursorLegacyBatch(ctx, tx, selected[start:end], since, limits, &result.Stats, seen, sessions, emit)
			}
			if err != nil {
				if ctx.Err() != nil {
					result.Stats.Warn("scan time limit reached")
				}
				return result, fmt.Errorf("cursor composer metadata: %w", err)
			}
			if limitReached {
				break
			}
		}
		if result.Stats.RecordsScanned >= limits.MaxRecords {
			break
		}
	}
	if ctx.Err() != nil {
		result.Stats.Warn("scan time limit reached")
		return result, fmt.Errorf("cursor composer metadata: %w", ctx.Err())
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("cursor composer metadata: %w", err)
	}
	result.Sessions = len(sessions)
	return result, nil
}

func cursorComposers(ctx context.Context, tx *sql.Tx, limits ScanLimits, stats *ScanStats) ([]cursorComposer, error) {
	rows, err := tx.QueryContext(ctx, `SELECT rowid,
		CASE WHEN typeof(key)='text' AND length(CAST(key AS BLOB)) BETWEEN 1 AND ? THEN key END,
		COALESCE(length(CAST(value AS BLOB)),-1),
		CASE
			WHEN COALESCE(length(CAST(value AS BLOB)),-1) > ? THEN -1
			WHEN COALESCE(length(CAST(value AS BLOB)),-1) < 0 THEN 0
			WHEN json_valid(CAST(value AS TEXT)) THEN CASE
				WHEN json_type(CAST(value AS TEXT),'$.fullConversationHeadersOnly')='array' THEN 2
				WHEN json_type(CAST(value AS TEXT),'$.conversation')='array' THEN 1
				ELSE 0 END
			ELSE 0
		END,
		CASE
			WHEN COALESCE(length(CAST(value AS BLOB)),-1) BETWEEN 0 AND ?
				AND json_valid(CAST(value AS TEXT))
				AND json_type(CAST(value AS TEXT),'$.fullConversationHeadersOnly')='array'
			THEN CASE
				WHEN json_type(CAST(value AS TEXT),'$.composerId')='text'
					AND length(CAST(json_extract(CAST(value AS TEXT),'$.composerId') AS BLOB)) BETWEEN 1 AND ?
				THEN 1 ELSE 2 END
			ELSE 0
		END,
		CASE
			WHEN COALESCE(length(CAST(value AS BLOB)),-1) BETWEEN 0 AND ?
				AND json_valid(CAST(value AS TEXT))
				AND json_type(CAST(value AS TEXT),'$.fullConversationHeadersOnly')='array'
				AND json_type(CAST(value AS TEXT),'$.composerId')='text'
				AND length(CAST(json_extract(CAST(value AS TEXT),'$.composerId') AS BLOB)) BETWEEN 1 AND ?
			THEN json_extract(CAST(value AS TEXT),'$.composerId')
		END
		FROM cursorDiskKV
		WHERE typeof(key)='text' AND key GLOB 'composerData:*'
		ORDER BY rowid LIMIT ?`, len("composerData:")+cursorMaxSourceIDBytes, limits.MaxLineBytes,
		limits.MaxLineBytes, cursorMaxSourceIDBytes, limits.MaxLineBytes, cursorMaxSourceIDBytes,
		limits.MaxRecords+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	composers := make([]cursorComposer, 0)
	inspected := 0
	for rows.Next() {
		if inspected >= limits.MaxRecords {
			stats.Warn("record scan limit reached")
			break
		}
		inspected++
		var rowID, recordBytes int64
		var sourceID, storedComposerID sql.NullString
		var recordState, storedComposerState int
		if err := rows.Scan(&rowID, &sourceID, &recordBytes, &recordState, &storedComposerState, &storedComposerID); err != nil {
			return nil, err
		}
		if recordState < 0 {
			stats.Warn(cursorOversizedWarning)
			continue
		}
		if recordState != cursorLayoutLegacy && recordState != cursorLayoutCurrent ||
			!sourceID.Valid || !cursorSafeText(sourceID.String, len("composerData:")+cursorMaxSourceIDBytes) ||
			!strings.HasPrefix(sourceID.String, "composerData:") || recordBytes < 0 {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		composerID := strings.TrimPrefix(sourceID.String, "composerData:")
		if composerID == "" || !cursorSafeText(composerID, cursorMaxSourceIDBytes) ||
			recordState == cursorLayoutCurrent && strings.Contains(composerID, ":") {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		if recordState == cursorLayoutCurrent && (storedComposerState != 1 || !storedComposerID.Valid ||
			storedComposerID.String != composerID || !cursorSafeText(storedComposerID.String, cursorMaxSourceIDBytes)) {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		composers = append(composers, cursorComposer{
			rowID: rowID, layout: recordState, sourceID: composerID,
			digest: cursorDigest("composer", []byte(sourceID.String)),
		})
	}
	return composers, rows.Err()
}

type cursorPendingEvent struct {
	digest [sha256.Size]byte
	event  Event
}

func scanCursorCurrentBatch(
	ctx context.Context,
	tx *sql.Tx,
	composers []cursorComposer,
	since time.Time,
	limits ScanLimits,
	stats *ScanStats,
	seen map[[sha256.Size]byte]struct{},
	sessions map[[sha256.Size]byte]struct{},
	emit func(Event),
) (bool, error) {
	remaining := limits.MaxRecords - stats.RecordsScanned
	if remaining <= 0 {
		stats.Warn("record scan limit reached")
		return true, nil
	}
	args := make([]any, 0, len(composers)*3+1)
	for ord, composer := range composers {
		args = append(args, ord, composer.rowID, composer.sourceID)
	}
	args = append(args, remaining+1)
	rows, err := tx.QueryContext(ctx, cursorCurrentMetadataQuery(len(composers), limits.MaxLineBytes), args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	currentOrd := -1
	var currentComposer cursorComposer
	var turn cursorTurn
	var expectedIndex int64
	headerIDs := make(map[[sha256.Size]byte]struct{})
	pendingIDs := make(map[[sha256.Size]byte]struct{})
	pending := make([]cursorPendingEvent, 0)
	flush := func() {
		for _, item := range pending {
			if _, duplicate := seen[item.digest]; duplicate {
				continue
			}
			seen[item.digest] = struct{}{}
			sessions[currentComposer.digest] = struct{}{}
			emit(item.event)
		}
		pending = pending[:0]
		clear(pendingIDs)
	}
	resetComposer := func(ord int) {
		currentOrd = ord
		currentComposer = composers[ord]
		turn = cursorTurn{}
		expectedIndex = 0
		clear(headerIDs)
		clear(pendingIDs)
		pending = pending[:0]
	}

	for rows.Next() {
		if stats.RecordsScanned >= limits.MaxRecords {
			flush()
			stats.Warn("record scan limit reached")
			return true, nil
		}
		var ord int
		var rowID, index int64
		var headerValid, headerBubbleState, headerAtState, messageState int
		var messageBubbleState, usageState, modelState, messageAtState, inputState, outputState int
		var headerRole, messageRole sql.NullInt64
		var headerBubble, headerAt, messageBubble, usageID, model, messageAt sql.NullString
		var input, output sql.NullInt64
		if err := rows.Scan(
			&ord, &rowID, &index, &headerValid, &headerRole,
			&headerBubbleState, &headerBubble, &headerAtState, &headerAt,
			&messageState, &messageRole, &messageBubbleState, &messageBubble,
			&usageState, &usageID, &modelState, &model, &messageAtState, &messageAt,
			&inputState, &input, &outputState, &output,
		); err != nil {
			return false, err
		}
		stats.RecordsScanned++
		if ord < 0 || ord >= len(composers) || rowID != composers[ord].rowID || ord < currentOrd {
			stats.Warn(cursorMalformedWarning)
			return false, nil
		}
		if ord != currentOrd {
			if currentOrd >= 0 {
				flush()
			}
			resetComposer(ord)
		}
		if index != expectedIndex {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			expectedIndex = index + 1
			continue
		}
		expectedIndex++

		if headerValid != 1 || !headerRole.Valid || headerBubbleState != 1 || !headerBubble.Valid ||
			!cursorSafeText(headerBubble.String, cursorMaxSourceIDBytes) || strings.Contains(headerBubble.String, ":") {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		headerDigest := cursorDigest("bubble", currentComposer.digest[:], []byte(headerBubble.String))
		if _, duplicate := headerIDs[headerDigest]; duplicate {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		headerIDs[headerDigest] = struct{}{}
		if messageState != 1 {
			if messageState < 0 {
				stats.Warn(cursorOversizedWarning)
			} else if messageState == 0 {
				stats.Warn(cursorAssociationWarning)
			} else {
				stats.Warn(cursorMalformedWarning)
			}
			turn = cursorTurn{}
			continue
		}
		if !messageRole.Valid || messageRole.Int64 != headerRole.Int64 ||
			messageBubbleState != 1 || !messageBubble.Valid || messageBubble.String != headerBubble.String ||
			!cursorSafeText(messageBubble.String, cursorMaxSourceIDBytes) {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}

		switch headerRole.Int64 {
		case 1:
			parsedModel, modelOK := cursorAssociatedModel(modelState, model)
			parsedAt, atPresent, atOK := cursorCurrentAssociatedTime(headerAtState, headerAt, messageAtState, messageAt, time.Time{})
			turn = cursorTurn{}
			if !modelOK || !atOK {
				if modelState == 2 || !atOK {
					stats.Warn(cursorMalformedWarning)
				} else {
					stats.Warn(cursorAssociationWarning)
				}
				continue
			}
			if !atPresent {
				stats.Warn(cursorAssociationWarning)
				continue
			}
			turn = cursorTurn{model: parsedModel, at: parsedAt}
			continue
		case 2:
		default:
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}

		if inputState == 0 && outputState == 0 {
			continue
		}
		if inputState != 1 || outputState != 1 || !input.Valid || !output.Valid ||
			input.Int64 > sourceadapter.MaxEventTokens-output.Int64 {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		associated := turn
		if modelState == 2 {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		if modelState == 1 {
			parsedModel, ok := cursorAssociatedModel(modelState, model)
			if !ok {
				stats.Warn(cursorMalformedWarning)
				turn = cursorTurn{}
				continue
			}
			associated.model = parsedModel
		}
		parsedAt, atPresent, atOK := cursorCurrentAssociatedTime(headerAtState, headerAt, messageAtState, messageAt, associated.at)
		if !atOK {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		if atPresent {
			associated.at = parsedAt
		}
		if associated.model == "" || associated.at.IsZero() {
			stats.Warn(cursorAssociationWarning)
			continue
		}
		if associated.at.Before(since) || input.Int64 == 0 && output.Int64 == 0 {
			continue
		}
		stableID, ok := cursorStableSourceID(usageState, usageID, messageBubbleState, messageBubble)
		if !ok {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		digest := cursorDigest("event", currentComposer.digest[:], []byte(stableID))
		if _, duplicate := seen[digest]; duplicate {
			continue
		}
		if _, duplicate := pendingIDs[digest]; duplicate {
			continue
		}
		event := Event{
			ID: "cursor:" + hex.EncodeToString(digest[:]), Provider: "cursor", Model: associated.model, Time: associated.at,
			Calls: 1, In: input.Int64, Out: output.Int64,
			Confidence: sourceadapter.ConfidencePartial,
		}
		if err := event.Validate(); err != nil {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		pendingIDs[digest] = struct{}{}
		pending = append(pending, cursorPendingEvent{digest: digest, event: event})
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if currentOrd >= 0 {
		flush()
	}
	return false, nil
}

func cursorCurrentMetadataQuery(composers, maxLineBytes int) string {
	values := strings.TrimSuffix(strings.Repeat("(?,?,?),", composers), ",")
	return fmt.Sprintf(`WITH selected(ord,rowid,composer_id) AS (VALUES %[1]s), headers AS (
		SELECT s.ord,d.rowid AS composer_rowid,s.composer_id,CAST(j.key AS INTEGER) AS idx,
			CASE WHEN j.type='object' AND length(CAST(j.value AS BLOB)) BETWEEN 2 AND %[2]d THEN 1 ELSE 0 END AS header_valid,
			CASE WHEN j.type='object' AND length(CAST(j.value AS BLOB)) BETWEEN 2 AND %[2]d THEN j.value ELSE '{}' END AS header_json
		FROM selected s JOIN cursorDiskKV d ON d.rowid=s.rowid
			AND typeof(d.key)='text' AND d.key='composerData:' || s.composer_id
		CROSS JOIN json_each(CAST(d.value AS TEXT),'$.fullConversationHeadersOnly') j
	), header_typed AS (
		SELECT *,json_type(header_json,'$.type') AS header_role_type,
			json_type(header_json,'$.bubbleId') AS header_bubble_type,
			json_type(header_json,'$.createdAt') AS header_at_type
		FROM headers
	), joined AS (
		SELECT h.*,m.rowid AS message_rowid,m.value AS message_value,
			COALESCE(length(CAST(m.value AS BLOB)),-1) AS message_bytes
		FROM header_typed h LEFT JOIN cursorDiskKV m
			ON h.header_bubble_type='text'
			AND length(CAST(json_extract(h.header_json,'$.bubbleId') AS BLOB)) BETWEEN 1 AND %[3]d
			AND instr(json_extract(h.header_json,'$.bubbleId'),':')=0
			AND typeof(m.key)='text'
			AND m.key='bubbleId:' || h.composer_id || ':' || json_extract(h.header_json,'$.bubbleId')
	), messages AS (
		SELECT *,CASE
			WHEN message_rowid IS NULL THEN 0
			WHEN message_bytes > %[2]d THEN -1
			WHEN message_bytes < 0 THEN 2
			WHEN json_valid(CAST(message_value AS TEXT))
				AND json_type(CAST(message_value AS TEXT),'$')='object' THEN 1
			ELSE 2 END AS message_state,
			CASE WHEN message_bytes BETWEEN 0 AND %[2]d
				AND json_valid(CAST(message_value AS TEXT))
				AND json_type(CAST(message_value AS TEXT),'$')='object'
				THEN CAST(message_value AS TEXT) ELSE '{}' END AS message_json
		FROM joined
	), typed AS (
		SELECT *,json_type(message_json,'$.type') AS message_role_type,
			json_type(message_json,'$.bubbleId') AS message_bubble_type,
			json_type(message_json,'$.usageUuid') AS usage_type,
			json_type(message_json,'$.modelInfo.modelName') AS model_type,
			json_type(message_json,'$.createdAt') AS message_at_type,
			json_type(message_json,'$.tokenCount.inputTokens') AS input_type,
			json_type(message_json,'$.tokenCount.outputTokens') AS output_type
		FROM messages
	)
	SELECT ord,composer_rowid,idx,header_valid,
		CASE WHEN header_role_type='integer' AND json_extract(header_json,'$.type') BETWEEN -2147483648 AND 2147483647
			THEN json_extract(header_json,'$.type') END,
		CASE WHEN header_bubble_type IS NULL THEN 0
			WHEN header_bubble_type='text' AND length(CAST(json_extract(header_json,'$.bubbleId') AS BLOB)) BETWEEN 1 AND %[3]d THEN 1 ELSE 2 END,
		CASE WHEN header_bubble_type='text' AND length(CAST(json_extract(header_json,'$.bubbleId') AS BLOB)) BETWEEN 1 AND %[3]d
			THEN json_extract(header_json,'$.bubbleId') END,
		CASE WHEN header_at_type IS NULL THEN 0
			WHEN header_at_type='text' AND length(CAST(json_extract(header_json,'$.createdAt') AS BLOB)) BETWEEN 1 AND %[4]d THEN 1
			WHEN header_at_type IN ('integer','real') AND length(CAST(json_extract(header_json,'$.createdAt') AS TEXT)) BETWEEN 1 AND %[4]d THEN 3 ELSE 2 END,
		CASE WHEN (header_at_type='text' AND length(CAST(json_extract(header_json,'$.createdAt') AS BLOB)) BETWEEN 1 AND %[4]d)
			OR (header_at_type IN ('integer','real') AND length(CAST(json_extract(header_json,'$.createdAt') AS TEXT)) BETWEEN 1 AND %[4]d)
			THEN CAST(json_extract(header_json,'$.createdAt') AS TEXT) END,
		message_state,
		CASE WHEN message_role_type='integer' AND json_extract(message_json,'$.type') BETWEEN -2147483648 AND 2147483647
			THEN json_extract(message_json,'$.type') END,
		CASE WHEN message_bubble_type IS NULL THEN 0
			WHEN message_bubble_type='text' AND length(CAST(json_extract(message_json,'$.bubbleId') AS BLOB)) BETWEEN 1 AND %[3]d THEN 1 ELSE 2 END,
		CASE WHEN message_bubble_type='text' AND length(CAST(json_extract(message_json,'$.bubbleId') AS BLOB)) BETWEEN 1 AND %[3]d
			THEN json_extract(message_json,'$.bubbleId') END,
		CASE WHEN usage_type IS NULL THEN 0
			WHEN usage_type='text' AND length(CAST(json_extract(message_json,'$.usageUuid') AS BLOB)) BETWEEN 1 AND %[3]d THEN 1 ELSE 2 END,
		CASE WHEN usage_type='text' AND length(CAST(json_extract(message_json,'$.usageUuid') AS BLOB)) BETWEEN 1 AND %[3]d
			THEN json_extract(message_json,'$.usageUuid') END,
		CASE WHEN model_type IS NULL THEN 0
			WHEN model_type='text' AND length(CAST(json_extract(message_json,'$.modelInfo.modelName') AS BLOB)) BETWEEN 1 AND %[5]d THEN 1 ELSE 2 END,
		CASE WHEN model_type='text' AND length(CAST(json_extract(message_json,'$.modelInfo.modelName') AS BLOB)) BETWEEN 1 AND %[5]d
			THEN json_extract(message_json,'$.modelInfo.modelName') END,
		CASE WHEN message_at_type IS NULL THEN 0
			WHEN message_at_type='text' AND length(CAST(json_extract(message_json,'$.createdAt') AS BLOB)) BETWEEN 1 AND %[4]d THEN 1
			WHEN message_at_type IN ('integer','real') AND length(CAST(json_extract(message_json,'$.createdAt') AS TEXT)) BETWEEN 1 AND %[4]d THEN 3 ELSE 2 END,
		CASE WHEN (message_at_type='text' AND length(CAST(json_extract(message_json,'$.createdAt') AS BLOB)) BETWEEN 1 AND %[4]d)
			OR (message_at_type IN ('integer','real') AND length(CAST(json_extract(message_json,'$.createdAt') AS TEXT)) BETWEEN 1 AND %[4]d)
			THEN CAST(json_extract(message_json,'$.createdAt') AS TEXT) END,
		CASE WHEN input_type IS NULL THEN 0
			WHEN input_type='integer' AND json_extract(message_json,'$.tokenCount.inputTokens') BETWEEN 0 AND %[6]d THEN 1 ELSE 2 END,
		CASE WHEN input_type='integer' AND json_extract(message_json,'$.tokenCount.inputTokens') BETWEEN 0 AND %[6]d
			THEN json_extract(message_json,'$.tokenCount.inputTokens') END,
		CASE WHEN output_type IS NULL THEN 0
			WHEN output_type='integer' AND json_extract(message_json,'$.tokenCount.outputTokens') BETWEEN 0 AND %[6]d THEN 1 ELSE 2 END,
		CASE WHEN output_type='integer' AND json_extract(message_json,'$.tokenCount.outputTokens') BETWEEN 0 AND %[6]d
			THEN json_extract(message_json,'$.tokenCount.outputTokens') END
	FROM typed ORDER BY ord,idx,message_rowid LIMIT ?`, values, maxLineBytes, cursorMaxSourceIDBytes,
		cursorMaxTimestampBytes, sourceadapter.MaxEventModelBytes, sourceadapter.MaxEventTokens)
}

func cursorCurrentAssociatedTime(
	headerState int,
	headerValue sql.NullString,
	messageState int,
	messageValue sql.NullString,
	fallback time.Time,
) (time.Time, bool, bool) {
	if headerState == 2 || messageState == 2 {
		return time.Time{}, false, false
	}
	var headerAt, messageAt time.Time
	headerPresent := headerState == 1 || headerState == 3
	messagePresent := messageState == 1 || messageState == 3
	if headerPresent {
		var ok bool
		headerAt, ok = cursorAssociatedTime(headerState, headerValue)
		if !ok {
			return time.Time{}, false, false
		}
	}
	if messagePresent {
		var ok bool
		messageAt, ok = cursorAssociatedTime(messageState, messageValue)
		if !ok {
			return time.Time{}, false, false
		}
	}
	if headerPresent && messagePresent && !headerAt.Equal(messageAt) {
		return time.Time{}, false, false
	}
	if messagePresent {
		return messageAt, true, true
	}
	if headerPresent {
		return headerAt, true, true
	}
	if !fallback.IsZero() {
		return fallback, true, true
	}
	return time.Time{}, false, true
}

func scanCursorLegacyBatch(
	ctx context.Context,
	tx *sql.Tx,
	composers []cursorComposer,
	since time.Time,
	limits ScanLimits,
	stats *ScanStats,
	seen map[[sha256.Size]byte]struct{},
	sessions map[[sha256.Size]byte]struct{},
	emit func(Event),
) (bool, error) {
	remaining := limits.MaxRecords - stats.RecordsScanned
	if remaining <= 0 {
		stats.Warn("record scan limit reached")
		return true, nil
	}
	args := make([]any, 0, len(composers)*2+1)
	for ord, composer := range composers {
		args = append(args, ord, composer.rowID)
	}
	args = append(args, remaining+1)
	rows, err := tx.QueryContext(ctx, cursorLegacyMetadataQuery(len(composers)), args...)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	var turn cursorTurn
	currentOrd := -1
	var currentComposer cursorComposer
	var expectedIndex int64
	for rows.Next() {
		if stats.RecordsScanned >= limits.MaxRecords {
			stats.Warn("record scan limit reached")
			return true, nil
		}
		stats.RecordsScanned++
		var ord int
		var rowID, index int64
		var elementValid int
		var role sql.NullInt64
		var bubbleState, usageState, modelState, atState, inputState, outputState int
		var bubbleID, usageID, model, atRaw sql.NullString
		var input, output sql.NullInt64
		if err := rows.Scan(
			&ord, &rowID, &index, &elementValid, &role,
			&bubbleState, &bubbleID, &usageState, &usageID,
			&modelState, &model, &atState, &atRaw,
			&inputState, &input, &outputState, &output,
		); err != nil {
			return false, err
		}
		if ord < 0 || ord >= len(composers) || rowID != composers[ord].rowID || ord < currentOrd {
			stats.Warn(cursorMalformedWarning)
			return false, nil
		}
		if ord != currentOrd {
			currentOrd = ord
			currentComposer = composers[ord]
			turn = cursorTurn{}
			expectedIndex = 0
		}
		if index != expectedIndex {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			expectedIndex = index + 1
			continue
		}
		expectedIndex++
		if elementValid != 1 {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		if !role.Valid {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		if role.Int64 == 1 {
			turn = cursorTurn{}
			parsedModel, modelOK := cursorAssociatedModel(modelState, model)
			parsedAt, atOK := cursorAssociatedTime(atState, atRaw)
			if modelState == 2 || atState == 2 || (modelState == 1 && !modelOK) ||
				((atState == 1 || atState == 3) && !atOK) {
				stats.Warn(cursorMalformedWarning)
			}
			if modelOK && atOK {
				turn = cursorTurn{model: parsedModel, at: parsedAt}
			}
			continue
		}
		if role.Int64 != 2 {
			stats.Warn(cursorMalformedWarning)
			turn = cursorTurn{}
			continue
		}
		if inputState == 0 && outputState == 0 {
			continue
		}
		if inputState != 1 || outputState != 1 || !input.Valid || !output.Valid ||
			input.Int64 > sourceadapter.MaxEventTokens-output.Int64 {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		associated := turn
		if modelState == 2 || atState == 2 {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		if modelState == 1 {
			parsedModel, ok := cursorAssociatedModel(modelState, model)
			if !ok {
				stats.Warn(cursorMalformedWarning)
				continue
			}
			associated.model = parsedModel
		}
		if atState == 1 || atState == 3 {
			parsedAt, ok := cursorAssociatedTime(atState, atRaw)
			if !ok {
				stats.Warn(cursorMalformedWarning)
				continue
			}
			associated.at = parsedAt
		}
		if associated.model == "" || associated.at.IsZero() {
			stats.Warn(cursorAssociationWarning)
			continue
		}
		if associated.at.Before(since) || input.Int64 == 0 && output.Int64 == 0 {
			continue
		}
		stableID, ok := cursorStableSourceID(usageState, usageID, bubbleState, bubbleID)
		if !ok {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		digest := cursorDigest("event", currentComposer.digest[:], []byte(stableID))
		if _, duplicate := seen[digest]; duplicate {
			continue
		}
		event := Event{
			ID: "cursor:" + hex.EncodeToString(digest[:]), Provider: "cursor", Model: associated.model, Time: associated.at,
			Calls: 1, In: input.Int64, Out: output.Int64,
			Confidence: sourceadapter.ConfidencePartial,
		}
		if err := event.Validate(); err != nil {
			stats.Warn(cursorMalformedWarning)
			continue
		}
		seen[digest] = struct{}{}
		sessions[currentComposer.digest] = struct{}{}
		emit(event)
	}
	return false, rows.Err()
}

func cursorLegacyMetadataQuery(composers int) string {
	values := strings.TrimSuffix(strings.Repeat("(?,?),", composers), ",")
	return fmt.Sprintf(`WITH selected(ord,rowid) AS (VALUES %[1]s), bubbles AS (
		SELECT s.ord,d.rowid AS composer_rowid,CAST(j.key AS INTEGER) AS idx,j.type AS element_type,
			CASE WHEN j.type='object' THEN j.value ELSE '{}' END AS bubble_json
		FROM selected s JOIN cursorDiskKV d ON d.rowid=s.rowid
		CROSS JOIN json_each(CAST(d.value AS TEXT),'$.conversation') j
	), typed AS (
		SELECT ord,composer_rowid,idx,element_type,bubble_json,
			json_type(bubble_json,'$.type') AS role_type,
			json_type(bubble_json,'$.bubbleId') AS bubble_type,
			json_type(bubble_json,'$.usageUuid') AS usage_type,
			json_type(bubble_json,'$.modelInfo.modelName') AS model_type,
			json_type(bubble_json,'$.createdAt') AS at_type,
			json_type(bubble_json,'$.tokenCount.inputTokens') AS input_type,
			json_type(bubble_json,'$.tokenCount.outputTokens') AS output_type
		FROM bubbles
	)
	SELECT ord,composer_rowid,idx,CASE WHEN element_type='object' THEN 1 ELSE 0 END,
		CASE WHEN role_type='integer' AND json_extract(bubble_json,'$.type') BETWEEN -2147483648 AND 2147483647
			THEN json_extract(bubble_json,'$.type') END,
		CASE WHEN bubble_type IS NULL THEN 0
			WHEN bubble_type='text' AND length(CAST(json_extract(bubble_json,'$.bubbleId') AS BLOB)) BETWEEN 1 AND %[2]d THEN 1 ELSE 2 END,
		CASE WHEN bubble_type='text' AND length(CAST(json_extract(bubble_json,'$.bubbleId') AS BLOB)) BETWEEN 1 AND %[2]d
			THEN json_extract(bubble_json,'$.bubbleId') END,
		CASE WHEN usage_type IS NULL THEN 0
			WHEN usage_type='text' AND length(CAST(json_extract(bubble_json,'$.usageUuid') AS BLOB)) BETWEEN 1 AND %[2]d THEN 1 ELSE 2 END,
		CASE WHEN usage_type='text' AND length(CAST(json_extract(bubble_json,'$.usageUuid') AS BLOB)) BETWEEN 1 AND %[2]d
			THEN json_extract(bubble_json,'$.usageUuid') END,
		CASE WHEN model_type IS NULL THEN 0
			WHEN model_type='text' AND length(CAST(json_extract(bubble_json,'$.modelInfo.modelName') AS BLOB)) BETWEEN 1 AND %[3]d THEN 1 ELSE 2 END,
		CASE WHEN model_type='text' AND length(CAST(json_extract(bubble_json,'$.modelInfo.modelName') AS BLOB)) BETWEEN 1 AND %[3]d
			THEN json_extract(bubble_json,'$.modelInfo.modelName') END,
		CASE WHEN at_type IS NULL THEN 0
			WHEN at_type='text' AND length(CAST(json_extract(bubble_json,'$.createdAt') AS BLOB)) BETWEEN 1 AND %[4]d THEN 1
			WHEN at_type IN ('integer','real') AND length(CAST(json_extract(bubble_json,'$.createdAt') AS TEXT)) BETWEEN 1 AND %[4]d THEN 3
			ELSE 2 END,
		CASE WHEN (at_type='text' AND length(CAST(json_extract(bubble_json,'$.createdAt') AS BLOB)) BETWEEN 1 AND %[4]d)
			OR (at_type IN ('integer','real') AND length(CAST(json_extract(bubble_json,'$.createdAt') AS TEXT)) BETWEEN 1 AND %[4]d)
			THEN CAST(json_extract(bubble_json,'$.createdAt') AS TEXT) END,
		CASE WHEN input_type IS NULL THEN 0
			WHEN input_type='integer' AND json_extract(bubble_json,'$.tokenCount.inputTokens') BETWEEN 0 AND %[5]d THEN 1 ELSE 2 END,
		CASE WHEN input_type='integer' AND json_extract(bubble_json,'$.tokenCount.inputTokens') BETWEEN 0 AND %[5]d
			THEN json_extract(bubble_json,'$.tokenCount.inputTokens') END,
		CASE WHEN output_type IS NULL THEN 0
			WHEN output_type='integer' AND json_extract(bubble_json,'$.tokenCount.outputTokens') BETWEEN 0 AND %[5]d THEN 1 ELSE 2 END,
		CASE WHEN output_type='integer' AND json_extract(bubble_json,'$.tokenCount.outputTokens') BETWEEN 0 AND %[5]d
			THEN json_extract(bubble_json,'$.tokenCount.outputTokens') END
	FROM typed ORDER BY ord,idx LIMIT ?`, values, cursorMaxSourceIDBytes, sourceadapter.MaxEventModelBytes, cursorMaxTimestampBytes, sourceadapter.MaxEventTokens)
}

func cursorAssociatedModel(state int, value sql.NullString) (string, bool) {
	if state != 1 || !value.Valid {
		return "", false
	}
	model := strings.TrimSpace(value.String)
	return model, cursorSafeText(model, sourceadapter.MaxEventModelBytes)
}

func cursorAssociatedTime(state int, value sql.NullString) (time.Time, bool) {
	if !value.Valid {
		return time.Time{}, false
	}
	if state == 1 {
		parsed, err := time.Parse(time.RFC3339Nano, value.String)
		return parsed, err == nil
	}
	if state != 3 {
		return time.Time{}, false
	}
	n, err := strconv.ParseFloat(value.String, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 {
		return time.Time{}, false
	}
	if n > 1e11 {
		n /= 1000
	}
	seconds := int64(n)
	if seconds <= 0 || seconds > 32503680000 {
		return time.Time{}, false
	}
	return time.Unix(seconds, int64((n-float64(seconds))*1e9)).UTC(), true
}

func cursorStableSourceID(usageState int, usageID sql.NullString, bubbleState int, bubbleID sql.NullString) (string, bool) {
	if usageState == 2 {
		return "", false
	}
	if usageState == 1 {
		return usageID.String, usageID.Valid && cursorSafeText(usageID.String, cursorMaxSourceIDBytes)
	}
	if bubbleState != 1 {
		return "", false
	}
	return bubbleID.String, bubbleID.Valid && cursorSafeText(bubbleID.String, cursorMaxSourceIDBytes)
}

func cursorDigest(domain string, parts ...[]byte) [sha256.Size]byte {
	h := sha256.New()
	h.Write([]byte("burnban.cursor." + domain + ".v1"))
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		h.Write(size[:])
		h.Write(part)
	}
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func cursorSafeText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) {
		return false
	}
	return strings.IndexFunc(value, func(r rune) bool {
		return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
	}) < 0
}
