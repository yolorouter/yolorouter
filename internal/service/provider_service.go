// Package service additions for M2: business logic, the destination-
// version-aware running-status computation, and encryption calls around
// internal/repository's pure data access. See design doc
// .claude/docs/2026-07-18-m2-provider-design.md.
package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter-ce/internal/model"
	"github.com/yolorouter/yolorouter-ce/internal/repository"
	"github.com/yolorouter/yolorouter-ce/pkg/crypto"
	"github.com/yolorouter/yolorouter-ce/pkg/errcode"
)

const (
	RunningStatusNotConfigured = "not_configured"
	RunningStatusPending       = "pending_test"
	RunningStatusAvailable     = "available"
	RunningStatusPartial       = "partial"
	RunningStatusUnavailable   = "unavailable"
)

// computeRunningStatus implements design doc §4's table: the provider's
// running status is derived at read time from its enabled keys' verification
// status and destination-version alignment, never stored.
func computeRunningStatus(keys []model.ProviderKey, destinationVersion int) string {
	if len(keys) == 0 {
		return RunningStatusNotConfigured
	}
	var hasEnabled, hasGood, hasFailedOrReentry, hasUntestedCurrent bool
	for _, k := range keys {
		if k.ManagementStatus != model.ProviderKeyStatusEnabled {
			continue
		}
		hasEnabled = true
		versionCurrent := k.AuthorizedDestinationVersion == destinationVersion
		switch {
		case k.VerificationStatus == model.VerificationStatusPassed && versionCurrent:
			hasGood = true
		case k.VerificationStatus == model.VerificationStatusUntested && versionCurrent:
			hasUntestedCurrent = true
		default:
			hasFailedOrReentry = true
		}
	}
	switch {
	case !hasEnabled:
		return RunningStatusUnavailable
	case hasGood && (hasFailedOrReentry || hasUntestedCurrent):
		return RunningStatusPartial
	case hasGood:
		return RunningStatusAvailable
	case hasFailedOrReentry:
		return RunningStatusUnavailable
	default:
		return RunningStatusPending
	}
}

const providerKeyFingerprintProbe = "yolorouter-ce-provider-key-fingerprint-probe-v1"

// minKeyPlaintextLength implements design doc §3's minimum-length rule: a
// key shorter than this would let key_prefix's "keep the last 4 chars
// hidden" math expose most/all of a short secret if the database leaks.
const minKeyPlaintextLength = 20

type ProviderService struct {
	db        *gorm.DB
	masterKey []byte
	client    ProviderClient
}

func NewProviderService(db *gorm.DB, masterKey []byte, client ProviderClient) *ProviderService {
	return &ProviderService{db: db, masterKey: masterKey, client: client}
}

// VerifyMasterKeyFingerprint implements design doc §5's startup check: on a
// brand-new instance (no fingerprint row yet) it claims one; on an
// existing instance, a decrypt failure means the current master key
// doesn't match whatever key encrypted the stored probe — almost always a
// database restored without its matching config.yaml. Must be called once
// at startup, after migrations run, before the server accepts traffic.
//
// A codex adversarial review round found the original "check not-found,
// then unconditionally Save" sequence was itself a check-then-act race:
// two instances booting concurrently against the same fresh database with
// DIFFERENT master keys could both observe "not found" and both attempt to
// write, with whichever Save ran last silently overwriting the earlier
// one's probe — leaving one instance's key permanently, silently
// mismatched with no error at that moment. Fixed by always attempting an
// atomic, never-overwriting claim first (repository.ClaimProviderKeyFingerprintIfAbsent,
// gorm's clause.OnConflict{DoNothing:true}), then UNCONDITIONALLY
// re-reading and decrypt-verifying afterward — regardless of whether this
// process's own claim actually won or lost the race. The losing instance
// then correctly fails this verification instead of silently believing it
// succeeded.
func (s *ProviderService) VerifyMasterKeyFingerprint(now time.Time) error {
	encrypted, encErr := crypto.Encrypt(s.masterKey, providerKeyFingerprintProbe)
	if encErr != nil {
		return fmt.Errorf("encrypt fingerprint probe: %w", encErr)
	}
	if err := repository.ClaimProviderKeyFingerprintIfAbsent(s.db, encrypted, now); err != nil {
		return fmt.Errorf("claim fingerprint row: %w", err)
	}

	fp, err := repository.GetProviderKeyFingerprint(s.db)
	if err != nil {
		return fmt.Errorf("read fingerprint row: %w", err)
	}
	decrypted, decErr := crypto.Decrypt(s.masterKey, fp.EncryptedProbe)
	if decErr != nil || decrypted != providerKeyFingerprintProbe {
		return fmt.Errorf("provider_master_key does not match the key used to encrypt existing provider data; " +
			"if this is a database restore, restore its matching config.yaml as well")
	}
	return nil
}

