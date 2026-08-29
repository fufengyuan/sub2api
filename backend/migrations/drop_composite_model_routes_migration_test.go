package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// composite 统一账号池取消路由机制后，路由表按确认口径彻底下线。
func TestDropCompositeModelRoutesMigration(t *testing.T) {
	content, err := FS.ReadFile("230_drop_composite_model_routes.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP TABLE IF EXISTS composite_model_routes CASCADE")
}
