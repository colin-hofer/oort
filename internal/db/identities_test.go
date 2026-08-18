package db

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestAppOIDCAttemptRoundTrip(t *testing.T) {
	if os.Getenv("OORT_INTEGRATION") != "1" {
		t.Skip("set OORT_INTEGRATION=1 to run the PostgreSQL-backed identity test")
	}
	database, err := Open(context.Background(), os.Getenv("OORT_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := Migrate(context.Background(), database); err != nil {
		t.Fatal(err)
	}
	state, err := CreateAppOIDCAttempt(context.Background(), database, "nonce", "verifier", "acme", "sales", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := ConsumeOIDCAttempt(context.Background(), database, state)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.AppTenantSlug == nil || *attempt.AppTenantSlug != "acme" ||
		attempt.AppSlug == nil || *attempt.AppSlug != "sales" || attempt.CLIReturnURL != nil || attempt.InvitationID != nil {
		t.Fatalf("unexpected app OIDC attempt: %+v", attempt)
	}
}
