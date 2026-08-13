/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

const (
	defaultRuntimeConfigPath = "data/runtime.json"
	runtimeBackupCount       = 3
	runtimeFileVersion       = 1
)

// RuntimeFile is the on-disk overlay. A non-null field fully overrides env for that key.
type RuntimeFile struct {
	Version       int               `json:"version"`
	UpdatedAt     string            `json:"updated_at,omitempty"`
	FriendlyNames *string           `json:"friendly_names,omitempty"` // CSV HWID:Name,...
	IgnoreList    *string           `json:"ignore_list,omitempty"`    // CSV
	HWIDMap       *string           `json:"hwid_map,omitempty"`       // CSV
	APITokens     []RuntimeAPIToken `json:"api_tokens,omitempty"`

	HADiscovery       *bool            `json:"ha_discovery,omitempty"`
	HADiscoveryPrefix *string          `json:"ha_discovery_prefix,omitempty"`
	HAButtons         *string          `json:"ha_buttons,omitempty"`
	HASwitches        *string          `json:"ha_switches,omitempty"`
	Webhooks          []RuntimeWebhook `json:"webhooks,omitempty"`
}

// RuntimeAPIToken stores only a hash of the secret (never plaintext on disk).
type RuntimeAPIToken struct {
	Name        string   `json:"name"`
	TokenHash   string   `json:"token_hash"`
	TokenSuffix string   `json:"token_suffix,omitempty"` // last 4 chars for UI
	ExpiresAt   *string  `json:"expires_at,omitempty"`   // RFC3339 or null/omit = never
	CreatedAt   string   `json:"created_at"`
	Scopes      []string `json:"scopes,omitempty"` // read, command, admin
}

// RuntimeSource describes which layer owns each editable key.
type RuntimeSource struct {
	FriendlyNames     string `json:"friendly_names"` // "env" | "runtime"
	IgnoreList        string `json:"ignore_list"`
	HWIDMap           string `json:"hwid_map"`
	APITokens         string `json:"api_tokens"`
	HADiscovery       string `json:"ha_discovery"`
	HADiscoveryPrefix string `json:"ha_discovery_prefix"`
	HAButtons         string `json:"ha_buttons"`
	HASwitches        string `json:"ha_switches"`
	Webhooks          string `json:"webhooks"`
}

type runtimeManager struct {
	mu        sync.RWMutex
	path      string
	file      RuntimeFile
	opts      *Options
	sources   RuntimeSource
	stopWatch chan struct{}
	onChange  func()
}

func defaultRuntimeSources() RuntimeSource {
	return RuntimeSource{
		FriendlyNames: "env", IgnoreList: "env", HWIDMap: "env", APITokens: "env",
		HADiscovery: "env", HADiscoveryPrefix: "env", HAButtons: "env", HASwitches: "env",
		Webhooks: "env",
	}
}

func newRuntimeManager(opts *Options) (*runtimeManager, error) {
	path := strings.TrimSpace(opts.RuntimeConfigFile)
	if path == "" {
		path = defaultRuntimeConfigPath
	}
	path = filepath.Clean(path)

	rm := &runtimeManager{
		path:      path,
		opts:      opts,
		sources:   defaultRuntimeSources(),
		stopWatch: make(chan struct{}),
	}

	if err := rm.ensureDataDir(); err != nil {
		return nil, err
	}

	if err := rm.loadAndApply(); err != nil {
		return nil, err
	}

	go rm.watchLoop()
	return rm, nil
}

func (rm *runtimeManager) ensureDataDir() error {
	dir := filepath.Dir(rm.path)
	if dir == "" || dir == "." {
		return nil
	}
	// 0700 — only the service account should read runtime secrets (API token hashes).
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime config dir %q: %w", dir, err)
	}
	// Best-effort tighten permissions if dir already existed.
	_ = os.Chmod(dir, 0o700)
	return nil
}

