package accounts

import (
	"context"
	"fmt"
	"strings"
	"time"

	"quickwork-openai/internal/store"
)

// TransferVersion is the schema version of the export/import document.
const TransferVersion = 1

// TransferFile is the JSON document produced by Export and consumed by Import.
// It is meant for moving accounts between installations or keeping an offline
// backup, so by default it contains the refresh tokens and client secrets the
// accounts need to keep working.
type TransferFile struct {
	Version    int               `json:"version"`
	ExportedAt time.Time         `json:"exported_at"`
	Accounts   []TransferAccount `json:"accounts"`
}

// TransferAccount is one exported account. Secrets are omitted from the JSON
// when the export runs with include_secrets=false.
type TransferAccount struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	Provider    string `json:"provider,omitempty"`
	BuildStage  string `json:"build_stage,omitempty"`
	Region      string `json:"region,omitempty"`

	TenantURL string `json:"tenant_url,omitempty"`
	WSHost    string `json:"ws_host,omitempty"`
	WSPath    string `json:"ws_path,omitempty"`

	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`

	ActorARN     string `json:"actor_arn,omitempty"`
	AWSAccountID string `json:"aws_account_id,omitempty"`
	AccountName  string `json:"account_name,omitempty"`

	ClientID      string `json:"client_id,omitempty"`
	ClientSecret  string `json:"client_secret,omitempty"`
	IssuerURL     string `json:"issuer_url,omitempty"`
	IDCRegion     string `json:"idc_region,omitempty"`
	AuthEndpoint  string `json:"auth_endpoint,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`

	UserInfo string `json:"user_info,omitempty"`

	ExpiresAt  int64 `json:"expires_at,omitempty"`
	ObtainedAt int64 `json:"obtained_at,omitempty"`
	CreatedAt  int64 `json:"created_at,omitempty"`
	Disabled   bool  `json:"disabled,omitempty"`
	// ProxyURL mirrors the proxy binding as a URL, because proxy ids are local
	// to one installation. Direct forces a direct connection (no pool).
	ProxyURL string `json:"proxy_url,omitempty"`
	Direct   bool   `json:"proxy_direct,omitempty"`
}

// ExportResult is one export plus the counts the caller may want to show.
type ExportResult struct {
	File          *TransferFile
	Secrets       bool
	AccountCount  int
	DisabledCount int
}

// Export dumps every stored account, including disabled ones.
func Export(ctx context.Context, st *store.Store, includeSecrets bool) (*ExportResult, error) {
	list, err := st.List(ctx)
	if err != nil {
		return nil, err
	}
	file := &TransferFile{
		Version:    TransferVersion,
		ExportedAt: time.Now().UTC(),
		Accounts:   make([]TransferAccount, 0, len(list)),
	}
	proxyURLs := make(map[int64]string)
	if proxies, err := st.ListProxies(ctx); err == nil {
		for _, proxy := range proxies {
			proxyURLs[proxy.ID] = proxy.URL
		}
	}
	res := &ExportResult{File: file, Secrets: includeSecrets, AccountCount: len(list)}
	for i := range list {
		account := &list[i]
		if account.Disabled {
			res.DisabledCount++
		}
		entry := TransferAccount{
			Email:        account.Email,
			DisplayName:  account.DisplayName,
			Provider:     account.Provider,
			BuildStage:   account.BuildStage,
			Region:       account.Region,
			TenantURL:    account.TenantURL,
			WSHost:       account.WSHost,
			WSPath:       account.WSPath,
			ActorARN:     account.ActorARN,
			AWSAccountID: account.AWSAccountID,
			AccountName:  account.AccountName,
			UserInfo:     account.UserInfo,
			ExpiresAt:    account.ExpiresAt,
			ObtainedAt:   account.ObtainedAt,
			CreatedAt:    account.CreatedAt,
			Disabled:     account.Disabled,
		}
		switch {
		case account.ProxyID < 0:
			entry.Direct = true
		case account.ProxyID > 0:
			entry.ProxyURL = proxyURLs[account.ProxyID]
		}
		if includeSecrets {
			entry.AccessToken = account.AccessToken
			entry.RefreshToken = account.RefreshToken
			entry.IDToken = account.IDToken
			entry.ClientID = account.ClientID
			entry.ClientSecret = account.ClientSecret
			entry.IssuerURL = account.IssuerURL
			entry.IDCRegion = account.IDCRegion
			entry.AuthEndpoint = account.AuthEndpoint
			entry.TokenEndpoint = account.TokenEndpoint
			entry.RedirectURI = account.RedirectURI
		}
		file.Accounts = append(file.Accounts, entry)
	}
	return res, nil
}

