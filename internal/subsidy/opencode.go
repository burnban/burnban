package subsidy

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/burnban/burnban/sourceadapter"
)

// OpenCode compatibility was verified against anomalyco/opencode commit
// 8168f0f0f6645a0ca741fe02e90ff532bce04148 on 2026-07-12. Released builds
// keep opencode.db in the XDG data directory. Both the legacy message table and
// the current session_message projection store assistant usage in JSON metadata.
const openCodeCheckedRevision = "8168f0f0f6645a0ca741fe02e90ff532bce04148"

const openCodeMalformedWarning = "one or more OpenCode usage records were malformed"
const openCodeRecordSizeWarning = "one or more OpenCode records exceeded the record size limit"

type openCodeLayout struct {
	table        string
	roleColumn   string
	rolePath     string
	providerPath string
	modelPath    string
}

var openCodeLayouts = []openCodeLayout{
	{
		table:        "session_message",
		roleColumn:   "type",
		providerPath: "$.model.providerID",
		modelPath:    "$.model.id",
	},
	{
		table:        "message",
		rolePath:     "$.role",
		providerPath: "$.providerID",
		modelPath:    "$.modelID",
	},
}

// DefaultOpenCodeDB returns the persistent database used by released OpenCode
// builds. OPENCODE_DB follows OpenCode's own absolute-or-XDG-relative override
// semantics; :memory: is returned unchanged and therefore is not auto-detected
// as a persistent source by Burnban.
func DefaultOpenCodeDB(home string) string {
	dataRoot := os.Getenv("XDG_DATA_HOME")
	if dataRoot == "" {
		dataRoot = filepath.Join(home, ".local", "share")
	}
	opencodeData := filepath.Join(dataRoot, "opencode")
	if configured := strings.TrimSpace(os.Getenv("OPENCODE_DB")); configured != "" {
		if configured == ":memory:" || filepath.IsAbs(configured) {
			return configured
		}
		return filepath.Join(opencodeData, configured)
	}
	return filepath.Join(opencodeData, "opencode.db")
}

// ScanOpenCode reads the metadata columns in OpenCode's SQLite store. It does
// not select prompt, response, reasoning, or tool content from either table.
func ScanOpenCode(path string, since time.Time, emit func(Event)) (int, error) {
	result, err := scanOpenCode(path, since, DefaultScanLimits(), emit)
	return result.Sessions, err
}

