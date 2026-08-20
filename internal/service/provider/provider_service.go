// Package provider owns provider management business logic: the destination-
// version-aware running-status computation, and encryption calls around
// internal/repository's pure data access.
package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/providerproto"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/service/modeladmin"
	"github.com/yolorouter/yolorouter/internal/service/providerclient"
	"github.com/yolorouter/yolorouter/pkg/crypto"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

const (
	RunningStatusNotConfigured = "not_configured"
	RunningStatusPending       = "pending_test"
	RunningStatusAvailable     = "available"
	RunningStatusPartial       = "partial"
	RunningStatusUnavailable   = "unavailable"
)

// computeRunningStatus derives the provider's
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

// providerKeyFingerprintProbe is a domain-separation token used by
// VerifyMasterKeyFingerprint: it is encrypted with the master key and the
// ciphertext is persisted, so startup can confirm the configured master key
// still matches the one that encrypted existing provider data.
// The value itself is arbitrary and secret-free, but it must stay STABLE
// across releases — changing it makes every existing database fail the startup
// key-match check. Treat it as frozen once a release ships.
const providerKeyFingerprintProbe = "yolorouter-provider-key-fingerprint-probe-v1"

// minKeyPlaintextLength is a low sanity floor that rejects empty or
// obviously mistyped keys without rejecting any legitimately short one.
// Disclosure risk from short keys is handled separately by keyPrefixFor, which
// stores no plaintext prefix below keyPrefixSafeMinLength — so lowering this
// floor never exposes more of a secret than before.
const minKeyPlaintextLength = 8

type ProviderService struct {
	db      *gorm.DB
	secrets crypto.SecretBox
	client  providerclient.ProviderClient
	// onKeyRetestPassed, when set, is told about every committed retest
	// that PROVED the key works (verification overwritten to passed) — the
	// gateway's key pool listens so a proven recovery releases the key's
	// rate-limit bench. Claimed or inconclusive retests never fire it.
	// observedAt is stamped BEFORE the probe ran: state recorded between
	// the probe and this callback is newer than the proof and must win.
	onKeyRetestPassed func(keyID uint, configVersion int, observedAt time.Time)
}

func NewProviderService(db *gorm.DB, secrets crypto.SecretBox, client providerclient.ProviderClient) *ProviderService {
	return &ProviderService{db: db, secrets: secrets, client: client}
}

// SetKeyRetestPassedListener wires the proven-recovery signal to a
// listener; call before serving traffic.
func (s *ProviderService) SetKeyRetestPassedListener(fn func(keyID uint, configVersion int, observedAt time.Time)) {
	s.onKeyRetestPassed = fn
}

// noteRetestCommitted fires the proven-recovery listener when a committed
// retest actually proved the key: applied, and classification overwrote
// verification to passed. Anything else — lost CAS, inconclusive probe,
// proven failure — is not recovery and stays silent.
func (s *ProviderService) noteRetestCommitted(keyID uint, configVersion int, observedAt time.Time, applied, overwrite bool, verificationStatus int) {
	if s.onKeyRetestPassed != nil && applied && overwrite && verificationStatus == model.VerificationStatusPassed {
		s.onKeyRetestPassed(keyID, configVersion, observedAt)
	}
}

// VerifyMasterKeyFingerprint runs the startup master-key check: on a
// brand-new instance (no fingerprint row yet) it claims one; on an
// existing instance, a decrypt failure means the current master key
// doesn't match whatever key encrypted the stored probe — almost always a
// database restored without its matching config.yaml. Must be called once
// at startup, after migrations run, before the server accepts traffic.
//
// The original "check not-found,
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
	encrypted, encErr := s.secrets.Encrypt(providerKeyFingerprintProbe)
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
	decrypted, decErr := s.secrets.Decrypt(fp.EncryptedProbe)
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
	// admin-supplied since there is no real model mapping yet.
	TestModel        string
	ManagementStatus int // requested status; server independently re-verifies before honoring "enabled"
	// ProviderType selects the wire protocol this provider primarily
	// speaks (openai/anthropic/gemini/responses). Empty normalizes to
	// "openai" via providerproto.ValidateType for backward compatibility.
	ProviderType string
	// ProtocolEndpoints is optional JSON text declaring extra protocols
	// (beyond ProviderType) this provider also accepts, each with its own
	// endpoint URL. Empty means no extra protocols. See providerproto.ValidateEndpoints.
	ProtocolEndpoints string
}

type UpdateProviderInput struct {
	Name    string
	BaseURL string
	// Note, ProviderType and ProtocolEndpoints follow PATCH semantics via
	// pointers: nil means "not supplied in this request, leave unchanged".
	// A non-nil pointer is authoritative and applied as given — even if it
	// points at an empty string — unlike CreateProviderInput's plain string
	// fields, where empty always means "default/clear". A present-empty Note
	// clears the note; a present-empty ProviderType normalizes to "openai"
	// (see providerproto.ValidateType); a present-empty ProtocolEndpoints clears all
	// additional protocols (see providerproto.ValidateEndpoints). Name and BaseURL
	// stay plain strings (binding:"required", always resent). See
	// UpdateProvider's doc comment for why this distinction matters.
	Note              *string
	ProviderType      *string
	ProtocolEndpoints *string
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
	ID                uint              `json:"id"`
	Name              string            `json:"name"`
	ProviderType      string            `json:"provider_type"`
	ProtocolEndpoints string            `json:"protocol_endpoints"`
	BaseURL           string            `json:"base_url"`
	Note              string            `json:"note"`
	ManagementStatus  int               `json:"management_status"`
	RunningStatus     string            `json:"running_status"`
	Keys              []ProviderKeyView `json:"keys"`
	CreatedAt         time.Time         `json:"created_at"`
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
		ProtocolEndpoints: provider.ProtocolEndpoints,
		BaseURL:           provider.BaseURL, Note: provider.Note, ManagementStatus: provider.ManagementStatus,
		RunningStatus: computeRunningStatus(keys, provider.DestinationVersion),
		Keys:          views, CreatedAt: provider.CreatedAt,
	}
}