type CreateProviderInput struct {
	Name         string
	BaseURL      string
	Note         string
	KeyLabel     string
	KeyPlaintext string
	// TestModel is the model name every test call for this key uses —
	// admin-supplied since M2 has no real model mapping yet (PRD §6.2.8).
	TestModel        string
	ManagementStatus int // requested status; server independently re-verifies before honoring "enabled" (design doc §6)
}

type UpdateProviderInput struct {
	Name    string
	BaseURL string
	Note    string
}

type ProviderKeyView struct {
	ID                 uint       `json:"id"`
	Label              string     `json:"label"`
	KeyPrefix          string     `json:"key_prefix"`
	SortOrder          int        `json:"sort_order"`
	TestModel          string     `json:"test_model"`
	ManagementStatus   int        `json:"management_status"`
	VerificationStatus int        `json:"verification_status"`
	NeedsReentry       bool       `json:"needs_reentry"`
	LastTestResult     *int       `json:"last_test_result"`
	LastTestModel      string     `json:"last_test_model"`
	LastTestDurationMs *int64     `json:"last_test_duration_ms"`
	LastTestedAt       *time.Time `json:"last_tested_at"`
}

type ProviderView struct {
	ID               uint              `json:"id"`
	Name             string            `json:"name"`
	ProviderType     string            `json:"provider_type"`
	BaseURL          string            `json:"base_url"`
	Note             string            `json:"note"`
	ManagementStatus int               `json:"management_status"`
	RunningStatus    string            `json:"running_status"`
	Keys             []ProviderKeyView `json:"keys"`
	CreatedAt        time.Time         `json:"created_at"`
}

func toKeyView(k model.ProviderKey, destinationVersion int) ProviderKeyView {
	return ProviderKeyView{
		ID: k.ID, Label: k.Label, KeyPrefix: k.KeyPrefix, SortOrder: k.SortOrder, TestModel: k.TestModel,
		ManagementStatus: k.ManagementStatus, VerificationStatus: k.VerificationStatus,
		NeedsReentry:       k.AuthorizedDestinationVersion != destinationVersion,
		LastTestResult:     k.LastTestResult,
		LastTestModel:      k.LastTestModel,
		LastTestDurationMs: k.LastTestDurationMs,
		LastTestedAt:       k.LastTestedAt,
	}
}

func (s *ProviderService) toProviderView(provider *model.Provider, keys []model.ProviderKey) ProviderView {
	views := make([]ProviderKeyView, 0, len(keys))
	for _, k := range keys {
		views = append(views, toKeyView(k, provider.DestinationVersion))
	}
	return ProviderView{
		ID: provider.ID, Name: provider.Name, ProviderType: provider.ProviderType,
		BaseURL: provider.BaseURL, Note: provider.Note, ManagementStatus: provider.ManagementStatus,
		RunningStatus: computeRunningStatus(keys, provider.DestinationVersion),
		Keys:          views, CreatedAt: provider.CreatedAt,
	}
}

// keyPrefixFor implements design doc §3's key_prefix formula:
// min(10, max(0, len(plaintext)-4)) characters from the start, never
// exposing the last 4+ characters of the secret.
func keyPrefixFor(plaintext string) string {
	// Rune-sliced, not byte-sliced: a code-review round found the byte
	// version could cut a multi-byte UTF-8 character in half if one
	// happened to straddle the cutoff, producing an invalid UTF-8
	// key_prefix that then round-trips through JSON as U+FFFD.
	runes := []rune(plaintext)
	n := len(runes) - 4
	if n < 0 {
		n = 0
	}
	if n > 10 {
		n = 10
	}
	return string(runes[:n])
}

func (s *ProviderService) ListProviders() ([]ProviderView, error) {
	providers, err := repository.ListProviders(s.db)
	if err != nil {
		return nil, err
	}

	ids := make([]uint, len(providers))
	for i := range providers {
		ids[i] = providers[i].ID
	}
	// One batched query for every provider's keys instead of one query per
	// provider (N+1) — grouped back by ProviderID below.
	allKeys, err := repository.ListProviderKeysByProviderIDs(s.db, ids)
	if err != nil {
		return nil, err
	}
	keysByProvider := make(map[uint][]model.ProviderKey, len(providers))
	for _, k := range allKeys {
		keysByProvider[k.ProviderID] = append(keysByProvider[k.ProviderID], k)
	}

	views := make([]ProviderView, 0, len(providers))
	for i := range providers {
		views = append(views, s.toProviderView(&providers[i], keysByProvider[providers[i].ID]))
	}
	return views, nil
}

func (s *ProviderService) GetProviderDetail(id uint) (*ProviderView, error) {
	provider, err := repository.FindProviderByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, id)
	if err != nil {
		return nil, err
	}
	view := s.toProviderView(provider, keys)
	return &view, nil
}