func (rm *runtimeManager) loadAndApply() error {
	data, err := os.ReadFile(rm.path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("runtime config: file not found, using env only",
				"path", rm.path,
			)
			rm.mu.Lock()
			rm.file = RuntimeFile{Version: runtimeFileVersion}
			rm.sources = defaultRuntimeSources()
			rm.mu.Unlock()
			return nil
		}
		return fmt.Errorf("read runtime config: %w", err)
	}

	var f RuntimeFile
	if len(strings.TrimSpace(string(data))) == 0 {
		f = RuntimeFile{Version: runtimeFileVersion}
	} else if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse runtime config %q: %w", rm.path, err)
	}
	if f.Version == 0 {
		f.Version = runtimeFileVersion
	}

	rm.mu.Lock()
	rm.file = f
	rm.applyLocked()
	rm.mu.Unlock()
	return nil
}

// applyLocked merges runtime over env into opts. Caller holds rm.mu.
func (rm *runtimeManager) applyLocked() {
	o := rm.opts
	src := defaultRuntimeSources()
	f := rm.file

	o.runtimeMu.Lock()
	defer o.runtimeMu.Unlock()

	// Start from env-parsed maps every time so we can re-apply cleanly.
	envFriendly := parseColonMap(o.Publish.FriendlyNames)
	envIgnore := parseIgnoreList(o.Publish.IgnoreList)
	envHWID := parseColonMap(o.Publish.HWIDMap)
	envAPI := parseAPIAuthTokens(o.APIAuthTokens, o.APITokenPepper)

	if f.FriendlyNames != nil {
		o.friendlyNameMap = parseColonMap(*f.FriendlyNames)
		src.FriendlyNames = "runtime"
		log.Info("config override: friendly_names from runtime file (env ignored for this key)")
	} else {
		o.friendlyNameMap = envFriendly
	}

	if f.IgnoreList != nil {
		o.ignoreSet = parseIgnoreList(*f.IgnoreList)
		src.IgnoreList = "runtime"
		log.Info("config override: ignore_list from runtime file (env ignored for this key)")
	} else {
		o.ignoreSet = envIgnore
	}

	if f.HWIDMap != nil {
		o.hwidMap = parseColonMap(*f.HWIDMap)
		src.HWIDMap = "runtime"
		log.Info("config override: hwid_map from runtime file (env ignored for this key)")
	} else {
		o.hwidMap = envHWID
	}

	if f.APITokens != nil {
		// Explicit empty slice still means "runtime owns this key".
		o.apiTokens = apiTokensFromRuntime(f.APITokens)
		src.APITokens = "runtime"
		log.Info("config override: api_tokens from runtime file (env ignored for this key)",
			"count", len(o.apiTokens),
		)
	} else {
		o.apiTokens = envAPI
	}

	if f.HADiscovery != nil {
		o.Publish.HomeAssistantDiscovery = *f.HADiscovery
		src.HADiscovery = "runtime"
		log.Info("config override: ha_discovery from runtime file")
	}
	if f.HADiscoveryPrefix != nil {
		pfx := strings.TrimSpace(*f.HADiscoveryPrefix)
		if pfx == "" {
			pfx = "homeassistant"
		}
		o.Publish.HADiscoveryPrefix = pfx
		src.HADiscoveryPrefix = "runtime"
		log.Info("config override: ha_discovery_prefix from runtime file")
	}
	if f.HAButtons != nil {
		o.Publish.HAButtons = *f.HAButtons
		o.haButtons = parseHAButtons(*f.HAButtons)
		src.HAButtons = "runtime"
		log.Info("config override: ha_buttons from runtime file")
	}
	if f.HASwitches != nil {
		o.Publish.HASwitches = *f.HASwitches
		o.haSwitches = parseHASwitches(*f.HASwitches)
		src.HASwitches = "runtime"
		log.Info("config override: ha_switches from runtime file")
	}

	// Safety: discovery on ⇒ prefix never empty
	if o.Publish.HomeAssistantDiscovery && strings.TrimSpace(o.Publish.HADiscoveryPrefix) == "" {
		o.Publish.HADiscoveryPrefix = "homeassistant"
		log.Warn("ha_discovery enabled with empty prefix — using default homeassistant")
	}

	envHooks := parseWebhooksJSON(o.WebhooksJSON)
	if f.Webhooks != nil {
		o.webhooks = append([]RuntimeWebhook{}, f.Webhooks...)
		src.Webhooks = "runtime"
		log.Info("config override: webhooks from runtime file", "count", len(o.webhooks))
	} else {
		o.webhooks = envHooks
	}

	rm.sources = src
}

