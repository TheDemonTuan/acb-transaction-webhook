package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authrecovery"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func runCredentialImport(ctx context.Context) error {
	path := strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	if path == "" {
		path = "/data/gateway.db"
	}
	keyPath := strings.TrimSpace(os.Getenv("APP_MASTER_KEY_FILE"))
	if keyPath == "" || os.Getenv("APP_MASTER_KEY") != "" {
		return errors.New("RECOVERY_KEY_UNAVAILABLE")
	}
	cfg := authrecovery.Config{Enabled: true, DatabasePath: path}
	return authrecovery.RunSingleton(ctx, cfg, func(ctx context.Context) error {
		store, err := storage.OpenRuntime(ctx, path)
		if err != nil {
			return errors.New("CREDENTIALS_UNAVAILABLE")
		}
		defer store.Close()
		var checksum string
		if err := store.DB().QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=13`).Scan(&checksum); err != nil || checksum != "2026-10-02-v13-telegram-session-control" {
			return errors.New("RECOVERY_SCHEMA_REQUIRED")
		}
		kr, err := security.LoadKeyring(keyPath)
		if err != nil {
			return errors.New("RECOVERY_KEY_UNAVAILABLE")
		}
		store.WithKeyring(kr)
		connection, err := store.Connection(ctx)
		if err == nil {
			configured, err := store.HasACBCredentials(ctx, connection.ID)
			if err != nil {
				return errors.New("CREDENTIALS_UNAVAILABLE")
			}
			if configured {
				fmt.Println("CREDENTIALS_ALREADY_CONFIGURED")
				return nil
			}
		}
		credentials, err := readImportCredentials("/run/import")
		if err != nil {
			return err
		}
		_, err = store.ImportACBCredentials(ctx, credentials)
		credentials = storage.ACBCredentials{}
		if errors.Is(err, storage.ErrCredentialsAlreadyConfigured) {
			fmt.Println("CREDENTIALS_ALREADY_CONFIGURED")
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Println("CREDENTIALS_IMPORTED; login requires Telegram button")
		return nil
	})
}
func readImportCredentials(dir string) (storage.ACBCredentials, error) {
	username, err := importCredentialFile(filepath.Join(dir, "acb_username"), false)
	if err != nil {
		return storage.ACBCredentials{}, err
	}
	password, err := importCredentialFile(filepath.Join(dir, "acb_password"), true)
	if err != nil {
		return storage.ACBCredentials{}, err
	}
	account, err := importCredentialFile(filepath.Join(dir, "acb_account"), false)
	if err != nil {
		return storage.ACBCredentials{}, err
	}
	for i := range len(account) {
		if account[i] < '0' || account[i] > '9' {
			return storage.ACBCredentials{}, errors.New("INVALID_CREDENTIAL_INPUT")
		}
	}
	return storage.ACBCredentials{Username: username, Password: password, AccountNumber: account}, nil
}
func importCredentialFile(path string, password bool) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || !importFilePermissionsOK(info) {
		return "", errors.New("CREDENTIAL_IMPORT_FILE_INVALID")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("CREDENTIAL_IMPORT_FILE_INVALID")
	}
	defer clear(data)
	return parseImportCredential(data, password)
}
func parseImportCredential(data []byte, password bool) (string, error) {
	text := string(data)
	if password {
		text = strings.TrimSuffix(text, "\n")
		text = strings.TrimSuffix(text, "\r")
	} else {
		text = strings.TrimSpace(text)
	}
	max := 256
	if password {
		max = 1024
	}
	if text == "" || len(text) > max || strings.ContainsAny(text, "\r\n\x00") {
		return "", errors.New("INVALID_CREDENTIAL_INPUT")
	}
	return text, nil
}