// CreateProvider implements design doc §6: provider + first key in one
// transaction, verification_status always starts untested regardless of
// the caller's requested management_status, then (if enabled was
// requested) a real out-of-transaction test decides the final status —
// design doc §6's "服务端重新验证".
func (s *ProviderService) CreateProvider(ctx context.Context, input CreateProviderInput, now time.Time) (*ProviderView, error) {
	if err := validatePlaintextLength(input.KeyPlaintext); err != nil {
		return nil, err
	}
	if _, err := repository.FindProviderByName(s.db, input.Name); err == nil {
		return nil, errcode.ErrProviderNameTaken
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	encryptedKey, err := crypto.Encrypt(s.masterKey, input.KeyPlaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt key: %w", err)
	}

	provider := &model.Provider{
		Name: input.Name, ProviderType: "openai", BaseURL: input.BaseURL, Note: input.Note,
		ManagementStatus: model.ProviderStatusEnabled, DestinationVersion: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	key := &model.ProviderKey{
		Label: input.KeyLabel, EncryptedKey: encryptedKey, KeyPrefix: keyPrefixFor(input.KeyPlaintext), TestModel: input.TestModel,
		SortOrder: 1, ManagementStatus: model.ProviderKeyStatusDisabled,
		VerificationStatus:           model.VerificationStatusUntested,
		AuthorizedDestinationVersion: 1, ConfigVersion: 1, TestGeneration: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := repository.CreateProviderWithKey(s.db, provider, key); err != nil {
		if isUniqueViolation(err) {
			return nil, errcode.ErrProviderNameTaken
		}
		return nil, err
	}

	// Server-side re-verify always runs for a brand-new key regardless of
	// the requested status, so a freshly created key's
	// verification_status/last_test_* reflect a real test result rather
	// than staying silently untested forever if the admin requested
	// disabled — matches design doc §6's "先测试、后开事务" applied
	// uniformly. Enabling only happens if it passes. configVersion/
	// testGeneration/snapshotVersion are all 1: CreateProviderWithKey just
	// inserted this row with those exact defaults, so there is no prior
	// row to race against yet (mirrors CreateProviderKeyPendingTest's
	// contract for a subsequently-added key).
	s.runNewPlaintextTestAndCommit(ctx, key.ID, 1, 1, 1, input.BaseURL, input.KeyPlaintext, input.TestModel,
		input.ManagementStatus == model.ProviderStatusEnabled, now)

	return s.GetProviderDetail(provider.ID)
}

// validatePlaintextLength is the one shared form of the "key plaintext too
// short" check duplicated across CreateProvider/CreateProviderKey/
// UpdateProviderKey — Gin's own binding:"min=20" already blocks every
// HTTP-originated request before it reaches here, so this only fires for
// a non-HTTP caller of these exported service methods; kept as
// defense-in-depth for that reason. A /simplify altitude-review finding:
// this used to wrap errcode.ErrProviderTestFailed purely so
// writeProviderServiceError's switch would have a matching case — but "key
// too short" is not "the connection test failed", a misleading
// classification if this ever actually fires. Uses its own sentinel now.
func validatePlaintextLength(plaintext string) error {
	if len(plaintext) < minKeyPlaintextLength {
		return fmt.Errorf("%w: key plaintext must be at least %d characters", errcode.ErrProviderKeyTooShort, minKeyPlaintextLength)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint") || strings.Contains(msg, "duplicate key value violates unique constraint")
}

// isSortOrderUniqueViolation narrows isUniqueViolation to specifically the
// UNIQUE(provider_id, sort_order) constraint (as opposed to
// UNIQUE(provider_id, label)) on provider_keys. Both SQLite and Postgres
// name unnamed multi-column UNIQUE constraint violations after their
// columns — SQLite: "UNIQUE constraint failed: provider_keys.provider_id,
// provider_keys.sort_order"; Postgres: constraint
// "provider_keys_provider_id_sort_order_key" — so a plain substring check
// on "sort_order" reliably identifies this one across both drivers.
func isSortOrderUniqueViolation(err error) bool {
	return isUniqueViolation(err) && strings.Contains(err.Error(), "sort_order")
}

// UpdateProvider handles name/note (no version bump) and base_url (atomic
// destination_version bump, design doc §3) separately, since only the
// latter must invalidate every key's authorization.
func (s *ProviderService) UpdateProvider(id uint, input UpdateProviderInput, now time.Time) (*ProviderView, error) {
	provider, err := repository.FindProviderByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}

	// A max-effort code-review round found these two writes previously ran
	// as independent, non-transactional statements: if UpdateProviderBaseURL
	// committed (bumping destination_version, which instantly invalidates
	// every key's authorization) and UpdateProviderNameNote then failed on a
	// duplicate name, the admin saw a failed request but the base_url change
	// — and its destination_version bump — had already silently landed.
	// Both writes now share one transaction so a name conflict rolls back
	// the base_url change too.
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if input.BaseURL != provider.BaseURL {
			if _, err := repository.UpdateProviderBaseURL(tx, id, input.BaseURL, now); err != nil {
				return err
			}
		}
		if err := repository.UpdateProviderNameNote(tx, id, input.Name, input.Note, now); err != nil {
			if isUniqueViolation(err) {
				return errcode.ErrProviderNameTaken
			}
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetProviderDetail(id)
}

func (s *ProviderService) SetProviderStatus(id uint, enabled bool, now time.Time) error {
	status := model.ProviderStatusDisabled
	if enabled {
		status = model.ProviderStatusEnabled
	}
	// A max-effort code-review round found this endpoint had no existence
	// check at all: toggling a nonexistent provider ID matched zero rows,
	// GORM reported no error, and the caller got a false 200 success. A
	// later /simplify efficiency-review finding collapsed the separate
	// FindProviderByID check into reading this write's own RowsAffected,
	// rather than paying for two round trips.
	applied, err := repository.UpdateProviderManagementStatus(s.db, id, status, now)
	if err != nil {
		return err
	}
	if !applied {
		return errcode.ErrProviderNotFound
	}
	return nil
}

type CreateKeyInput struct {
	Label            string
	Plaintext        string
	TestModel        string // model name every test call for this key uses (PRD §6.2.8)
	ManagementStatus int
}

type UpdateKeyInput struct {
	Label            string
	Plaintext        *string // nil = no plaintext change ("重测"/仅改标签路径)
	TestModel        string
	ManagementStatus *int // nil = not provided in this request, preserve current status
}

// CreateProviderKey appends a new key to an existing provider's pool
// (design doc §8 POST .../keys). Always goes through the "提交新明文"
// verify-then-commit flow since there is no prior plaintext to compare
// against.
func (s *ProviderService) CreateProviderKey(ctx context.Context, providerID uint, input CreateKeyInput, now time.Time) (*ProviderKeyView, error) {
	if err := validatePlaintextLength(input.Plaintext); err != nil {
		return nil, err
	}
	if _, err := repository.FindProviderKeyByLabel(s.db, providerID, input.Label); err == nil {
		return nil, errcode.ErrProviderKeyLabelTaken
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}

	encryptedKey, err := crypto.Encrypt(s.masterKey, input.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt key: %w", err)
	}
	// NextSortOrder's read and CreateProviderKeyPendingTest's insert are not
	// atomic, so two concurrent "add key" requests on the same provider can
	// compute the same next sort_order and race on UNIQUE(provider_id,
	// sort_order) — a max-effort code-review round found this was being
	// misreported as ErrProviderKeyLabelTaken (the two distinct UNIQUE
	// constraints on this table were never told apart), confusing an admin
	// whose label genuinely wasn't taken. sort_order is purely internal
	// bookkeeping the caller never chose, so on that specific collision the
	// fix is a bounded retry recomputing sort_order, not surfacing an error.
	const maxSortOrderRetries = 3
	var key *model.ProviderKey
	var snapshotVersion int
	for attempt := 0; ; attempt++ {
		nextOrder, err := repository.NextSortOrder(s.db, providerID)
		if err != nil {
			return nil, err
		}
		key = &model.ProviderKey{
			ProviderID: providerID, Label: input.Label, EncryptedKey: encryptedKey, KeyPrefix: keyPrefixFor(input.Plaintext),
			TestModel: input.TestModel,
			SortOrder: nextOrder, ManagementStatus: model.ProviderKeyStatusDisabled,
			VerificationStatus: model.VerificationStatusUntested,
			CreatedAt:          now, UpdatedAt: now,
		}
		snapshotVersion, err = repository.CreateProviderKeyPendingTest(s.db, key, now)
		if err == nil {
			break
		}
		// Checked BEFORE the generic isUniqueViolation fallback below — a
		// max-effort code-review round found the original version fell
		// through to ErrProviderKeyLabelTaken once retries were exhausted,
		// reintroducing the exact "sort_order collision misreported as a
		// label conflict" bug this retry loop exists to fix. The label was
		// already confirmed available above via FindProviderKeyByLabel, so
		// any sort_order-specific violation reaching this point is never a
		// real label conflict, retries exhausted or not.
		if isSortOrderUniqueViolation(err) {
			if attempt < maxSortOrderRetries {
				continue
			}
			return nil, fmt.Errorf("could not allocate a unique key position after %d attempts due to concurrent writes, please retry: %w", maxSortOrderRetries, err)
		}
		if isUniqueViolation(err) {
			return nil, errcode.ErrProviderKeyLabelTaken
		}
		return nil, err
	}

	s.runNewPlaintextTestAndCommit(ctx, key.ID, key.ConfigVersion, key.TestGeneration, snapshotVersion,
		provider.BaseURL, input.Plaintext, input.TestModel, input.ManagementStatus == model.ProviderKeyStatusEnabled, now)

	reloaded, err := repository.FindProviderKeyByID(s.db, key.ID)
	if err != nil {
		return nil, err
	}
	view := toKeyView(*reloaded, provider.DestinationVersion)
	return &view, nil
}

// runNewPlaintextTestAndCommit is the shared "先测试、后开事务" flow for any
// brand-new plaintext (design doc §6): run the real test OUTSIDE any
// transaction, classify per §5's three-tier rule, then commit via the
// snapshot-based CAS (design doc §3). If the CAS is lost to a race, the
// result is silently discarded — a later retest will pick up correctly,
// and this is a best-effort verification convenience, not an operation
// the caller must retry synchronously.
//
// A codex adversarial review round found the original version discarded
// TestChatCompletion's error return (`result, _ := ...`). Since TestSuccess
// is TestOutcome's zero value, an error case (e.g. the client's own
// concurrency cap rejecting the call before any network I/O happened) was
// silently classified as a passing test and could enable a key that was
// never actually verified. err != nil must skip classification/commit
// entirely — the row keeps whatever pre-test state
// CreateProviderKeyPendingTest/SwapProviderKeyPlaintext already forced
// (untested, disabled) until a later attempt actually runs.
func (s *ProviderService) runNewPlaintextTestAndCommit(ctx context.Context, keyID uint, configVersion, testGeneration, snapshotVersion int, baseURL, plaintext, testModel string, requestEnable bool, now time.Time) {
	result, err := s.client.TestChatCompletion(ctx, baseURL, plaintext, testModel)
	if err != nil {
		return
	}
	verificationStatus, overwrite, lastTestResult := classifyTestResult(result)

	applied, commitErr := repository.CommitProviderKeyPlaintextTestResult(s.db, keyID, configVersion, testGeneration, snapshotVersion,
		overwrite, verificationStatus, lastTestResult, testModel, result.DurationMs, now)
	if commitErr != nil || !applied {
		return // race or transient DB error — a later manual retest recovers; nothing more to do here.
	}
	// CAS-guarded on the row still reading Disabled — the value every
	// caller of this function forces immediately before running the test
	// (CreateProviderWithKey, CreateProviderKeyPendingTest,
	// SwapProviderKeyPlaintext all set Disabled up front). If a concurrent
	// PATCH .../status call changed management_status during the test's
	// network round trip, this stale request's own enable/disable intent
	// must lose to it rather than silently overwrite it.
	// Only fire the write when it can actually change something: every
	// caller already forces Disabled before running the test, so a
	// finalStatus of Disabled would just be a no-op UPDATE (WHERE
	// management_status = Disabled SET management_status = Disabled) —
	// wasted regardless of whether it applies, since a mismatched row
	// (concurrent status change already flipped it) leaves the same
	// no-op-with-CAS-miss outcome either way (a /simplify efficiency-review
	// finding).
	if result.Outcome == TestSuccess && requestEnable {
		_, _ = repository.CASProviderKeyManagementStatus(s.db, keyID, model.ProviderKeyStatusDisabled, model.ProviderKeyStatusEnabled, now)
	}
}

// classifyTestResult implements design doc §5's three-tier
// verification_status write rule. Returns (value-to-write-if-overwriting,
// whether-to-overwrite-at-all, last_test_result-value-to-record).
func classifyTestResult(result TestResult) (verificationStatus int, overwrite bool, lastTestResult *int) {
	outcomeInt := int(result.Outcome)
	switch result.Outcome {
	case TestSuccess:
		return model.VerificationStatusPassed, true, &outcomeInt
	case TestAuthFailed, TestQuotaUnavailable:
		return model.VerificationStatusFailed, true, &outcomeInt
	case TestPermissionDenied:
		if !result.IsModelScoped {
			return model.VerificationStatusFailed, true, &outcomeInt
		}
		return 0, false, &outcomeInt
	case TestModelNotFound, TestRateLimited:
		return 0, false, &outcomeInt
	case TestUnreachable, TestUpstreamError:
		return 0, false, &outcomeInt
	default:
		return 0, false, &outcomeInt
	}
}

// UpdateProviderKey handles both the label/status-only path (no plaintext
// change — "重测" CAS if a retest is also requested) and the new-plaintext
// path (design doc §8 PATCH .../keys/:keyId).
// verifyKeyEnableAllowed enforces PRD §6.2.7 / design doc §6's rule that a
// key can only be enabled if it has actually passed verification against
// the CURRENT destination — shared by every entry point that can request
// enabling a key WITHOUT itself running a fresh test first
// (SetProviderKeyStatus, and UpdateProviderKey's label/status-only path).
// Entry points that DO run a fresh test (runNewPlaintextTestAndCommit)
// decide enablement from that real result instead and never call this.
//
// A code-review round found both call sites had this gap independently:
// UpdateProviderKey's label-only path wrote ManagementStatus straight to
// the DB with no check at all, and SetProviderKeyStatus checked
// VerificationStatus but not AuthorizedDestinationVersion — so an admin
// could re-enable a key that needs re-entry (its provider's base_url
// changed since it last passed) via the status-only toggle, even though
// TestProviderKey explicitly rejects testing that exact same key for the
// exact same reason.
func verifyKeyEnableAllowed(key *model.ProviderKey, provider *model.Provider) error {
	if key.VerificationStatus != model.VerificationStatusPassed {
		return errcode.ErrProviderKeyNotVerified
	}
	if key.AuthorizedDestinationVersion != provider.DestinationVersion {
		return errcode.ErrProviderKeyNeedsReentry
	}
	return nil
}

// findKeyForProvider looks up a key by ID and verifies it belongs to
// providerID (the :id path segment) — a /simplify altitude-review finding:
// UpdateProviderKey, SetProviderKeyStatus, and TestProviderKey each
// repeated this exact lookup+ownership-check+not-found-translation
// sequence. A keyID that's real but belongs to a DIFFERENT provider than
// the URL claims is rejected identically to a genuinely-missing one, not a
// distinct error, so a caller can't use this to probe which provider a
// given key ID actually belongs to.
func (s *ProviderService) findKeyForProvider(providerID, keyID uint) (*model.ProviderKey, error) {
	key, err := repository.FindProviderKeyByID(s.db, keyID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderKeyNotFound
		}
		return nil, err
	}
	if key.ProviderID != providerID {
		return nil, errcode.ErrProviderKeyNotFound
	}
	return key, nil
}

func (s *ProviderService) UpdateProviderKey(ctx context.Context, providerID, keyID uint, input UpdateKeyInput, now time.Time) (*ProviderKeyView, error) {
	key, err := s.findKeyForProvider(providerID, keyID)
	if err != nil {
		return nil, err
	}
	provider, err := repository.FindProviderByID(s.db, key.ProviderID)
	if err != nil {
		return nil, err
	}

	// input.ManagementStatus is *int (nil = "not provided in this
	// request"), mirroring input.Plaintext's own nil-means-unchanged
	// convention. A max-effort code-review round found the previous plain
	// `int` field made "omitted" and "explicitly 0" indistinguishable: a
	// label/test_model-only PATCH that left management_status out of the
	// JSON body bound to Go's zero value 0, which was then written
	// straight to the DB — silently corrupting a previously-enabled key
	// (management_status=1) into status 0, neither Enabled nor Disabled.
	if input.Plaintext == nil {
		// Label/test_model/status-only edit — never touches
		// verification_status (design doc §8). Enabling still has to pass
		// the same gate SetProviderKeyStatus enforces regardless of the
		// key's prior state: this path runs no fresh test of its own, so
		// it must never let an unverified/stale key end up enabled just
		// because the request happened to also carry
		// management_status=enabled alongside a label change. The gate
		// only runs when the caller explicitly requested enabled — a
		// rename-only edit on a key that's already enabled-but-needs-
		// reentry must not be blocked by a status it never asked to touch.
		managementStatus := key.ManagementStatus
		enabling := false
		if input.ManagementStatus != nil {
			managementStatus = *input.ManagementStatus
			if managementStatus == model.ProviderKeyStatusEnabled {
				if err := verifyKeyEnableAllowed(key, provider); err != nil {
					return nil, err
				}
				enabling = true
			}
		}
		if enabling {
			// CAS-guarded on the same verification_status/
			// authorized_destination_version verifyKeyEnableAllowed just
			// checked, above — a max-effort code-review round found the
			// unconditional write below left a check-then-act window where
			// a concurrent base_url change or retest could invalidate the
			// key between that check and this write.
			applied, err := repository.UpdateProviderKeyLabelAndStatusIfVerified(s.db, keyID, input.Label, input.TestModel, managementStatus,
				model.VerificationStatusPassed, provider.DestinationVersion, now)
			if err != nil {
				if isUniqueViolation(err) {
					return nil, errcode.ErrProviderKeyLabelTaken
				}
				return nil, err
			}
			if !applied {
				return nil, errcode.ErrProviderKeyNeedsReentry
			}
		} else if err := repository.UpdateProviderKeyLabelAndStatus(s.db, keyID, input.Label, input.TestModel, managementStatus, now); err != nil {
			if isUniqueViolation(err) {
				return nil, errcode.ErrProviderKeyLabelTaken
			}
			return nil, err
		}
		reloaded, err := repository.FindProviderKeyByID(s.db, keyID)
		if err != nil {
			return nil, err
		}
		view := toKeyView(*reloaded, provider.DestinationVersion)
		return &view, nil
	}

	if err := validatePlaintextLength(*input.Plaintext); err != nil {
		return nil, err
	}
	encryptedKey, err := crypto.Encrypt(s.masterKey, *input.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt key: %w", err)
	}

	// SwapProviderKeyPlaintext atomically writes label + test_model +
	// encrypted_key + key_prefix + management_status(disabled) +
	// config_version bump + verification_status reset + test_generation
	// claim in ONE transaction (codex adversarial review finding: the
	// previous version split this across 3 separate statements —
	// UpdateProviderKeyLabelAndStatus, then a raw ciphertext/prefix
	// update, then a separate BeginProviderKeyPlaintextSwap call — leaving
	// a window where a crash or concurrent read between them could
	// observe new ciphertext still paired with the OLD credential's
	// config_version/verification_status).
	configVersion, testGeneration, snapshotVersion, err := repository.SwapProviderKeyPlaintext(s.db, keyID,
		input.Label, input.TestModel, encryptedKey, keyPrefixFor(*input.Plaintext), model.ProviderKeyStatusDisabled, now)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, errcode.ErrProviderKeyLabelTaken
		}
		return nil, err
	}
	// wantsEnabled resolves the caller's actual enable/disable intent for
	// the fresh test about to run: explicit value if given, else the
	// key's status before this edit (a /simplify simplification-review
	// finding: this used to be computed above, before the branch split,
	// even though only this plaintext branch reads it).
	wantsEnabled := key.ManagementStatus == model.ProviderKeyStatusEnabled
	if input.ManagementStatus != nil {
		wantsEnabled = *input.ManagementStatus == model.ProviderKeyStatusEnabled
	}
	s.runNewPlaintextTestAndCommit(ctx, keyID, configVersion, testGeneration, snapshotVersion,
		provider.BaseURL, *input.Plaintext, input.TestModel, wantsEnabled, now)

	reloaded, err := repository.FindProviderKeyByID(s.db, keyID)
	if err != nil {
		return nil, err
	}
	view := toKeyView(*reloaded, provider.DestinationVersion)
	return &view, nil
}

// SetProviderKeyStatus enables/disables a key with no plaintext change.
// Enabling a key whose verification_status isn't "passed", or whose
// authorized_destination_version doesn't match the provider's current one
// (needs re-entry), is rejected (PRD §6.2.7 / design doc §6) — the admin
// must run a real test first.
func (s *ProviderService) SetProviderKeyStatus(providerID, keyID uint, enabled bool, now time.Time) error {
	key, err := s.findKeyForProvider(providerID, keyID)
	if err != nil {
		return err
	}
	if !enabled {
		return repository.SetProviderKeyManagementStatus(s.db, keyID, model.ProviderKeyStatusDisabled, now)
	}
	provider, err := repository.FindProviderByID(s.db, key.ProviderID)
	if err != nil {
		return err
	}
	if err := verifyKeyEnableAllowed(key, provider); err != nil {
		return err
	}
	// CAS-guarded on the same verification_status/authorized_destination_
	// version verifyKeyEnableAllowed just checked — a max-effort
	// code-review round found the unconditional write below left a
	// check-then-act window where a concurrent base_url change or retest
	// could invalidate the key between that check and this write.
	applied, err := repository.SetProviderKeyManagementStatusIfVerified(s.db, keyID, model.ProviderKeyStatusEnabled,
		model.VerificationStatusPassed, provider.DestinationVersion, now)
	if err != nil {
		return err
	}
	if !applied {
		return errcode.ErrProviderKeyNeedsReentry
	}
	return nil
}

// ReorderProviderKey moves a key up/down one position (atomic swap,
// design doc §8). A no-op at either boundary is not an error.
func (s *ProviderService) ReorderProviderKey(providerID, keyID uint, direction string, now time.Time) error {
	_, err := repository.SwapProviderKeySortOrder(s.db, providerID, keyID, direction, now)
	// A max-effort code-review round found this was the one key-lookup
	// endpoint in this file that left gorm.ErrRecordNotFound untranslated,
	// answering 500 InternalError instead of the 400 ProviderKeyNotFound
	// every sibling endpoint (UpdateProviderKey, SetProviderKeyStatus,
	// TestProviderKey) returns for the identical unknown/cross-provider key
	// condition.
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errcode.ErrProviderKeyNotFound
	}
	return err
}