// ImportResult summarises an incremental import.
type ImportResult struct {
	Imported int      `json:"imported"`
	Updated  int      `json:"updated"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// Import adds accounts that are not stored yet and, when update is true,
// refreshes the stored tokens of accounts that already exist. Existing
// accounts keep their disabled flag, so an import cannot put a disabled
// account back into rotation. Entries are matched on (email, tenant_url),
// the same key the store uses.
func Import(ctx context.Context, st *store.Store, file *TransferFile, update bool) (ImportResult, error) {
	var res ImportResult
	if file == nil || len(file.Accounts) == 0 {
		return res, fmt.Errorf("import file contains no accounts")
	}
	if file.Version > TransferVersion {
		return res, fmt.Errorf("unsupported export version %d (this build understands %d)", file.Version, TransferVersion)
	}
	existingList, err := st.List(ctx)
	if err != nil {
		return res, err
	}
	proxyIDs := make(map[string]int64)
	if proxies, err := st.ListProxies(ctx); err == nil {
		for _, proxy := range proxies {
			proxyIDs[proxy.URL] = proxy.ID
		}
	}
	type key struct{ email, tenant string }
	existing := make(map[key]*store.Account, len(existingList))
	for i := range existingList {
		account := &existingList[i]
		existing[key{strings.ToLower(account.Email), account.TenantURL}] = account
	}
	seen := make(map[key]bool, len(file.Accounts))

	for i := range file.Accounts {
		entry := file.Accounts[i]
		email := strings.TrimSpace(entry.Email)
		tenant := strings.TrimSpace(entry.TenantURL)
		if email == "" {
			res.Errors = append(res.Errors, fmt.Sprintf("account %d: missing email", i+1))
			continue
		}
		if tenant == "" && strings.TrimSpace(entry.WSHost) == "" {
			res.Errors = append(res.Errors, fmt.Sprintf("account %d (%s): missing tenant_url", i+1, email))
			continue
		}
		if entry.RefreshToken == "" && entry.AccessToken == "" {
			res.Errors = append(res.Errors, fmt.Sprintf("account %d (%s): no access_token or refresh_token", i+1, email))
			continue
		}
		k := key{strings.ToLower(email), tenant}
		if seen[k] {
			res.Skipped++
			continue
		}
		seen[k] = true

		account := &store.Account{
			Email:         email,
			DisplayName:   entry.DisplayName,
			Provider:      entry.Provider,
			BuildStage:    entry.BuildStage,
			Region:        entry.Region,
			TenantURL:     tenant,
			WSHost:        entry.WSHost,
			WSPath:        entry.WSPath,
			AccessToken:   entry.AccessToken,
			RefreshToken:  entry.RefreshToken,
			IDToken:       entry.IDToken,
			ActorARN:      entry.ActorARN,
			AWSAccountID:  entry.AWSAccountID,
			AccountName:   entry.AccountName,
			ClientID:      entry.ClientID,
			ClientSecret:  entry.ClientSecret,
			IssuerURL:     entry.IssuerURL,
			IDCRegion:     entry.IDCRegion,
			AuthEndpoint:  entry.AuthEndpoint,
			TokenEndpoint: entry.TokenEndpoint,
			RedirectURI:   entry.RedirectURI,
			UserInfo:      entry.UserInfo,
			ExpiresAt:     entry.ExpiresAt,
			ObtainedAt:    entry.ObtainedAt,
			CreatedAt:     entry.CreatedAt,
			Disabled:      entry.Disabled,
		}
		switch {
		case entry.Direct:
			account.ProxyID = -1
		case strings.TrimSpace(entry.ProxyURL) != "":
			// The target installation may have the same proxy under a new id;
			// when it is absent the account follows the local pool instead.
			account.ProxyID = proxyIDs[strings.TrimSpace(entry.ProxyURL)]
		}
		if account.Provider == "" {
			account.Provider = "social"
		}
		if current, ok := existing[k]; ok {
			if !update {
				res.Skipped++
				continue
			}
			// Keep local state that is not part of the token set.
			account.Disabled = current.Disabled
			account.CreatedAt = current.CreatedAt
			account.UseCount = current.UseCount
			account.LastUsedAt = current.LastUsedAt
			if !entry.Direct && strings.TrimSpace(entry.ProxyURL) == "" {
				account.ProxyID = current.ProxyID
			}
			if err := st.Save(ctx, account); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("account %d (%s): %v", i+1, email, err))
				continue
			}
			res.Updated++
			continue
		}
		if err := st.Save(ctx, account); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("account %d (%s): %v", i+1, email, err))
			continue
		}
		existing[k] = account
		res.Imported++
	}
	return res, nil
}
