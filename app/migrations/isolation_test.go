package migrations

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This package sits inside app/, so importing it from a server package is a one-line
// accident that would silently relink sql-migrate and gorp into the app image. Only
// cmd/migrate and this package's test consumers may depend on it.
func TestServerBinaryDoesNotLinkSqlMigrate(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}

	out, err := exec.Command("go", "list", "-deps", "magnet-feed-sync/app").CombinedOutput()
	require.NoError(t, err, string(out))

	assert.NotContains(t, string(out), "sql-migrate",
		"the server binary must not depend on the migration library")

	migrateDeps, err := exec.Command("go", "list", "-deps", "magnet-feed-sync/cmd/migrate").CombinedOutput()
	require.NoError(t, err, string(migrateDeps))
	require.True(t, strings.Contains(string(migrateDeps), "sql-migrate"),
		"sanity check: the migrate binary is expected to depend on it")
}
