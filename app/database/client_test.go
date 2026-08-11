package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// busy_timeout is per-connection, and cmd/migrate runs on the raw *sql.DB with no retry
// wrapper: a connection that starts at 0 turns a momentarily busy database into a failed
// deploy instead of a 30s wait
func TestPragmasApplyToEveryPooledConnection(t *testing.T) {
	t.Chdir(t.TempDir())

	client, err := NewClient("test.db")
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, client.Close())
	})

	ctx := context.Background()

	for i := range 3 {
		conn, err := client.DB().Conn(ctx)
		require.NoError(t, err)
		// held open so the pool is forced to hand out a fresh connection next iteration
		defer func() {
			require.NoError(t, conn.Close())
		}()

		var timeout, foreignKeys int
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout))
		require.NoError(t, conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys))

		assert.Equal(t, 30000, timeout, "connection %d", i)
		assert.Equal(t, 1, foreignKeys, "connection %d", i)
	}

	var journalMode string
	require.NoError(t, client.DB().QueryRow("PRAGMA journal_mode").Scan(&journalMode))
	assert.Equal(t, "wal", journalMode)
}