func scanOpenCode(path string, since time.Time, limits ScanLimits, emit func(Event)) (ScanResult, error) {
	limits = normalizeScanLimits(limits)
	result := ScanResult{}

	files, bytes, err := openCodeSourceSize(path)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, fmt.Errorf("opencode database: %w", err)
	}
	if files > limits.MaxFiles {
		result.Stats.FilesSkipped = files
		result.Stats.Warn("file scan limit reached")
		return result, nil
	}
	if bytes > limits.MaxBytes {
		result.Stats.FilesSkipped = files
		result.Stats.Warn("byte scan limit reached")
		return result, nil
	}
	result.Stats.FilesScanned = files
	result.Stats.BytesScanned = bytes

	uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	db, err := sql.Open("sqlite", uri+"?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)")
	if err != nil {
		return result, fmt.Errorf("opencode database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), limits.MaxDuration)
	defer cancel()
	tables, err := openCodeTables(ctx, db)
	if err != nil {
		if ctx.Err() != nil {
			result.Stats.Warn("scan time limit reached")
		}
		return result, fmt.Errorf("opencode schema: %w", err)
	}
	if !tables["message"] && !tables["session_message"] {
		return result, fmt.Errorf("opencode database has no supported message table")
	}

	seen := map[string]struct{}{}
	sessions := map[string]struct{}{}
	for _, layout := range openCodeLayouts {
		if !tables[layout.table] {
			continue
		}
		if err := scanOpenCodeTable(ctx, db, layout, since, limits.MaxRecords, limits.MaxLineBytes, &result.Stats, seen, sessions, emit); err != nil {
			result.Sessions = len(sessions)
			if ctx.Err() != nil {
				result.Stats.Warn("scan time limit reached")
			}
			return result, fmt.Errorf("opencode %s metadata: %w", layout.table, err)
		}
		if ctx.Err() != nil {
			result.Stats.Warn("scan time limit reached")
			return result, fmt.Errorf("opencode %s metadata: %w", layout.table, ctx.Err())
		}
	}
	result.Sessions = len(sessions)
	return result, nil
}

func openCodeSourceSize(path string) (int, int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	if info.IsDir() {
		return 0, 0, fmt.Errorf("expected a database file")
	}
	files, bytes := 1, info.Size()
	for _, suffix := range []string{"-wal", "-shm"} {
		aux, err := os.Stat(path + suffix)
		if err == nil && !aux.IsDir() {
			files++
			bytes += aux.Size()
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return 0, 0, err
		}
	}
	return files, bytes, nil
}

func openCodeTables(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name IN ('message', 'session_message')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

func openCodeJSONText(data, path string) string {
	return fmt.Sprintf(`CASE WHEN json_valid(%s) THEN
		CASE WHEN json_type(%s, '%s') = 'text' THEN json_extract(%s, '%s') END
	END`, data, data, path, data, path)
}

func openCodeJSONInteger(data, path string) string {
	return fmt.Sprintf(`CASE WHEN json_valid(%s) THEN
		CASE WHEN json_type(%s, '%s') = 'integer' THEN json_extract(%s, '%s') END
	END`, data, data, path, data, path)
}

func openCodeJSONNumber(data, path string) string {
	return fmt.Sprintf(`CASE WHEN json_valid(%s) THEN
		CASE WHEN json_type(%s, '%s') IN ('integer', 'real') THEN json_extract(%s, '%s') END
	END`, data, data, path, data, path)
}

func scanOpenCodeTable(
	ctx context.Context,
	db *sql.DB,
	layout openCodeLayout,
	since time.Time,
	maxRecords int,
	maxRecordBytes int,
	stats *ScanStats,
	seen map[string]struct{},
	sessions map[string]struct{},
	emit func(Event),
) error {
	remaining := maxRecords - stats.RecordsScanned
	if remaining < 0 {
		remaining = 0
	}
	recordLength := "length(CAST(data AS BLOB))"
	boundedData := fmt.Sprintf("CASE WHEN %s <= %d THEN data END", recordLength, maxRecordBytes)
	role := layout.roleColumn
	if role == "" {
		role = openCodeJSONText(boundedData, layout.rolePath)
	}
	query := fmt.Sprintf(`SELECT id, session_id, time_created, %s, COALESCE(json_valid(%s), 0),
		%s, %s, %s,
		%s, %s, %s, %s, %s, %s
		FROM %s WHERE time_created >= ? ORDER BY time_created, id LIMIT ?`,
		recordLength, boundedData, role, openCodeJSONText(boundedData, layout.providerPath), openCodeJSONText(boundedData, layout.modelPath),
		openCodeJSONInteger(boundedData, "$.tokens.input"), openCodeJSONInteger(boundedData, "$.tokens.output"),
		openCodeJSONInteger(boundedData, "$.tokens.reasoning"), openCodeJSONInteger(boundedData, "$.tokens.cache.read"),
		openCodeJSONInteger(boundedData, "$.tokens.cache.write"), openCodeJSONNumber(boundedData, "$.cost"), layout.table)
	rows, err := db.QueryContext(ctx, query, since.UnixMilli(), remaining+1)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		if stats.RecordsScanned >= maxRecords {
			stats.Warn("record scan limit reached")
			break
		}
		stats.RecordsScanned++
		var messageID, sessionID string
		var createdMillis, recordBytes int64
		var validJSON int
		var role, providerID, modelID sql.NullString
		var in, out, reasoning, cacheRead, cacheWrite sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(
			&messageID, &sessionID, &createdMillis, &recordBytes, &validJSON,
			&role, &providerID, &modelID, &in, &out, &reasoning, &cacheRead, &cacheWrite, &cost,
		); err != nil {
			return err
		}
		if recordBytes > int64(maxRecordBytes) {
			stats.Warn(openCodeRecordSizeWarning)
			continue
		}
		if validJSON == 0 {
			stats.Warn(openCodeMalformedWarning)
			continue
		}
		if !role.Valid || role.String != "assistant" {
			continue
		}
		if createdMillis <= 0 || messageID == "" || sessionID == "" || !providerID.Valid || strings.TrimSpace(providerID.String) == "" ||
			!modelID.Valid || strings.TrimSpace(modelID.String) == "" {
			stats.Warn(openCodeMalformedWarning)
			continue
		}
		inputTokens, inputOK := openCodeToken(in)
		outputTokens, outputOK := openCodeToken(out)
		reasoningTokens, reasoningOK := openCodeToken(reasoning)
		cacheReadTokens, cacheReadOK := openCodeToken(cacheRead)
		cacheWriteTokens, cacheWriteOK := openCodeToken(cacheWrite)
		if !inputOK || !outputOK || !reasoningOK || !cacheReadOK || !cacheWriteOK {
			stats.Warn(openCodeMalformedWarning)
			continue
		}
		if inputTokens == 0 && outputTokens == 0 && reasoningTokens == 0 && cacheReadTokens == 0 && cacheWriteTokens == 0 {
			continue
		}
		if reasoningTokens > math.MaxInt64-outputTokens {
			stats.Warn(openCodeMalformedWarning)
			continue
		}

		eventID := sessionID + "/" + messageID
		if _, duplicate := seen[eventID]; duplicate {
			continue
		}
		seen[eventID] = struct{}{}
		sessions[sessionID] = struct{}{}
		event := Event{
			ID: eventID, Provider: "opencode",
			Model: strings.TrimSpace(providerID.String) + "/" + strings.TrimSpace(modelID.String),
			Time:  time.UnixMilli(createdMillis), Calls: 1,
			In: inputTokens, Out: outputTokens + reasoningTokens,
			CacheRead: cacheReadTokens, CacheWrite5m: cacheWriteTokens,
			Confidence: sourceadapter.ConfidenceExact,
		}
		// OpenCode's cost is a model-catalog estimate, not proof that the call
		// was billed. It remains a fallback for unknown models while billing
		// classification requires an explicit --metered opencode override.
		if cost.Valid && cost.Float64 > 0 && !math.IsNaN(cost.Float64) && !math.IsInf(cost.Float64, 0) {
			event.CostUSD = cost.Float64
			event.CostKnown = true
		}
		emit(event)
	}
	return rows.Err()
}

func openCodeToken(value sql.NullInt64) (int64, bool) {
	if !value.Valid || value.Int64 < 0 {
		return 0, false
	}
	return value.Int64, true
}