const (
	// keyPrefixMaxExposed is the most leading characters keyPrefixFor ever
	// stores in plaintext.
	keyPrefixMaxExposed = 10
	// keyPrefixMinHidden is the fewest trailing characters keyPrefixFor always
	// withholds, so the stored prefix never reaches the end of the secret.
	keyPrefixMinHidden = 4
	// keyPrefixMinSafeUnknown is how many characters must stay unknown after a
	// database leak for the remaining secret to resist brute-forcing.
	keyPrefixMinSafeUnknown = 10
	// keyPrefixSafeMinLength is the shortest key for which any prefix is
	// stored: expose the first keyPrefixMaxExposed and at least
	// keyPrefixMinSafeUnknown must remain hidden. Below it, keyPrefixFor stores
	// nothing and the admin identifies the key by its label — preserving the
	// leak-resistance the previous accept-time minimum gave, without blocking
	// the (now permitted) shorter keys some upstreams issue.
	keyPrefixSafeMinLength = keyPrefixMaxExposed + keyPrefixMinSafeUnknown
)

// keyPrefixFor computes the key_prefix: up to the first keyPrefixMaxExposed
// characters, always hiding the last keyPrefixMinHidden+, and nothing at all
// for a key below keyPrefixSafeMinLength.
func keyPrefixFor(plaintext string) string {
	// Rune-sliced, not byte-sliced: the byte
	// version could cut a multi-byte UTF-8 character in half if one
	// happened to straddle the cutoff, producing an invalid UTF-8
	// key_prefix that then round-trips through JSON as U+FFFD.
	runes := []rune(plaintext)
	if len(runes) < keyPrefixSafeMinLength {
		return ""
	}
	n := len(runes) - keyPrefixMinHidden
	if n > keyPrefixMaxExposed {
		n = keyPrefixMaxExposed
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

// CreateProvider creates a provider + first key in one
// transaction, verification_status always starts untested regardless of
// the caller's requested management_status, then (if enabled was
// requested) a real out-of-transaction test decides the final status —
// the "server-side re-verify" step.
func (s *ProviderService) CreateProvider(ctx context.Context, input CreateProviderInput, now time.Time) (*ProviderView, error) {
	if err := validatePlaintextLength(input.KeyPlaintext); err != nil {
		return nil, err
	}
	// Validated up front, before any DB work: an empty ProviderType
	// normalizes to "openai" for backward compatibility with callers that
	// don't supply one yet.
	providerType, err := validateProviderType(input.ProviderType)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrProviderProtocolInvalid, err)
	}
	protocolEndpoints, err := providerproto.ValidateEndpoints(input.ProtocolEndpoints)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errcode.ErrProviderProtocolInvalid, err)
	}
	if _, err := repository.FindProviderByName(s.db, input.Name); err == nil {
		return nil, errcode.ErrProviderNameTaken
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	encryptedKey, err := s.secrets.Encrypt(input.KeyPlaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt key: %w", err)
	}

	provider := &model.Provider{
		Name: input.Name, ProviderType: providerType, ProtocolEndpoints: protocolEndpoints,
		BaseURL: input.BaseURL, Note: input.Note,
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
		if repository.IsUniqueViolation(err) {
			return nil, errcode.ErrProviderNameTaken
		}
		return nil, err
	}

	// Server-side re-verify always runs for a brand-new key regardless of
	// the requested status, so a freshly created key's
	// verification_status/last_test_* reflect a real test result rather
	// than staying silently untested forever if the admin requested
	// disabled — matches the "test first, then open the transaction" ordering applied
	// uniformly. Enabling only happens if it passes. configVersion/
	// testGeneration/snapshotVersion are all 1: CreateProviderWithKey just
	// inserted this row with those exact defaults, so there is no prior
	// row to race against yet (mirrors CreateProviderKeyPendingTest's
	// contract for a subsequently-added key).
	s.runNewPlaintextTestAndCommit(ctx, provider, key.ID, 1, 1, 1, input.KeyPlaintext, input.TestModel,
		input.ManagementStatus == model.ProviderStatusEnabled, now)

	return s.GetProviderDetail(provider.ID)
}

// validatePlaintextLength is the one shared form of the "key plaintext too
// short" check duplicated across CreateProvider/CreateProviderKey/
// UpdateProviderKey — Gin's own binding:"min=20" already blocks every
// HTTP-originated request before it reaches here, so this only fires for
// a non-HTTP caller of these exported service methods; kept as
// defense-in-depth for that reason.
// This used to wrap errcode.ErrProviderTestFailed purely so the handler's
// error mapping would have a matching case — but "key
// too short" is not "the connection test failed", a misleading
// classification if this ever actually fires. Uses its own sentinel now.
func validatePlaintextLength(plaintext string) error {
	if len(plaintext) < minKeyPlaintextLength {
		return fmt.Errorf("%w: key plaintext must be at least %d characters", errcode.ErrProviderKeyTooShort, minKeyPlaintextLength)
	}
	return nil
}