func (rm *runtimeManager) snapshot() (RuntimeFile, RuntimeSource) {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.file, rm.sources
}

func (rm *runtimeManager) pathString() string {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	return rm.path
}

// SaveOverlay updates present keys and persists. firstTimeKeys lists keys that
// switched from env → runtime on this save (caller can surface in API/GUI).
func (rm *runtimeManager) SaveOverlay(patch RuntimeFile) (firstTimeKeys []string, err error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	f := rm.file
	if f.Version == 0 {
		f.Version = runtimeFileVersion
	}

	mark := func(key string, already bool) {
		if !already {
			firstTimeKeys = append(firstTimeKeys, key)
		}
	}

	if patch.FriendlyNames != nil {
		mark("friendly_names", f.FriendlyNames != nil)
		v := *patch.FriendlyNames
		f.FriendlyNames = &v
	}
	if patch.IgnoreList != nil {
		mark("ignore_list", f.IgnoreList != nil)
		v := *patch.IgnoreList
		f.IgnoreList = &v
	}
	if patch.HWIDMap != nil {
		mark("hwid_map", f.HWIDMap != nil)
		v := *patch.HWIDMap
		f.HWIDMap = &v
	}
	if patch.APITokens != nil {
		mark("api_tokens", f.APITokens != nil)
		f.APITokens = patch.APITokens
	}
	if patch.HADiscovery != nil {
		mark("ha_discovery", f.HADiscovery != nil)
		v := *patch.HADiscovery
		f.HADiscovery = &v
	}
	if patch.HADiscoveryPrefix != nil {
		mark("ha_discovery_prefix", f.HADiscoveryPrefix != nil)
		v := strings.TrimSpace(*patch.HADiscoveryPrefix)
		if v == "" {
			v = "homeassistant"
		}
		f.HADiscoveryPrefix = &v
	}
	if patch.HAButtons != nil {
		mark("ha_buttons", f.HAButtons != nil)
		v := *patch.HAButtons
		f.HAButtons = &v
	}
	if patch.HASwitches != nil {
		mark("ha_switches", f.HASwitches != nil)
		v := *patch.HASwitches
		f.HASwitches = &v
	}
	if patch.Webhooks != nil {
		mark("webhooks", f.Webhooks != nil)
		f.Webhooks = patch.Webhooks
	}

	f.UpdatedAt = now
	f.Version = runtimeFileVersion

	if err := rm.persistLocked(f); err != nil {
		return nil, err
	}
	rm.file = f
	rm.applyLocked()
	cb := rm.onChange
	rm.mu.Unlock()
	if cb != nil {
		cb()
	}
	rm.mu.Lock()
	return firstTimeKeys, nil
}

