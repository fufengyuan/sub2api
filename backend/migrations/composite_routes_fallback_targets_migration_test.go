package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompositeRoutesFallbackTargetsMigration(t *testing.T) {
	content, err := FS.ReadFile("229_composite_routes_fallback_targets.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "ALTER TABLE composite_model_routes")
	require.Contains(t, sql, "ADD COLUMN IF NOT EXISTS fallback_targets jsonb")
}
