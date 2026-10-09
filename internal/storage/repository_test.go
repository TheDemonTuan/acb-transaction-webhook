package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEndpointLifecycle(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	e, err := s.CreateEndpoint(context.Background(), "test", "https://events.example.com/bank")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEndpointStatus(context.Background(), e.ID, "ACTIVE"); err != nil {
		t.Fatal(err)
	}
	items, err := s.Endpoints(context.Background())
	if err != nil || len(items) != 1 || items[0].Status != "ACTIVE" {
		t.Fatal(items, err)
	}
}