func (rm *runtimeManager) persistLocked(f RuntimeFile) error {
	if err := rm.ensureDataDir(); err != nil {
		return err
	}
	if err := rm.rotateBackupsLocked(); err != nil {
		log.Warn("runtime config backup rotate failed", "err", err)
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(rm.path)
	tmp, err := os.CreateTemp(dir, "runtime-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp runtime config: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		log.Debug("chmod runtime temp", "err", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, rm.path); err != nil {
		return fmt.Errorf("replace runtime config: %w", err)
	}
	_ = os.Chmod(rm.path, 0o600)
	log.Info("runtime config saved", "path", rm.path)
	return nil
}

func (rm *runtimeManager) rotateBackupsLocked() error {
	// runtime.json.bak.3 ← .bak.2 ← .bak.1 ← current
	for i := runtimeBackupCount; i >= 1; i-- {
		src := rm.backupPath(i - 1)
		dst := rm.backupPath(i)
		if i == 1 {
			src = rm.path
		}
		if _, err := os.Stat(src); err != nil {
			continue
		}
		_ = os.Remove(dst)
		if err := copyFile(src, dst); err != nil {
			return err
		}
		_ = os.Chmod(dst, 0o600)
	}
	return nil
}

func (rm *runtimeManager) backupPath(n int) string {
	if n <= 0 {
		return rm.path
	}
	return fmt.Sprintf("%s.bak.%d", rm.path, n)
}

// ListBackups returns existing backup slots 1..3 with mtime.
func (rm *runtimeManager) ListBackups() []map[string]any {
	rm.mu.RLock()
	defer rm.mu.RUnlock()
	var out []map[string]any
	for i := 1; i <= runtimeBackupCount; i++ {
		p := rm.backupPath(i)
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		out = append(out, map[string]any{
			"slot":     i,
			"path":     p,
			"size":     st.Size(),
			"mod_time": st.ModTime().UTC().Format(time.RFC3339),
		})
	}
	return out
}

// RestoreBackup copies bak.N over current after rotating current into bak chain.
func (rm *runtimeManager) RestoreBackup(slot int) error {
	if slot < 1 || slot > runtimeBackupCount {
		return fmt.Errorf("invalid backup slot %d", slot)
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()

	src := rm.backupPath(slot)
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read backup: %w", err)
	}
	var f RuntimeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse backup: %w", err)
	}
	if err := rm.persistLocked(f); err != nil {
		return err
	}
	rm.file = f
	rm.applyLocked()
	log.Info("runtime config restored from backup", "slot", slot, "path", src)
	return nil
}

func (rm *runtimeManager) watchLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var lastMod time.Time
	if st, err := os.Stat(rm.path); err == nil {
		lastMod = st.ModTime()
	}
	for {
		select {
		case <-rm.stopWatch:
			return
		case <-ticker.C:
			st, err := os.Stat(rm.path)
			if err != nil {
				continue
			}
			if st.ModTime().Equal(lastMod) {
				continue
			}
			lastMod = st.ModTime()
			if err := rm.loadAndApply(); err != nil {
				log.Warn("runtime config hot-reload failed", "err", err)
			} else {
				log.Info("runtime config hot-reloaded", "path", rm.path)
				if rm.onChange != nil {
					rm.onChange()
				}
			}
		}
	}
}

func (rm *runtimeManager) Stop() {
	select {
	case <-rm.stopWatch:
	default:
		close(rm.stopWatch)
	}
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o600)
}

// --- API tokens ---

type apiToken struct {
	Name      string
	Hash      string
	Suffix    string
	ExpiresAt *time.Time
	CreatedAt time.Time
	Scopes    []string // empty → defaultScopes
}

// Default API token scopes when none specified.
var defaultAPITokenScopes = []string{"read", "command"}

func (t apiToken) hasScope(scope string) bool {
	scopes := t.Scopes
	if len(scopes) == 0 {
		scopes = defaultAPITokenScopes
	}
	for _, s := range scopes {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "admin" || s == "*" || s == "all" {
			return true
		}
		if s == strings.ToLower(scope) {
			return true
		}
	}
	return false
}

func hashAPIToken(raw, pepper string) string {
	payload := raw
	if pepper != "" {
		payload = pepper + ":" + raw
	}
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func generateAPITokenSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "grflnk_" + hex.EncodeToString(b), nil
}

