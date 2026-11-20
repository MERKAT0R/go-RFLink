/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
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
