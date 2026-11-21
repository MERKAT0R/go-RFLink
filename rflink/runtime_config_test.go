/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-json"
)

func TestParseTokenExpiryDays(t *testing.T) {
	exp, err := parseTokenExpiry([]string{"", "", "30d"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if exp == nil {
		t.Fatal("30d became never")
	}
	d := time.Until(*exp)
	if d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("30d delta %s", d)
	}

	never, err := parseTokenExpiry([]string{"", "", "never"}, 2)
	if err != nil || never != nil {
		t.Fatalf("never: %v %v", never, err)
	}
	if _, err := parseTokenExpiry([]string{"", "", "nope"}, 2); err == nil {
		t.Fatal("invalid expire accepted")
	}
}

func TestAPITokenKeepsRFC3339AndRelativeExpiry(t *testing.T) {
	got := parseAPIAuthTokens("ha:s3cretvalue:2026-12-01T15:04:05Z:read", "")
	if len(got) != 1 || got[0].ExpiresAt == nil || len(got[0].Scopes) != 1 || got[0].Scopes[0] != "read" {
		t.Fatalf("%#v", got)
	}
	want := time.Date(2026, 12, 1, 15, 4, 5, 0, time.UTC)
	if !got[0].ExpiresAt.Equal(want) {
		t.Fatalf("expiry %s", got[0].ExpiresAt)
	}
	plain := parseAPIAuthTokens("ha:s3cretvalue:2026-12-01T15:04:05Z", "")
	if len(plain) != 1 || plain[0].ExpiresAt == nil || !plain[0].ExpiresAt.Equal(want) {
		t.Fatalf("no scopes: %#v", plain)
	}

	dir := t.TempDir()
	opts := &Options{RuntimeConfigFile: filepath.Join(dir, "runtime.json")}
	opts.APIAuthTokens = "ops:supersecretvalue:30m:read"
	rm, err := newRuntimeManager(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rm.Stop)
	if len(opts.apiTokens) != 1 || opts.apiTokens[0].ExpiresAt == nil {
		t.Fatalf("%#v", opts.apiTokens)
	}
	first := *opts.apiTokens[0].ExpiresAt
	time.Sleep(20 * time.Millisecond)
	rm.mu.Lock()
	rm.applyLocked()
	rm.mu.Unlock()
	if opts.apiTokens[0].ExpiresAt == nil || !opts.apiTokens[0].ExpiresAt.Equal(first) {
		t.Fatalf("expiry moved from %s to %v", first, opts.apiTokens[0].ExpiresAt)
	}
}

func TestAPITokensFromRuntimeRejectsBadExpiry(t *testing.T) {
	bad := "30d"
	if got := apiTokensFromRuntime([]RuntimeAPIToken{{
		Name: "a", TokenHash: "abc", ExpiresAt: &bad,
	}}); len(got) != 0 {
		t.Fatalf("bad expiry kept: %#v", got)
	}
	ok := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	got := apiTokensFromRuntime([]RuntimeAPIToken{{
		Name: "b", TokenHash: "abc", ExpiresAt: &ok, CreatedAt: ok,
	}})
	if len(got) != 1 || got[0].ExpiresAt == nil {
		t.Fatalf("good expiry dropped: %#v", got)
	}
}

func TestRuntimeFileEmptyListsStayOwned(t *testing.T) {
	owned := RuntimeFile{
		Version:   1,
		APITokens: []RuntimeAPIToken{},
		Webhooks:  []RuntimeWebhook{},
	}
	raw, err := json.Marshal(owned)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"api_tokens":[]`) || !strings.Contains(string(raw), `"webhooks":[]`) {
		t.Fatalf("empty lists omitted: %s", raw)
	}
	var back RuntimeFile
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.APITokens == nil || back.Webhooks == nil {
		t.Fatalf("empty lists came back nil: tokens %#v hooks %#v", back.APITokens, back.Webhooks)
	}

	absent := RuntimeFile{Version: 1}
	raw, err = json.Marshal(absent)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"api_tokens":null`) {
		t.Fatalf("nil list was not null: %s", raw)
	}
	var envOwned RuntimeFile
	if err := json.Unmarshal(raw, &envOwned); err != nil {
		t.Fatal(err)
	}
	if envOwned.APITokens != nil || envOwned.Webhooks != nil {
		t.Fatal("null lists became non-nil")
	}
}