// UpdateProvider handles name/note (no version bump), base_url, and
// provider_type/protocol_endpoints (both atomic destination_version bumps)
// — changing the protocol or its endpoints changes the destination just as
// much as changing base_url does, so both must invalidate every key's
// authorization the same way.
//
// PATCH field-presence semantics: input.Note/ProviderType/ProtocolEndpoints
// are *string. nil means "not supplied in this request, leave unchanged" — a
// name-only edit that omits these fields must not silently clear an existing
// provider's note, flip an anthropic provider back to openai, or drop its
// extra endpoints. A non-nil pointer is authoritative and applied as given,
// even when it points at an empty string: a present-empty Note clears the
// note, a present-empty ProviderType normalizes to "openai" via
// providerproto.ValidateType, and a present-empty ProtocolEndpoints clears all
// additional protocols via providerproto.ValidateEndpoints. This lets the edit UI —
// which always sends these fields — legitimately clear the last extra
// endpoint or repoint the primary protocol without a stale leftover entry
// surviving in the stored JSON. Name and BaseURL stay plain strings because
// they carry binding:"required" and are always resent.
func (s *ProviderService) UpdateProvider(id uint, input UpdateProviderInput, now time.Time) (*ProviderView, error) {
	provider, err := repository.FindProviderByID(s.db, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}

	resolvePatchField := func(current string, input *string, validate func(string) (string, error)) (string, error) {
		if input == nil {
			return current, nil
		}
		normalized, err := validate(*input)
		if err != nil {
			return "", fmt.Errorf("%w: %v", errcode.ErrProviderProtocolInvalid, err)
		}
		return normalized, nil
	}

	providerType, err := resolvePatchField(provider.ProviderType, input.ProviderType, validateProviderType)
	if err != nil {
		return nil, err
	}
	protocolEndpoints, err := resolvePatchField(provider.ProtocolEndpoints, input.ProtocolEndpoints, providerproto.ValidateEndpoints)
	if err != nil {
		return nil, err
	}
	// Note has no validator; nil = leave the stored note untouched (an omitted
	// field must not clobber it), a present value (incl. empty = clear) wins.
	note := provider.Note
	if input.Note != nil {
		note = *input.Note
	}

	// These writes previously ran
	// as independent, non-transactional statements: if UpdateProviderBaseURL
	// committed (bumping destination_version, which instantly invalidates
	// every key's authorization) and UpdateProviderNameNote then failed on a
	// duplicate name, the admin saw a failed request but the base_url change
	// — and its destination_version bump — had already silently landed.
	// All writes now share one transaction so a name conflict rolls back
	// the base_url/protocol change too.
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if input.BaseURL != provider.BaseURL {
			if _, err := repository.UpdateProviderBaseURL(tx, id, input.BaseURL, now); err != nil {
				return err
			}
		}
		// Compared against the NORMALIZED values, not the raw request
		// strings — a semantically-identical re-submit (e.g. protocol_endpoints
		// JSON with reordered keys) must not spuriously bump
		// destination_version.
		if providerType != provider.ProviderType || protocolEndpoints != provider.ProtocolEndpoints {
			if _, err := repository.UpdateProviderProtocol(tx, id, providerType, protocolEndpoints, now); err != nil {
				return err
			}
		}
		if err := repository.UpdateProviderNameNote(tx, id, input.Name, note, now); err != nil {
			if repository.IsUniqueViolation(err) {
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
	// Without an existence check this endpoint would report a false success:
	// toggling a nonexistent provider ID matches zero rows, GORM reports no
	// error, and the caller gets a false 200. The separate FindProviderByID
	// check is collapsed into reading this write's own RowsAffected, rather
	// than paying for two round trips.
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
	TestModel        string // model name every test call for this key uses
	ManagementStatus int
}

type UpdateKeyInput struct {
	Label            string
	Plaintext        *string // nil = no plaintext change ("retest"/label-only path)
	TestModel        string
	ManagementStatus *int // nil = not provided in this request, preserve current status
}

// CreateProviderKey appends a new key to an existing provider's pool
// (POST .../keys). Always goes through the "submit new plaintext"
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

	encryptedKey, err := s.secrets.Encrypt(input.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt key: %w", err)
	}
	// NextSortOrder's read and CreateProviderKeyPendingTest's insert are not
	// atomic, so two concurrent "add key" requests on the same provider can
	// compute the same next sort_order and race on UNIQUE(provider_id,
	// sort_order) — this was being
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
		// Checked BEFORE the generic IsUniqueViolation fallback below — the
		// original version fell
		// through to ErrProviderKeyLabelTaken once retries were exhausted,
		// reintroducing the exact "sort_order collision misreported as a
		// label conflict" bug this retry loop exists to fix. The label was
		// already confirmed available above via FindProviderKeyByLabel, so
		// any sort_order-specific violation reaching this point is never a
		// real label conflict, retries exhausted or not.
		if repository.IsSortOrderUniqueViolation(err) {
			if attempt < maxSortOrderRetries {
				continue
			}
			return nil, fmt.Errorf("could not allocate a unique key position after %d attempts due to concurrent writes, please retry: %w", maxSortOrderRetries, err)
		}
		if repository.IsUniqueViolation(err) {
			return nil, errcode.ErrProviderKeyLabelTaken
		}
		return nil, err
	}

	s.runNewPlaintextTestAndCommit(ctx, provider, key.ID, key.ConfigVersion, key.TestGeneration, snapshotVersion,
		input.Plaintext, input.TestModel, input.ManagementStatus == model.ProviderKeyStatusEnabled, now)

	reloaded, err := repository.FindProviderKeyByID(s.db, key.ID)
	if err != nil {
		return nil, err
	}
	view := toKeyView(*reloaded, provider.DestinationVersion)
	return &view, nil
}

