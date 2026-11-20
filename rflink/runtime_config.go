/* SPDX-License-Identifier: AGPL-3.0-or-later
 * Copyright (c) 2022-2026 MERKATOR <merkator@merkator.pro>
 * @AGPL-3.0-or-later <https://spdx.org/licenses/AGPL-3.0-or-later.html>
 */

package rflink

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
)

var (
	errListConflict = errors.New("name already exists")
	errListNotFound = errors.New("not_found")
	errListLimit    = errors.New("max webhooks reached")
	errTokenLimit   = errors.New("max api tokens reached")
)

const (
	defaultRuntimeConfigPath = "data/runtime.json"
	runtimeBackupCount       = 3
	runtimeFileVersion       = 1
	maxAPITokens             = 32
	maxAPITokenNameLen       = 64
)

// RuntimeFile is the on-disk overlay. A non-null field fully overrides env for that key.
type RuntimeFile struct {
	Version       int     `json:"version"`
	UpdatedAt     string  `json:"updated_at,omitempty"`
	FriendlyNames *string `json:"friendly_names,omitempty"` // CSV HWID:Name,...
	IgnoreList    *string `json:"ignore_list,omitempty"`    // CSV
	HWIDMap       *string `json:"hwid_map,omitempty"`       // CSV
	// No omitempty: a missing/null key means "env owns this", an empty array means
	// "runtime owns this and the list is empty". omitempty cannot tell those apart.
	APITokens []RuntimeAPIToken `json:"api_tokens"`

	HADiscovery       *bool            `json:"ha_discovery,omitempty"`
	HADiscoveryPrefix *string          `json:"ha_discovery_prefix,omitempty"`
	HAButtons         *string          `json:"ha_buttons,omitempty"`
	HASwitches        *string          `json:"ha_switches,omitempty"`
	Webhooks          []RuntimeWebhook `json:"webhooks"`
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
	mu         sync.RWMutex
	path       string
	file       RuntimeFile
	opts       *Options
	sources    RuntimeSource
	stopWatch  chan struct{}
	watchDone  chan struct{}
	stopOnce   sync.Once
	persistGen uint64
	onChange   func()

	// Env values captured before the first overlay apply. Later applies must be
	// able to restore them when a key disappears from the file.
	envHADiscovery bool
	envHAPrefix    string
	envHAButtons   string
	envHASwitches  string

	haSig      string
	haSigReady bool
	haChanged  bool
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
		path:           path,
		opts:           opts,
		sources:        defaultRuntimeSources(),
		stopWatch:      make(chan struct{}),
		watchDone:      make(chan struct{}),
		envHADiscovery: opts.Publish.HomeAssistantDiscovery,
		envHAPrefix:    opts.Publish.HADiscoveryPrefix,
		envHAButtons:   opts.Publish.HAButtons,
		envHASwitches:  opts.Publish.HASwitches,
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
	rm.mu.Lock()
	gen := rm.persistGen
	rm.mu.Unlock()

	data, err := os.ReadFile(rm.path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("runtime config: file not found, using env only",
				"path", rm.path,
			)
			rm.mu.Lock()
			defer rm.mu.Unlock()
			if rm.persistGen != gen {
				return nil
			}
			rm.file = RuntimeFile{Version: runtimeFileVersion}
			rm.sources = defaultRuntimeSources()
			return nil
		}
		return fmt.Errorf("read runtime config: %w", err)
	}

	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("runtime config %q is empty", rm.path)
	}
	var f RuntimeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return fmt.Errorf("parse runtime config %q: %w", rm.path, err)
	}
	if f.Version == 0 {
		f.Version = runtimeFileVersion
	}

	rm.mu.Lock()
	defer rm.mu.Unlock()
	if rm.persistGen != gen {
		log.Debug("runtime config reload skipped; a newer save is already in memory", "path", rm.path)
		return nil
	}
	rm.file = f
	rm.applyLocked()
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
	} else {
		o.Publish.HomeAssistantDiscovery = rm.envHADiscovery
	}
	if f.HADiscoveryPrefix != nil {
		pfx := cleanMQTTTopic(*f.HADiscoveryPrefix)
		if pfx == "" {
			pfx = "homeassistant"
		}
		o.Publish.HADiscoveryPrefix = pfx
		src.HADiscoveryPrefix = "runtime"
		log.Info("config override: ha_discovery_prefix from runtime file")
	} else {
		pfx := cleanMQTTTopic(rm.envHAPrefix)
		if pfx == "" {
			pfx = "homeassistant"
		}
		o.Publish.HADiscoveryPrefix = pfx
	}
	if f.HAButtons != nil {
		o.Publish.HAButtons = *f.HAButtons
		o.haButtons = parseHAButtons(*f.HAButtons)
		src.HAButtons = "runtime"
		log.Info("config override: ha_buttons from runtime file")
	} else {
		o.Publish.HAButtons = rm.envHAButtons
		o.haButtons = parseHAButtons(rm.envHAButtons)
	}
	if f.HASwitches != nil {
		o.Publish.HASwitches = *f.HASwitches
		o.haSwitches = parseHASwitches(*f.HASwitches)
		src.HASwitches = "runtime"
		log.Info("config override: ha_switches from runtime file")
	} else {
		o.Publish.HASwitches = rm.envHASwitches
		o.haSwitches = parseHASwitches(rm.envHASwitches)
	}

	// Safety: discovery on ⇒ prefix never empty
	if o.Publish.HomeAssistantDiscovery && strings.TrimSpace(o.Publish.HADiscoveryPrefix) == "" {
		o.Publish.HADiscoveryPrefix = "homeassistant"
		log.Warn("ha_discovery enabled with empty prefix — using default homeassistant")
	}

	prevHooks := append([]RuntimeWebhook(nil), o.webhooks...)
	if f.Webhooks != nil {
		o.webhooks = stabilizeWebhookIDs(sanitizeRuntimeWebhooks(f.Webhooks), prevHooks)
		src.Webhooks = "runtime"
		log.Info("config override: webhooks from runtime file", "count", len(o.webhooks))
	} else {
		o.webhooks = stabilizeWebhookIDs(parseWebhooksJSON(o.WebhooksJSON), prevHooks)
	}

	rm.sources = src
	sig := fmt.Sprintf("%t\n%s\n%s\n%s",
		o.Publish.HomeAssistantDiscovery,
		o.Publish.HADiscoveryPrefix,
		o.Publish.HAButtons,
		o.Publish.HASwitches,
	)
	if rm.haSigReady && sig != rm.haSig {
		rm.haChanged = true
	}
	rm.haSig = sig
	rm.haSigReady = true
}

