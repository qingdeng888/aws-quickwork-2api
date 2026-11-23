package accounts

import (
	"context"
	"path/filepath"
	"testing"

	"quickwork-openai/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "quickwork.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func seedAccounts(t *testing.T, st *store.Store) (active, disabled *store.Account) {
	t.Helper()
	ctx := context.Background()
	active = &store.Account{
		Email:        "active@example.com",
		Provider:     "social",
		TenantURL:    "https://tenant-a.example",
		WSHost:       "ws-a.example",
		AccessToken:  "access-active",
		RefreshToken: "refresh-active",
		ExpiresAt:    1_900_000_000,
	}
	if err := st.Save(ctx, active); err != nil {
		t.Fatalf("save active: %v", err)
	}
	disabled = &store.Account{
		Email:        "parked@example.com",
		Provider:     "enterprise",
		TenantURL:    "https://tenant-b.example",
		AccessToken:  "access-parked",
		RefreshToken: "refresh-parked",
		Disabled:     true,
	}
	if err := st.Save(ctx, disabled); err != nil {
		t.Fatalf("save disabled: %v", err)
	}
	return active, disabled
}

func TestExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	source := testStore(t)
	seedAccounts(t, source)

	exported, err := Export(ctx, source, true)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if exported.AccountCount != 2 || exported.DisabledCount != 1 {
		t.Fatalf("export counts = %d/%d, want 2/1", exported.AccountCount, exported.DisabledCount)
	}
	if exported.File.Version != TransferVersion {
		t.Fatalf("version = %d", exported.File.Version)
	}
	if exported.File.Accounts[0].RefreshToken == "" {
		t.Fatal("secrets missing from export")
	}

	target := testStore(t)
	res, err := Import(ctx, target, exported.File, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 2 || res.Updated != 0 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("first import = %+v", res)
	}
	list, err := target.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("stored %d accounts, want 2", len(list))
	}
	var parked *store.Account
	for i := range list {
		if list[i].Email == "parked@example.com" {
			parked = &list[i]
		}
	}
	if parked == nil || !parked.Disabled {
		t.Fatalf("disabled flag not imported: %+v", parked)
	}
	if parked.RefreshToken != "refresh-parked" {
		t.Fatalf("refresh token not imported: %q", parked.RefreshToken)
	}

	// A second import is incremental: nothing is duplicated or overwritten.
	res, err = Import(ctx, target, exported.File, false)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if res.Imported != 0 || res.Skipped != 2 {
		t.Fatalf("second import = %+v", res)
	}
	list, _ = target.List(ctx)
	if len(list) != 2 {
		t.Fatalf("second import produced %d accounts", len(list))
	}
}

func TestImportUpdateKeepsDisabled(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	_, parked := seedAccounts(t, st)

	file := &TransferFile{
		Version: TransferVersion,
		Accounts: []TransferAccount{
			{
				Email:        parked.Email,
				Provider:     parked.Provider,
				TenantURL:    parked.TenantURL,
				RefreshToken: "refresh-rotated",
				AccessToken:  "access-rotated",
			},
		},
	}
	res, err := Import(ctx, st, file, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("without update the account must be skipped: %+v", res)
	}

	res, err = Import(ctx, st, file, true)
	if err != nil {
		t.Fatalf("import update: %v", err)
	}
	if res.Updated != 1 || res.Imported != 0 {
		t.Fatalf("update import = %+v", res)
	}
	got, err := st.Get(ctx, parked.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.RefreshToken != "refresh-rotated" {
		t.Fatalf("refresh token not updated: %q", got.RefreshToken)
	}
	if !got.Disabled {
		t.Fatal("update import re-enabled a parked account")
	}
}

func TestExportWithoutSecrets(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	seedAccounts(t, st)

	exported, err := Export(ctx, st, false)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for i, account := range exported.File.Accounts {
		if account.AccessToken != "" || account.RefreshToken != "" || account.ClientSecret != "" {
			t.Fatalf("account %d leaked secrets without include_secrets", i)
		}
	}
}

func TestImportValidation(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	file := &TransferFile{
		Version: TransferVersion,
		Accounts: []TransferAccount{
			{Email: "", TenantURL: "https://x", RefreshToken: "r"},
			{Email: "no-tenant@example.com", RefreshToken: "r"},
			{Email: "no-token@example.com", TenantURL: "https://x"},
			{Email: "ok@example.com", TenantURL: "https://x", RefreshToken: "r"},
			{Email: "ok@example.com", TenantURL: "https://x", RefreshToken: "r"},
		},
	}
	res, err := Import(ctx, st, file, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Imported != 1 || res.Skipped != 1 || len(res.Errors) != 3 {
		t.Fatalf("validation result = %+v", res)
	}
	if _, err := Import(ctx, st, &TransferFile{}, false); err == nil {
		t.Fatal("empty document must be rejected")
	}
	if _, err := Import(ctx, st, &TransferFile{Version: TransferVersion + 1}, false); err == nil {
		t.Fatal("future version must be rejected")
	}
}