// verificationSeverity ranks a per-destination result by how strongly it
// should drive the aggregate verification outcome, derived FROM
// classifyTestResult so it stays in lockstep with the write rule:
//   - 0: providerclient.TestSuccess (this destination passed).
//   - 2: a decisive failure — classifyTestResult overwrites verification_status
//     to Failed (providerclient.TestAuthFailed / providerclient.TestQuotaUnavailable / non-model-scoped
//     providerclient.TestPermissionDenied). On a retest this must DEMOTE the key.
//   - 1: an inconclusive result — classifyTestResult leaves verification_status
//     untouched (providerclient.TestModelNotFound / providerclient.TestRateLimited / providerclient.TestUnreachable /
//     providerclient.TestTimeout / providerclient.TestUpstreamError / model-scoped providerclient.TestPermissionDenied /
//     providerclient.TestVerificationUnsupported).
func verificationSeverity(r providerclient.TestResult) int {
	if r.Outcome == providerclient.TestSuccess {
		return 0
	}
	vs, overwrite, _ := classifyTestResult(r)
	if overwrite && vs == model.VerificationStatusFailed {
		return 2
	}
	return 1
}

// verifyKeyAllDestinations tests the plaintext credential against EVERY
// routable destination (VerificationTargets) and returns an aggregate: the
// MOST SEVERE per-destination result (ties broken by target order, primary
// first). providerclient.TestSuccess only if every destination passed. A transport-level
// error from ANY destination (err != nil, e.g. the client's concurrency cap)
// aborts with that error and commits nothing — matching the existing "never
// classify a zero-value providerclient.TestResult as success" rule. Upholds
// credential-destination isolation: a key is authorized only after proving it
// works everywhere negotiation can route it.
//
// Severity-ranked, NOT first-encountered-wins: on a RETEST of an
// already-Passed key, a decisive failure at a SECONDARY destination (its
// credential just got rejected) must not be masked by a weaker, inconclusive
// failure at the primary — first-wins would return the primary's
// non-overwriting result and leave the key wrongly Passed/Enabled/routable to
// the rejected destination. Picking the most severe result guarantees any
// decisive failure anywhere demotes the key to Failed.
func (s *ProviderService) verifyKeyAllDestinations(ctx context.Context, provider *model.Provider, plaintext, testModel string) (providerclient.TestResult, error) {
	targets := providerproto.VerificationTargets(provider.ProviderType, provider.ProtocolEndpoints, provider.BaseURL)

	var chosen providerclient.TestResult
	chosenSeverity := -1
	for _, tgt := range targets {
		result, err := s.client.TestChatCompletion(ctx, tgt.Proto, tgt.URL, plaintext, testModel)
		if err != nil {
			return providerclient.TestResult{}, err
		}
		// Strictly greater keeps the FIRST result at each severity level
		// (target order, primary first) and preserves its
		// DurationMs/IsModelScoped. The single-target success case therefore
		// returns exactly that one target's own result, unchanged from before
		// this helper existed.
		if severity := verificationSeverity(result); severity > chosenSeverity {
			chosenSeverity = severity
			chosen = result
		}
	}
	return chosen, nil
}

// runNewPlaintextTestAndCommit is the shared "test first, then open the transaction" flow for any
// brand-new plaintext: run the real test OUTSIDE any
// transaction, classify per the three-tier rule, then commit via the
// snapshot-based CAS. If the CAS is lost to a race, the
// result is silently discarded — a later retest will pick up correctly,
// and this is a best-effort verification convenience, not an operation
// the caller must retry synchronously.
//
// The original version discarded
// TestChatCompletion's error return (`result, _ := ...`). Since providerclient.TestSuccess
// is providerclient.TestOutcome's zero value, an error case (e.g. the client's own
// concurrency cap rejecting the call before any network I/O happened) was
// silently classified as a passing test and could enable a key that was
// never actually verified. err != nil must skip classification/commit
// entirely — the row keeps whatever pre-test state
// CreateProviderKeyPendingTest/SwapProviderKeyPlaintext already forced
// (untested, disabled) until a later attempt actually runs.
func (s *ProviderService) runNewPlaintextTestAndCommit(ctx context.Context, provider *model.Provider, keyID uint, configVersion, testGeneration, snapshotVersion int, plaintext, testModel string, requestEnable bool, now time.Time) {
	result, err := s.verifyKeyAllDestinations(ctx, provider, plaintext, testModel)
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
	// final status of Disabled would just be a no-op UPDATE (WHERE
	// management_status = Disabled SET management_status = Disabled) —
	// wasted regardless of whether it applies, since a mismatched row
	// (concurrent status change already flipped it) leaves the same
	// no-op-with-CAS-miss outcome either way.
	if result.Outcome == providerclient.TestSuccess && requestEnable {
		_, _ = repository.CASProviderKeyManagementStatus(s.db, keyID, model.ProviderKeyStatusDisabled, model.ProviderKeyStatusEnabled, now)
	}
}