// TestKeyPreview is the stateless, unpersisted preview (design doc §6
// POST .../providers/test-key) — never writes to the database, never
// trusted by any later request.
func (s *ProviderService) TestKeyPreview(ctx context.Context, baseURL, apiKey, model string) (TestResult, error) {
	return s.client.TestChatCompletion(ctx, baseURL, apiKey, model)
}

// TestProviderKey retests an existing key's stored plaintext (design doc
// §8 POST .../keys/:keyId/test). Rejects up front, without any network
// call, if the key needs re-entry (its authorized_destination_version
// doesn't match the provider's current destination_version) — design doc
// §3's availability rule.
func (s *ProviderService) TestProviderKey(ctx context.Context, providerID, keyID uint, now time.Time) (*ProviderKeyView, error) {
	key, err := s.findKeyForProvider(providerID, keyID)
	if err != nil {
		return nil, err
	}
	provider, err := repository.FindProviderByID(s.db, key.ProviderID)
	if err != nil {
		return nil, err
	}
	if key.AuthorizedDestinationVersion != provider.DestinationVersion {
		return nil, errcode.ErrProviderKeyNeedsReentry
	}

	// Claim test_generation and atomically snapshot encrypted_key in the
	// SAME statement — must happen BEFORE decrypting, not after (codex
	// adversarial review finding). Reading/decrypting key.EncryptedKey
	// (fetched above, before this claim) would let a concurrent plaintext
	// replacement race in between: the claim would then return the NEW
	// config_version while the network test below still ran against the
	// OLD, already-decrypted plaintext, and the CAS write-back would
	// incorrectly accept writing the OLD credential's result onto the NEW
	// credential's row (nothing in the CAS condition itself re-examines
	// which plaintext was actually tested).
	configVersion, testGeneration, encryptedKeySnapshot, err := repository.BeginProviderKeyRetest(s.db, keyID)
	if err != nil {
		return nil, err
	}
	plaintext, err := crypto.Decrypt(s.masterKey, encryptedKeySnapshot)
	if err != nil {
		return nil, fmt.Errorf("decrypt key: %w", err)
	}

	result, testErr := s.client.TestChatCompletion(ctx, provider.BaseURL, plaintext, key.TestModel)
	if testErr != nil {
		// The client itself refused this call (e.g. concurrency cap
		// exceeded) — not a real test outcome. Since TestSuccess is
		// TestOutcome's zero value, silently proceeding to classify a
		// zero-value TestResult here would incorrectly report success
		// (codex adversarial review finding).
		return nil, fmt.Errorf("provider test call could not be started: %w", testErr)
	}
	verificationStatus, overwrite, lastTestResult := classifyTestResult(result)
	_, _ = repository.CommitProviderKeyRetestResult(s.db, keyID, configVersion, testGeneration,
		overwrite, verificationStatus, lastTestResult, key.TestModel, result.DurationMs, now)

	reloaded, err := repository.FindProviderKeyByID(s.db, keyID)
	if err != nil {
		return nil, err
	}
	view := toKeyView(*reloaded, provider.DestinationVersion)
	return &view, nil
}

