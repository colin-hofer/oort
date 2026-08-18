package db

import (
	"strings"
	"testing"
)

func TestMigrationChecksum(t *testing.T) {
	checksum := make([]byte, 32)
	if err := checkMigrationChecksum(1, checksum, checksum); err != nil {
		t.Fatal(err)
	}
	if err := checkMigrationChecksum(1, nil, checksum); err == nil || !strings.Contains(err.Error(), "oort platform reset --yes") {
		t.Fatalf("missing checksum did not return reset guidance: %v", err)
	}
	changed := append([]byte(nil), checksum...)
	changed[0] = 1
	if err := checkMigrationChecksum(1, changed, checksum); err == nil || !strings.Contains(err.Error(), "changed after it was applied") {
		t.Fatalf("changed migration was not rejected: %v", err)
	}
}