// classifyTestResult implements the three-tier
// verification_status write rule. Returns (value-to-write-if-overwriting,
// whether-to-overwrite-at-all, last_test_result-value-to-record).
func classifyTestResult(result providerclient.TestResult) (verificationStatus int, overwrite bool, lastTestResult *int) {
	outcomeInt := int(result.Outcome)
	switch result.Outcome {
	case providerclient.TestSuccess:
		return model.VerificationStatusPassed, true, &outcomeInt
	case providerclient.TestAuthFailed, providerclient.TestQuotaUnavailable:
		return model.VerificationStatusFailed, true, &outcomeInt
	case providerclient.TestPermissionDenied:
		if !result.IsModelScoped {
			return model.VerificationStatusFailed, true, &outcomeInt
		}
		return 0, false, &outcomeInt
	case providerclient.TestModelNotFound, providerclient.TestRateLimited:
		return 0, false, &outcomeInt
	case providerclient.TestUnreachable, providerclient.TestUpstreamError, providerclient.TestTimeout:
		// providerclient.TestTimeout belongs with these, not with the decisive failures: the
		// upstream never got far enough to judge the credential, so a slow
		// destination must not demote a key that is perfectly good.
		return 0, false, &outcomeInt
	case providerclient.TestVerificationUnsupported:
		// The destination's protocol (gemini/responses) has no real
		// success-body validator yet, so a 2xx from it never counts as
		// proof the credential works — verification_status is left
		// untouched (never overwritten to passed), same "inconclusive"
		// shape as providerclient.TestModelNotFound/providerclient.TestRateLimited above. last_test_result
		// is still recorded so the UI can surface "pending/unsupported".
		return 0, false, &outcomeInt
	default:
		return 0, false, &outcomeInt
	}
}

// UpdateProviderKey handles both the label/status-only path (no plaintext
// change — "retest" CAS if a retest is also requested) and the new-plaintext
// path (PATCH .../keys/:keyId).
// verifyKeyEnableAllowed enforces the rule that a
// key can only be enabled if it has actually passed verification against
// the CURRENT destination — shared by every entry point that can request
// enabling a key WITHOUT itself running a fresh test first
// (SetProviderKeyStatus, and UpdateProviderKey's label/status-only path).
// Entry points that DO run a fresh test (runNewPlaintextTestAndCommit)
// decide enablement from that real result instead and never call this.
//
// Both call sites had this gap independently:
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
// providerID (the :id path segment). UpdateProviderKey,
// SetProviderKeyStatus, and TestProviderKey each repeated this exact
// lookup+ownership-check+not-found-translation
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
	// convention. The previous plain
	// `int` field made "omitted" and "explicitly 0" indistinguishable: a
	// label/test_model-only PATCH that left management_status out of the
	// JSON body bound to Go's zero value 0, which was then written
	// straight to the DB — silently corrupting a previously-enabled key
	// (management_status=1) into status 0, neither Enabled nor Disabled.
	if input.Plaintext == nil {
		// Label/test_model/status-only edit — never touches
		// verification_status. Enabling still has to pass
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
			// checked, above — the
			// unconditional write below left a check-then-act window where
			// a concurrent base_url change or retest could invalidate the
			// key between that check and this write.
			applied, err := repository.UpdateProviderKeyLabelAndStatusIfVerified(s.db, keyID, input.Label, input.TestModel, managementStatus,
				model.VerificationStatusPassed, provider.DestinationVersion, now)
			if err != nil {
				if repository.IsUniqueViolation(err) {
					return nil, errcode.ErrProviderKeyLabelTaken
				}
				return nil, err
			}
			if !applied {
				return nil, errcode.ErrProviderKeyNeedsReentry
			}
		} else if err := repository.UpdateProviderKeyLabelAndStatus(s.db, keyID, input.Label, input.TestModel, managementStatus, now); err != nil {
			if repository.IsUniqueViolation(err) {
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
	encryptedKey, err := s.secrets.Encrypt(*input.Plaintext)
	if err != nil {
		return nil, fmt.Errorf("encrypt key: %w", err)
	}

	// SwapProviderKeyPlaintext atomically writes label + test_model +
	// encrypted_key + key_prefix + management_status(disabled) +
	// config_version bump + verification_status reset + test_generation
	// claim in ONE transaction (the previous version split this across
	// 3 separate statements —
	// UpdateProviderKeyLabelAndStatus, then a raw ciphertext/prefix
	// update, then a separate BeginProviderKeyPlaintextSwap call — leaving
	// a window where a crash or concurrent read between them could
	// observe new ciphertext still paired with the OLD credential's
	// config_version/verification_status).
	configVersion, testGeneration, snapshotVersion, err := repository.SwapProviderKeyPlaintext(s.db, keyID,
		input.Label, input.TestModel, encryptedKey, keyPrefixFor(*input.Plaintext), model.ProviderKeyStatusDisabled, now)
	if err != nil {
		if repository.IsUniqueViolation(err) {
			return nil, errcode.ErrProviderKeyLabelTaken
		}
		return nil, err
	}
	// wantsEnabled resolves the caller's actual enable/disable intent for
	// the fresh test about to run: explicit value if given, else the
	// key's status before this edit. This used to be computed above,
	// before the branch split, even though only this plaintext branch
	// reads it.
	wantsEnabled := key.ManagementStatus == model.ProviderKeyStatusEnabled
	if input.ManagementStatus != nil {
		wantsEnabled = *input.ManagementStatus == model.ProviderKeyStatusEnabled
	}
	s.runNewPlaintextTestAndCommit(ctx, provider, keyID, configVersion, testGeneration, snapshotVersion,
		*input.Plaintext, input.TestModel, wantsEnabled, now)

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
// (needs re-entry), is rejected — the admin
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
	// version verifyKeyEnableAllowed just checked — the
	// unconditional write below left a
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

// ReorderProviderKey moves a key up/down one position (atomic swap).
// A no-op at either boundary is not an error.
func (s *ProviderService) ReorderProviderKey(providerID, keyID uint, direction string, now time.Time) error {
	_, err := repository.SwapProviderKeySortOrder(s.db, providerID, keyID, direction, now)
	// This is the one key-lookup endpoint in this file that would otherwise
	// leave gorm.ErrRecordNotFound untranslated,
	// answering 500 InternalError instead of the 400 ProviderKeyNotFound
	// every sibling endpoint (UpdateProviderKey, SetProviderKeyStatus,
	// TestProviderKey) returns for the identical unknown/cross-provider key
	// condition.
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return errcode.ErrProviderKeyNotFound
	}
	return err
}

// TestKeyPreview is the stateless, unpersisted preview
// (POST .../providers/test-key) — never writes to the database, never
// trusted by any later request. providerType is the candidate provider's
// intended provider_type (e.g. "anthropic"); an empty value defaults to
// openai, letting existing callers that don't supply one yet keep working
// unchanged.
func (s *ProviderService) TestKeyPreview(ctx context.Context, baseURL, apiKey, model, providerType string) (providerclient.TestResult, error) {
	return s.client.TestChatCompletion(ctx, providerproto.TypeOf(providerType), baseURL, apiKey, model)
}

// ListModelsPreview fetches the upstream model catalogue for a candidate
// credential (POST .../providers/list-models) so the admin UI can offer a
// picker instead of a free-text model field. Like TestKeyPreview it is
// stateless — nothing is persisted and no later request trusts the result.
func (s *ProviderService) ListModelsPreview(ctx context.Context, baseURL, apiKey, providerType string) (providerclient.ListModelsResult, error) {
	return s.client.ListModels(ctx, providerproto.TypeOf(providerType), baseURL, apiKey)
}

// ListModelsForProvider fetches the upstream model catalogue for an already-
// stored provider (GET .../providers/:id/models) so the candidate-mapping UI
// can offer a picker instead of a free-text model field. Unlike the stateless
// preview, the plaintext lives only server-side: it decrypts one of the
// provider's stored keys here and queries the provider's primary protocol.
// When no usable key exists the result carries providerclient.TestAuthFailed (not an error)
// so the caller falls back to manual entry rather than showing a failure.
func (s *ProviderService) ListModelsForProvider(ctx context.Context, providerID uint) (providerclient.ListModelsResult, error) {
	provider, err := repository.FindProviderByID(s.db, providerID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return providerclient.ListModelsResult{}, errcode.ErrProviderNotFound
		}
		return providerclient.ListModelsResult{}, err
	}
	keys, err := repository.ListProviderKeysByProvider(s.db, providerID)
	if err != nil {
		return providerclient.ListModelsResult{}, err
	}
	key := pickKeyForCatalogueFetch(keys, provider.DestinationVersion)
	if key == nil {
		return providerclient.ListModelsResult{Outcome: providerclient.TestAuthFailed}, nil
	}
	plaintext, err := s.secrets.Decrypt(key.EncryptedKey)
	if err != nil {
		return providerclient.ListModelsResult{}, fmt.Errorf("decrypt key: %w", err)
	}
	return s.client.ListModels(ctx, providerproto.TypeOf(provider.ProviderType), provider.BaseURL, plaintext)
}