// BatchTestResult is one row of TestAllProviderKeys' response (design doc §7).
type BatchTestResult struct {
	KeyID        uint   `json:"key_id"`
	Label        string `json:"label"`
	NeedsReentry bool   `json:"needs_reentry"`
	Skipped      bool   `json:"skipped"` // true for needs_reentry or a lost CAS race
	Outcome      *int   `json:"outcome"`
	DurationMs   int64  `json:"duration_ms"`
}

// TestAllProviderKeys sequentially tests every enabled key in sort_order
// (design doc §7) — synchronous and blocking by deliberate design decision
// (not a background job). Keys needing re-entry are skipped without any
// network call.
func (s *ProviderService) TestAllProviderKeys(ctx context.Context, providerID uint, now time.Time) ([]BatchTestResult, error) {
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, providerID)
	if err != nil {
		return nil, err
	}

	results := make([]BatchTestResult, 0, len(keys))
	for _, key := range keys {
		if key.ManagementStatus != model.ProviderKeyStatusEnabled {
			continue
		}
		if key.AuthorizedDestinationVersion != provider.DestinationVersion {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, NeedsReentry: true, Skipped: true})
			continue
		}

		// Claim generation + read encrypted_key atomically — see
		// TestProviderKey's comment for why decrypting a value read BEFORE
		// this claim would be a race (codex adversarial review finding).
		configVersion, testGeneration, encryptedKeySnapshot, beginErr := repository.BeginProviderKeyRetest(s.db, key.ID)
		if beginErr != nil {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, Skipped: true})
			continue
		}
		plaintext, decErr := crypto.Decrypt(s.masterKey, encryptedKeySnapshot)
		if decErr != nil {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, Skipped: true})
			continue
		}

		result, testErr := s.client.TestChatCompletion(ctx, provider.BaseURL, plaintext, key.TestModel)
		if testErr != nil {
			// The client itself refused this call (e.g. concurrency cap
			// exceeded) — not a real outcome, nothing to commit (codex
			// adversarial review finding: TestSuccess is TestOutcome's
			// zero value, so silently classifying an error+zero-value
			// TestResult would incorrectly report success).
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, Skipped: true})
			continue
		}
		verificationStatus, overwrite, lastTestResult := classifyTestResult(result)
		applied, commitErr := repository.CommitProviderKeyRetestResult(s.db, key.ID, configVersion, testGeneration,
			overwrite, verificationStatus, lastTestResult, key.TestModel, result.DurationMs, now)

		outcomeInt := int(result.Outcome)
		results = append(results, BatchTestResult{
			KeyID: key.ID, Label: key.Label, Skipped: commitErr != nil || !applied,
			Outcome: &outcomeInt, DurationMs: result.DurationMs,
		})
	}
	return results, nil
}