// consumeHAChanged reports whether HA discovery settings changed since the last call.
func (rm *runtimeManager) consumeHAChanged() bool {
	if rm == nil {
		return false
	}
	rm.mu.Lock()
	defer rm.mu.Unlock()
	changed := rm.haChanged
	rm.haChanged = false
	return changed
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
	return rm.commitLocked(f, firstTimeKeys)
}

// commitLocked persists f and applies it. Caller holds rm.mu.
// The change callback runs without the lock, then the lock is taken again.
func (rm *runtimeManager) commitLocked(f RuntimeFile, first []string) ([]string, error) {
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
	return first, nil
}

// UpdateAPITokens replaces the token list under the runtime lock.
// A nil file list is filled from env first, so two overlapping edits cannot drop each other.
func (rm *runtimeManager) UpdateAPITokens(mut func(cur []RuntimeAPIToken) ([]RuntimeAPIToken, error)) (first []string, err error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	var cur []RuntimeAPIToken
	if rm.file.APITokens != nil {
		cur = append([]RuntimeAPIToken{}, rm.file.APITokens...)
	} else {
		cur = rm.envAPITokens()
	}
	next, err := mut(cur)
	if err != nil {
		return nil, err
	}
	if next == nil {
		next = []RuntimeAPIToken{}
	}
	f := rm.file
	if f.Version == 0 {
		f.Version = runtimeFileVersion
	}
	if f.APITokens == nil {
		first = []string{"api_tokens"}
	}
	f.APITokens = next
	f.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	f.Version = runtimeFileVersion
	return rm.commitLocked(f, first)
}