// pickKeyForCatalogueFetch chooses the stored key most likely to authenticate
// a model-catalogue request: an enabled, still-current (not needs-reentry)
// key, preferring one already verified, else the first enabled current key.
// Returns nil when none qualify — the catalogue can't be fetched and the
// caller falls back to manual entry. Keys arrive ordered by sort_order.
func pickKeyForCatalogueFetch(keys []model.ProviderKey, destinationVersion int) *model.ProviderKey {
	var fallback *model.ProviderKey
	for i := range keys {
		k := &keys[i]
		if k.ManagementStatus != model.ProviderKeyStatusEnabled {
			continue
		}
		// A needs-reentry key's stored ciphertext predates the current
		// destination version, so it may no longer authenticate — skip it.
		if k.AuthorizedDestinationVersion != destinationVersion {
			continue
		}
		if k.VerificationStatus == model.VerificationStatusPassed {
			return k
		}
		if fallback == nil {
			fallback = k
		}
	}
	return fallback
}

// TestProviderKey retests an existing key's stored plaintext
// (POST .../keys/:keyId/test). Rejects up front, without any network
// call, if the key needs re-entry (its authorized_destination_version
// doesn't match the provider's current destination_version) — the
// availability rule.
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
	// SAME statement — must happen BEFORE decrypting, not after.
	// Reading/decrypting key.EncryptedKey
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
	plaintext, err := s.secrets.Decrypt(encryptedKeySnapshot)
	if err != nil {
		return nil, fmt.Errorf("decrypt key: %w", err)
	}

	// Stamped before the probe: state recorded while the probe, commit, and
	// listener run is newer than this proof and must not be overridden by it.
	probeObserved := time.Now().UTC()
	result, testErr := s.verifyKeyAllDestinations(ctx, provider, plaintext, key.TestModel)
	if testErr != nil {
		// The client itself refused this call (e.g. concurrency cap
		// exceeded) — not a real test outcome. Since providerclient.TestSuccess is
		// providerclient.TestOutcome's zero value, silently proceeding to classify a
		// zero-value providerclient.TestResult here would incorrectly report success.
		return nil, fmt.Errorf("provider test call could not be started: %w", testErr)
	}
	verificationStatus, overwrite, lastTestResult := classifyTestResult(result)
	applied, commitErr := repository.CommitProviderKeyRetestResult(s.db, keyID, configVersion, testGeneration,
		overwrite, verificationStatus, lastTestResult, key.TestModel, result.DurationMs, now)
	if commitErr != nil {
		return nil, fmt.Errorf("record test result: %w", commitErr)
	}
	s.noteRetestCommitted(keyID, configVersion, probeObserved, applied, overwrite, verificationStatus)
	if !applied {
		// The network test ran, but a concurrent plaintext/config edit bumped
		// config_version between BeginProviderKeyRetest and this write, so the
		// CAS matched no row and THIS run's result was not persisted. Reloading
		// and returning the row here would hand back a stale last_test_result
		// (possibly the concurrent edit's own outcome), which the caller would
		// then present as this test's result — a false confirmation. Surface it
		// as a retryable error instead; the batch path reports the same lost
		// race via BatchTestResult.Skipped.
		return nil, errcode.ErrProviderKeyTestNotSaved
	}

	reloaded, err := repository.FindProviderKeyByID(s.db, keyID)
	if err != nil {
		return nil, err
	}
	view := toKeyView(*reloaded, provider.DestinationVersion)
	return &view, nil
}

