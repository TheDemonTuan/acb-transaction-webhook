package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Store) Backup(ctx context.Context, destination string) error {
	if !filepath.IsAbs(destination) {
		return errors.New("backup destination must be absolute")
	}
	if _, err := os.Stat(destination); err == nil {
		return errors.New("backup destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect backup destination: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	escaped := strings.ReplaceAll(filepath.ToSlash(destination), "'", "''")
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+escaped+"'"); err != nil {
		_ = os.Remove(destination)
		return fmt.Errorf("backup database: %w", err)
	}
	if err := os.Chmod(destination, 0o600); err != nil {
		return fmt.Errorf("restrict backup permissions: %w", err)
	}
	return nil
}