// UpdateWebhooks replaces the webhook list under the runtime lock.
func (rm *runtimeManager) UpdateWebhooks(mut func(cur []RuntimeWebhook) ([]RuntimeWebhook, error)) (first []string, err error) {
	rm.mu.Lock()
	defer rm.mu.Unlock()

	var cur []RuntimeWebhook
	if rm.file.Webhooks != nil {
		cur = append([]RuntimeWebhook{}, rm.file.Webhooks...)
	} else {
		rm.opts.runtimeMu.RLock()
		cur = append([]RuntimeWebhook{}, rm.opts.webhooks...)
		rm.opts.runtimeMu.RUnlock()
	}
	next, err := mut(cur)
	if err != nil {
		return nil, err
	}
	if next == nil {
		next = []RuntimeWebhook{}
	}
	if len(next) > maxWebhooks {
		return nil, errListLimit
	}
	f := rm.file
	if f.Version == 0 {
		f.Version = runtimeFileVersion
	}
	if f.Webhooks == nil {
		first = []string{"webhooks"}
	}
	f.Webhooks = next
	f.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	f.Version = runtimeFileVersion
	return rm.commitLocked(f, first)
}

func (rm *runtimeManager) envAPITokens() []RuntimeAPIToken {
	rm.opts.runtimeMu.RLock()
	defer rm.opts.runtimeMu.RUnlock()
	out := make([]RuntimeAPIToken, 0, len(rm.opts.apiTokens))
	for _, t := range rm.opts.apiTokens {
		var es *string
		if t.ExpiresAt != nil {
			s := t.ExpiresAt.Format(time.RFC3339)
			es = &s
		}
		out = append(out, RuntimeAPIToken{
			Name: t.Name, TokenHash: t.Hash, TokenSuffix: t.Suffix,
			ExpiresAt: es, CreatedAt: t.CreatedAt.Format(time.RFC3339),
			Scopes: append([]string{}, t.Scopes...),
		})
	}
	return out
}

func (rm *runtimeManager) persistLocked(f RuntimeFile) error {
	if err := rm.ensureDataDir(); err != nil {
		return err
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
	snap := rm.path + ".bak.incoming"
	hadCurrent := false
	if _, statErr := os.Stat(rm.path); statErr == nil {
		if err := copyFile(rm.path, snap); err != nil {
			return fmt.Errorf("snapshot runtime config: %w", err)
		}
		hadCurrent = true
	}
	if err := os.Rename(tmpName, rm.path); err != nil {
		_ = os.Remove(snap)
		return fmt.Errorf("replace runtime config: %w", err)
	}
	rm.persistGen++
	_ = os.Chmod(rm.path, 0o600)
	if hadCurrent {
		if err := rm.shiftBackupsLocked(snap); err != nil {
			log.Warn("runtime config backup rotate failed", "err", err)
		}
	}
	log.Info("runtime config saved", "path", rm.path)
	return nil
}

// shiftBackupsLocked moves an already-taken snapshot of the previous file into bak.1.
// Older slots are shifted only after the new runtime.json is in place.
func (rm *runtimeManager) shiftBackupsLocked(snap string) error {
	for i := runtimeBackupCount; i >= 2; i-- {
		src := rm.backupPath(i - 1)
		dst := rm.backupPath(i)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if err := moveFileReplace(src, dst); err != nil {
			return err
		}
	}
	return moveFileReplace(snap, rm.backupPath(1))
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
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("backup slot %d is empty", slot)
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
	cb := rm.onChange
	log.Info("runtime config restored from backup", "slot", slot, "path", src)
	rm.mu.Unlock()
	if cb != nil {
		cb()
	}
	rm.mu.Lock()
	return nil
}

func (rm *runtimeManager) watchLoop() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	defer close(rm.watchDone)
	var lastMod time.Time
	var lastSize int64
	var seenMod time.Time
	var seenSize int64
	var seen bool
	if st, err := os.Stat(rm.path); err == nil {
		lastMod = st.ModTime()
		lastSize = st.Size()
	}
	for {
		select {
		case <-rm.stopWatch:
			return
		case <-ticker.C:
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						log.Error("runtime watch panic recovered", "panic", rec)
					}
				}()
				st, err := os.Stat(rm.path)
				if err != nil {
					return
				}
				if st.ModTime().Equal(lastMod) && st.Size() == lastSize {
					seen = false
					return
				}
				// Wait until size and mtime stay still for one tick so a truncated
				// write is not applied as an overlay.
				if !seen || !st.ModTime().Equal(seenMod) || st.Size() != seenSize {
					seenMod = st.ModTime()
					seenSize = st.Size()
					seen = true
					return
				}
				seen = false
				if err := rm.loadAndApply(); err != nil {
					log.Warn("runtime config hot-reload failed", "err", err)
					lastMod = st.ModTime()
					lastSize = st.Size()
					return
				}
				lastMod = st.ModTime()
				lastSize = st.Size()
				log.Info("runtime config hot-reloaded", "path", rm.path)
				if rm.onChange != nil {
					rm.onChange()
				}
			}()
		}
	}
}