// BatchTestResult is one row of TestAllProviderKeys' response.
type BatchTestResult struct {
	KeyID        uint   `json:"key_id"`
	Label        string `json:"label"`
	NeedsReentry bool   `json:"needs_reentry"`
	Skipped      bool   `json:"skipped"` // true for needs_reentry, a lost CAS race, or not_run
	// NotRun marks a key the run's budget never reached, or whose probe the
	// budget cut short. It is reported separately from the other Skipped cases
	// because it is not a statement about the key at all: nothing was tested,
	// so the UI must not present it as a failure, and the operator needs to
	// know a second run will pick up where this one stopped.
	NotRun     bool  `json:"not_run"`
	Outcome    *int  `json:"outcome"`
	DurationMs int64 `json:"duration_ms"`
}

// providerBatchTestBudget caps ONE batch-test request end to end. Without it
// the run costs keys × destinations × providerClientTimeout, a product with no
// upper bound that silently outgrows every deadline stacked around it — the
// browser's request budget and, past roughly half an hour, the server's own
// http.Server.WriteTimeout, at which point the handler still commits its
// results but can no longer send them, leaving the operator staring at a
// network error over work that actually happened.
//
// Bounding the run instead of widening those deadlines keeps the cost
// independent of how many keys and endpoints a provider has: whatever the
// budget does not reach comes back marked NotRun, and a second click resumes
// from there. Five minutes is chosen as the ceiling on how long an admin will
// sit watching a synchronous button, not as a figure any healthy run
// approaches — a working provider answers every key in seconds.
//
// A variable rather than a constant only so tests can shorten it; nothing at
// runtime writes to it.
var providerBatchTestBudget = 5 * time.Minute

// TestAllProviderKeys sequentially tests every key in sort_order —
// synchronous and blocking by deliberate design decision (not a background
// job). Management status is deliberately NOT a filter: a not-yet-enabled
// key must still be testable so an admin can verify it before flipping it
// on (the create/enable flow is test-first, so a fresh key is always
// disabled until it passes). Only keys needing re-entry are skipped without
// a network call. Batch test only RECORDS verification results; it never
// changes management_status (enabling stays an explicit admin action).
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

	ctx, cancel := context.WithTimeout(ctx, providerBatchTestBudget)
	defer cancel()

	results := make([]BatchTestResult, 0, len(keys))
	for _, key := range keys {
		if key.AuthorizedDestinationVersion != provider.DestinationVersion {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, NeedsReentry: true, Skipped: true})
			continue
		}
		// Budget spent — report the rest without touching them. Checked before
		// claiming a test generation so an unreached key's bookkeeping stays
		// exactly as it was.
		if ctx.Err() != nil {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, NotRun: true, Skipped: true})
			continue
		}

		// Claim generation + read encrypted_key atomically — see
		// TestProviderKey's comment for why decrypting a value read BEFORE
		// this claim would be a race.
		configVersion, testGeneration, encryptedKeySnapshot, beginErr := repository.BeginProviderKeyRetest(s.db, key.ID)
		if beginErr != nil {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, Skipped: true})
			continue
		}
		plaintext, decErr := s.secrets.Decrypt(encryptedKeySnapshot)
		if decErr != nil {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, Skipped: true})
			continue
		}

		// Stamped before the probe, same as the single-key retest above.
		probeObserved := time.Now().UTC()
		result, testErr := s.verifyKeyAllDestinations(ctx, provider, plaintext, key.TestModel)
		if testErr != nil {
			// The client itself refused this call (e.g. concurrency cap
			// exceeded) — not a real outcome, nothing to commit
			// (providerclient.TestSuccess is providerclient.TestOutcome's
			// zero value, so silently classifying an error+zero-value
			// providerclient.TestResult would incorrectly report success).
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, Skipped: true})
			continue
		}
		// The budget expiring mid-probe cancels the request underneath, so the
		// result describes our own deadline rather than the key. Committing it
		// would record an unreachable/timeout verdict against a credential that
		// was never actually judged.
		if ctx.Err() != nil {
			results = append(results, BatchTestResult{KeyID: key.ID, Label: key.Label, NotRun: true, Skipped: true})
			continue
		}
		verificationStatus, overwrite, lastTestResult := classifyTestResult(result)
		applied, commitErr := repository.CommitProviderKeyRetestResult(s.db, key.ID, configVersion, testGeneration,
			overwrite, verificationStatus, lastTestResult, key.TestModel, result.DurationMs, now)
		if commitErr == nil {
			s.noteRetestCommitted(key.ID, configVersion, probeObserved, applied, overwrite, verificationStatus)
		}

		outcomeInt := int(result.Outcome)
		results = append(results, BatchTestResult{
			KeyID: key.ID, Label: key.Label, Skipped: commitErr != nil || !applied,
			Outcome: &outcomeInt, DurationMs: result.DurationMs,
		})
	}
	return results, nil
}

