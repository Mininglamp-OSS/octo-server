package card_template_catalog

import (
	"context"
	"fmt"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
)

const selectActiveTargetsSQL = `SELECT template_id, active_version
        FROM card_template_activation
        WHERE status = 'active'
        ORDER BY template_id ASC
        LIMIT 129`

func (s *store) ListActiveTargets(ctx context.Context) ([]startupActivationTarget, error) {
	rows, err := s.db.QueryContext(ctx, selectActiveTargetsSQL)
	if err != nil {
		return nil, fmt.Errorf("card template catalog: list active startup targets: %w", err)
	}
	defer rows.Close()
	targets := make([]startupActivationTarget, 0)
	for rows.Next() {
		var rawID, version string
		if err := rows.Scan(&rawID, &version); err != nil {
			return nil, fmt.Errorf("card template catalog: scan active startup target: %w", err)
		}
		id, validID := parseManagerTemplateID(rawID)
		if !validID || !validManagerVersion(version) {
			return nil, fmt.Errorf("%w: malformed active target identity", ErrCatalogIntegrity)
		}
		targets = append(targets, startupActivationTarget{ID: cardtmpl.ID(id), Version: version})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("card template catalog: iterate active startup targets: %w", err)
	}
	if len(targets) > startupActiveTargetLimit {
		return targets, fmt.Errorf("%w: active target count exceeds hard limit %d",
			ErrCatalogIntegrity, startupActiveTargetLimit)
	}
	return targets, nil
}
