package storage

import "context"

// OpenOptions configures SQLite database initialization.
type OpenOptions struct {
	RunMigrations bool
}

// OpenWithOptions opens SQLite with explicit configuration.
func OpenWithOptions(ctx context.Context, path string, opts OpenOptions) (*Store, error) {
	return openInternal(ctx, path, opts)
}

// OpenRuntime opens SQLite in runtime mode without executing schema migrations.
func OpenRuntime(ctx context.Context, path string) (*Store, error) {
	return openInternal(ctx, path, OpenOptions{RunMigrations: false})
}