// validateProviderType adapts providerproto.ValidateType to the
// string-in/string-out shape the provider row and the patch resolver use.
func validateProviderType(t string) (string, error) {
	id, err := providerproto.ValidateType(t)
	return string(id), err
}

// ProviderImpactModelView is one model that references the provider.
// NoOtherRoutableSource is the severity flag: disabling the provider leaves
// this model with no routable candidate anywhere, so requests for it start
// failing rather than falling over to another provider.
type ProviderImpactModelView struct {
	ID                    uint   `json:"id"`
	Name                  string `json:"name"`
	NoOtherRoutableSource bool   `json:"no_other_routable_source"`
}

// ProviderImpactView is what disabling this provider touches: every model
// with a candidate on it, flagged by whether another provider can still
// serve the model. AffectedKeys are the callable keys whose allowlist names
// a model this disable would strand — the callers who feel it; empty when
// nothing is stranded. AllowAllKeyCount likewise counts allow-all keys only
// when something is stranded, since only then do they lose anything.
type ProviderImpactView struct {
	Models           []ProviderImpactModelView       `json:"models"`
	AffectedKeys     []modeladmin.ModelImpactKeyView `json:"affected_keys"`
	AllowAllKeyCount int64                           `json:"allow_all_key_count"`
}

// GetProviderImpact answers "what breaks if I disable this provider" for the
// confirm dialogs. A model counts as still served when at least one candidate
// on a different provider is routable right now — the same routability rule
// the candidate list reports, applied with this provider taken out.
func (s *ProviderService) GetProviderImpact(id uint, now time.Time) (*ProviderImpactView, error) {
	if _, err := repository.FindProviderByID(s.db, id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errcode.ErrProviderNotFound
		}
		return nil, err
	}
	onThis, err := repository.ListModelCandidatesByProviderID(s.db, id)
	if err != nil {
		return nil, err
	}
	if len(onThis) == 0 {
		return &ProviderImpactView{Models: []ProviderImpactModelView{}, AffectedKeys: []modeladmin.ModelImpactKeyView{}}, nil
	}
	modelIDSet := make(map[uint]struct{}, len(onThis))
	modelIDs := make([]uint, 0, len(onThis))
	for _, c := range onThis {
		if _, seen := modelIDSet[c.ModelID]; seen {
			continue
		}
		modelIDSet[c.ModelID] = struct{}{}
		modelIDs = append(modelIDs, c.ModelID)
	}
	models, err := repository.ListModelsByIDs(s.db, modelIDs)
	if err != nil {
		return nil, err
	}
	allCandidates, err := repository.ListModelCandidatesByModelIDs(s.db, modelIDs)
	if err != nil {
		return nil, err
	}
	keysByProvider, err := modeladmin.KeysByProviderForCandidates(s.db, allCandidates)
	if err != nil {
		return nil, err
	}
	// A model is flagged only when disabling this provider actually takes its
	// last source away: the model itself is enabled (the gateway rejects a
	// disabled model before it ever reaches candidate selection, so such a
	// model is already unavailable and loses nothing), this provider currently
	// gives it a routable candidate, and no other provider does. Anything less
	// dresses a harmless disable up as an outage.
	servedElsewhere := make(map[uint]bool, len(modelIDs))
	servedByThis := make(map[uint]bool, len(modelIDs))
	for _, c := range allCandidates {
		if c.Provider == nil {
			continue
		}
		providerEnabled := c.Provider.ManagementStatus == model.ProviderStatusEnabled
		hasKey := modeladmin.ProviderHasAvailableKey(keysByProvider[c.ProviderID], c.Provider.DestinationVersion)
		if modeladmin.CandidateBlockedBy(c, providerEnabled, hasKey) != "" {
			continue
		}
		if c.ProviderID == id {
			servedByThis[c.ModelID] = true
		} else {
			servedElsewhere[c.ModelID] = true
		}
	}
	views := make([]ProviderImpactModelView, 0, len(models))
	strandedIDs := make([]uint, 0, len(models))
	for _, m := range models {
		stranded := m.ManagementStatus == model.ModelStatusEnabled && servedByThis[m.ID] && !servedElsewhere[m.ID]
		if stranded {
			strandedIDs = append(strandedIDs, m.ID)
		}
		views = append(views, ProviderImpactModelView{
			ID:                    m.ID,
			Name:                  m.Name,
			NoOtherRoutableSource: stranded,
		})
	}
	// Keys are affected through the stranded models only: a model that keeps
	// a source elsewhere keeps serving its keys, so those keys lose nothing
	// and must not be named.
	affectedKeys := []modeladmin.ModelImpactKeyView{}
	var allowAll int64
	if len(strandedIDs) > 0 {
		keys, err := repository.ListCallableAPIKeysAllowlistingAny(s.db, strandedIDs, now)
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			affectedKeys = append(affectedKeys, modeladmin.ModelImpactKeyView{ID: k.ID, Remark: k.Remark, KeyPrefix: k.KeyPrefix})
		}
		allowAll, err = repository.CountCallableAllowAllAPIKeys(s.db, now)
		if err != nil {
			return nil, err
		}
	}
	return &ProviderImpactView{Models: views, AffectedKeys: affectedKeys, AllowAllKeyCount: allowAll}, nil
}