func parseAPIAuthTokens(raw, pepper string) []apiToken {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []apiToken
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		// name:TOKEN[:expire[:scopes]] — scopes e.g. read+command or read,command,admin
		bits := strings.SplitN(part, ":", 4)
		if len(bits) < 2 {
			log.Warn("API_AUTH_TOKEN: skip invalid entry (want name:TOKEN[:expire[:scopes]])", "entry", part)
			continue
		}
		name := strings.TrimSpace(bits[0])
		tok := strings.TrimSpace(bits[1])
		if name == "" || tok == "" {
			continue
		}
		exp := parseTokenExpiry(bits, 2)
		scopes := defaultAPITokenScopes
		if len(bits) >= 4 {
			scopes = parseScopeList(bits[3])
		}
		suffix := tok
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		out = append(out, apiToken{
			Name: name, Hash: hashAPIToken(tok, pepper), Suffix: suffix,
			ExpiresAt: exp, CreatedAt: time.Now().UTC(), Scopes: scopes,
		})
	}
	return out
}

func parseScopeList(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string{}, defaultAPITokenScopes...)
	}
	raw = strings.ReplaceAll(raw, "+", ",")
	var out []string
	seen := map[string]struct{}{}
	for p := range strings.SplitSeq(raw, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return append([]string{}, defaultAPITokenScopes...)
	}
	return out
}

func parseTokenExpiry(bits []string, idx int) *time.Time {
	if len(bits) <= idx {
		return nil // never
	}
	s := strings.TrimSpace(bits[idx])
	if s == "" || strings.EqualFold(s, "never") {
		return nil
	}
	// duration: 30m, 24h, 7d
	if d, err := time.ParseDuration(s); err == nil {
		t := time.Now().UTC().Add(d)
		return &t
	}
	// hours as plain number
	if h, err := time.ParseDuration(s + "h"); err == nil && !strings.ContainsAny(s, "smh") {
		t := time.Now().UTC().Add(h)
		return &t
	}
	// RFC3339 date
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t
	}
	log.Warn("API_AUTH_TOKEN: invalid expire, treating as never", "value", s)
	return nil
}

func apiTokensFromRuntime(list []RuntimeAPIToken) []apiToken {
	out := make([]apiToken, 0, len(list))
	for _, e := range list {
		if e.TokenHash == "" || e.Name == "" {
			continue
		}
		var exp *time.Time
		if e.ExpiresAt != nil && *e.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, *e.ExpiresAt); err == nil {
				exp = &t
			}
		}
		created := time.Now().UTC()
		if e.CreatedAt != "" {
			if t, err := time.Parse(time.RFC3339, e.CreatedAt); err == nil {
				created = t
			}
		}
		scopes := e.Scopes
		if len(scopes) == 0 {
			scopes = append([]string{}, defaultAPITokenScopes...)
		}
		out = append(out, apiToken{
			Name: e.Name, Hash: e.TokenHash, Suffix: e.TokenSuffix,
			ExpiresAt: exp, CreatedAt: created, Scopes: scopes,
		})
	}
	return out
}

func (o *Options) validAPIToken(raw string) (tok *apiToken, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	h := hashAPIToken(raw, o.APITokenPepper)
	now := time.Now().UTC()
	o.runtimeMu.RLock()
	defer o.runtimeMu.RUnlock()
	for i := range o.apiTokens {
		t := &o.apiTokens[i]
		if subtleConstantTimeEq(t.Hash, h) {
			if t.ExpiresAt != nil && now.After(*t.ExpiresAt) {
				return t, false
			}
			return t, true
		}
	}
	return nil, false
}

func (o *Options) guiTokenOK(raw string) bool {
	want := strings.TrimSpace(o.HTTP.AuthToken)
	if want == "" {
		return false
	}
	return subtleConstantTimeEq(strings.TrimSpace(raw), want)
}
