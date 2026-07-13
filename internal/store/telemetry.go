package store

import (
	"fmt"
	"time"
)

// TelemetryRow is a prompt-free ledger row with its monotonic local cursor.
// The cursor is intentionally separate from Request's public JSON shape: it is
// transport state, not provider metadata.
type TelemetryRow struct {
	ID      int64
	Request Request
}

// TelemetryRowsAfter returns at most limit committed request rows newer than
// afterID. It is the bounded read surface used by the optional asynchronous
// telemetry worker; the proxy's Insert path never waits for an exporter.
func (s *Store) TelemetryRowsAfter(afterID int64, limit int) ([]TelemetryRow, error) {
	return s.telemetryRowsAfter(afterID, time.Time{}, limit)
}

// TelemetryRowsSinceAfter is the historical warehouse-export variant. The
// timestamp predicate is applied by SQLite so a short export window does not
// marshal or retain unrelated ledger rows.
func (s *Store) TelemetryRowsSinceAfter(afterID int64, since time.Time, limit int) ([]TelemetryRow, error) {
	return s.telemetryRowsAfter(afterID, since, limit)
}

func (s *Store) telemetryRowsAfter(afterID int64, since time.Time, limit int) ([]TelemetryRow, error) {
	if afterID < 0 {
		return nil, fmt.Errorf("telemetry cursor must be non-negative")
	}
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("telemetry batch size must be between 1 and 1000")
	}
	sinceText := ""
	if !since.IsZero() {
		sinceText = since.UTC().Format(time.RFC3339)
	}
	rows, err := s.db.Query(`SELECT r.id, r.ts, r.provider, r.model, r.agent,
		in_tokens, out_tokens, cache_read_tokens, cache_write_tokens,
		cache_write_1h_tokens, cost_usd, latency_ms, status, streamed, estimated,
		priced, usage_state, pricing_state, incomplete, enforcement_unsafe, route,
		service_tier, inference_geo, server_tool_calls, fee_unpriced,
		cost_source, cost_source_ref, cost_effective_from, cost_valid_through,
		cost_confidence, r.identity_tenant, r.identity_device, r.principal,
		r.service_account, r.project, r.cost_center, r.identity_confidence,
		r.requested_provider, r.requested_model, r.requested_route, r.downshift_action,
		r.downshift_rule, r.downshift_trigger, r.downshift_reason, r.downshift_config_digest,
		r.downshift_features, r.downshift_source_estimated_usd, r.downshift_target_estimated_usd,
		r.policy_decision_id, COALESCE(d.policy_digest,''),
		COALESCE(d.policy_revision,0), COALESCE(d.policy_name,''),
		COALESCE(d.policy_namespace,''), COALESCE(d.mode,''),
		COALESCE(d.outcome,''), COALESCE(d.admitted,0),
		COALESCE(d.confidence,''), COALESCE(d.context_json,'')
		FROM requests r
		LEFT JOIN policy_decisions d ON d.id = r.policy_decision_id
		WHERE r.id > ? AND (? = '' OR r.ts >= ?) ORDER BY r.id LIMIT ?`,
		afterID, sinceText, sinceText, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TelemetryRow, 0, limit)
	for rows.Next() {
		var row TelemetryRow
		var ts string
		var streamed, estimated, priced, incomplete, enforcementUnsafe, feeUnpriced int
		var policyDigest, policyName, policyNamespace, policyMode, policyOutcome, policyConfidence, policyContext string
		var policyAdmitted int
		var policyRevision int64
		r := &row.Request
		if err := rows.Scan(&row.ID, &ts, &r.Provider, &r.Model, &r.Agent,
			&r.InTokens, &r.OutTokens, &r.CacheReadTokens, &r.CacheWriteTokens,
			&r.CacheWrite1hTokens, &r.CostUSD, &r.LatencyMs, &r.Status,
			&streamed, &estimated, &priced, &r.UsageState, &r.PricingState,
			&incomplete, &enforcementUnsafe, &r.Route, &r.ServiceTier,
			&r.InferenceGeo, &r.ServerToolCalls, &feeUnpriced,
			&r.CostSource, &r.CostSourceRef, &r.CostEffectiveFrom,
			&r.CostValidThrough, &r.CostConfidence, &r.IdentityTenant,
			&r.IdentityDevice, &r.Principal, &r.ServiceAccount, &r.Project,
			&r.CostCenter, &r.IdentityConfidence,
			&r.RequestedProvider, &r.RequestedModel, &r.RequestedRoute, &r.DownshiftAction,
			&r.DownshiftRule, &r.DownshiftTrigger, &r.DownshiftReason, &r.DownshiftDigest,
			&r.DownshiftFeatures, &r.DownshiftSourceUSD, &r.DownshiftTargetUSD,
			&r.PolicyDecisionID, &policyDigest, &policyRevision, &policyName,
			&policyNamespace, &policyMode, &policyOutcome, &policyAdmitted,
			&policyConfidence, &policyContext); err != nil {
			return nil, err
		}
		r.Ts, err = time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			return nil, fmt.Errorf("parse telemetry request timestamp: %w", err)
		}
		r.Streamed, r.Estimated, r.Priced = streamed != 0, estimated != 0, priced != 0
		r.Incomplete, r.EnforcementUnsafe = incomplete != 0, enforcementUnsafe != 0
		r.FeeUnpriced = feeUnpriced != 0
		if r.PolicyDecisionID != 0 {
			r.Policy = &PolicyMetadata{
				DecisionID: r.PolicyDecisionID, Digest: policyDigest,
				Revision: policyRevision, Name: policyName, Namespace: policyNamespace,
				Mode: policyMode, Outcome: policyOutcome, Admitted: policyAdmitted != 0,
				Confidence: policyConfidence, ContextJSON: policyContext,
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// TelemetryBacklog reports the number of rows after afterID. If that count is
// larger than keep, dropThrough is the last cursor that must be explicitly
// recorded as dropped to restore the configured bound. A separate dropped
// cursor prevents overflow loss from being misreported as collector delivery.
func (s *Store) TelemetryBacklog(afterID, keep int64) (pending, dropThrough int64, err error) {
	if afterID < 0 {
		return 0, 0, fmt.Errorf("telemetry cursor must be non-negative")
	}
	if keep < 1 || keep > 10_000_000 {
		return 0, 0, fmt.Errorf("telemetry backlog bound must be between 1 and 10000000")
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM requests WHERE id > ?`, afterID).Scan(&pending); err != nil {
		return 0, 0, err
	}
	if pending <= keep {
		return pending, 0, nil
	}
	toDrop := pending - keep
	if err := s.db.QueryRow(`SELECT id FROM requests WHERE id > ? ORDER BY id LIMIT 1 OFFSET ?`,
		afterID, toDrop-1).Scan(&dropThrough); err != nil {
		return 0, 0, err
	}
	return pending, dropThrough, nil
}