func (rm *runtimeManager) Stop() {
	rm.stopOnce.Do(func() { close(rm.stopWatch) })
	<-rm.watchDone
}

func copyFile(src, dst string) error {
	in, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, in, 0o600)
}

// moveFileReplace moves src onto dst without deleting dst until the new bytes
// are in a temp file beside it. A failed Stat or copy leaves the old slot intact.
func moveFileReplace(src, dst string) error {
	if _, err := os.Stat(dst); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(src, dst); err == nil {
			_ = os.Chmod(dst, 0o600)
			return nil
		}
	}
	tmp := dst + ".tmp"
	_ = os.Remove(tmp)
	if err := copyFile(src, tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	backup := dst + ".old"
	_ = os.Remove(backup)
	dstExists := true
	if _, err := os.Stat(dst); err != nil {
		if !os.IsNotExist(err) {
			_ = os.Remove(tmp)
			return err
		}
		dstExists = false
	}
	if dstExists {
		if err := os.Rename(dst, backup); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		if dstExists {
			_ = os.Rename(backup, dst)
		}
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(backup)
	_ = os.Remove(src)
	_ = os.Chmod(dst, 0o600)
	return nil
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
		exp, err := parseTokenExpiry(bits, 2)
		if err != nil {
			log.Warn("API_AUTH_TOKEN: skip entry with invalid expire", "name", name, "err", err)
			continue
		}
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

func parseTokenExpiry(bits []string, idx int) (*time.Time, error) {
	if len(bits) <= idx {
		return nil, nil // never
	}
	s := strings.TrimSpace(bits[idx])
	if s == "" || strings.EqualFold(s, "never") {
		return nil, nil
	}
	// duration: 30m, 24h. Go's ParseDuration does not accept a day suffix.
	if d, err := time.ParseDuration(s); err == nil {
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	if d, ok := parseDayDuration(s); ok {
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	// hours as plain number
	if h, err := time.ParseDuration(s + "h"); err == nil && !strings.ContainsAny(s, "smh") {
		t := time.Now().UTC().Add(h)
		return &t, nil
	}
	// RFC3339 date
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t, nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return &t, nil
	}
	return nil, fmt.Errorf("invalid expire %q", s)
}

// parseDayDuration accepts a positive day count such as "30d" (max 10 years).
func parseDayDuration(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}
	last := s[len(s)-1]
	if last != 'd' && last != 'D' {
		return 0, false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 || n > 3650 {
		return 0, false
	}
	return time.Duration(n) * 24 * time.Hour, true
}

func apiTokensFromRuntime(list []RuntimeAPIToken) []apiToken {
	out := make([]apiToken, 0, len(list))
	for _, e := range list {
		if e.TokenHash == "" || e.Name == "" {
			continue
		}
		var exp *time.Time
		if e.ExpiresAt != nil && *e.ExpiresAt != "" {
			t, err := time.Parse(time.RFC3339, *e.ExpiresAt)
			if err != nil {
				log.Warn("runtime api token skipped: bad expires_at", "name", e.Name, "err", err)
				continue
			}
			exp = &t
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
	got := strings.TrimSpace(raw)
	if want == "" || got == "" {
		return false
	}
	sum := sha256.Sum256([]byte(got))
	exp := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(sum[:], exp[:]) == 1
}
