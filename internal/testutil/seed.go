package testutil

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/pkg/crypto"
)

// SeedRequestLog inserts one request_logs row with the given shape. The
// mutator applies test-specific overrides (status, fail_reason, attempts,
// attempts_detail, FKs) after the sensible defaults are filled in.
func SeedRequestLog(t *testing.T, db *gorm.DB, requestID string, ts time.Time, mut func(*model.RequestLog)) {
	t.Helper()
	row := model.RequestLog{
		RequestID:    requestID,
		ModelName:    "gpt-4o-mini",
		IsStream:     false,
		StatusCode:   200,
		InputTokens:  10,
		OutputTokens: 20,
		CostMicros:   100,
		CostKnown:    true,
		Attempts:     1,
		DurationMs:   42,
		CreatedAt:    ts.UTC(),
	}
	if mut != nil {
		mut(&row)
	}
	// Seeded with a plain create: testutil is imported by the repository
	// package's own tests, so the repository helpers are unreachable from
	// here (import cycle). CreatedAt is always set above.
	if err := db.Create(&row).Error; err != nil {
		t.Fatalf("SeedRequestLog %q: %v", requestID, err)
	}
}

// seedUser inserts a users row with the given username and returns its ID,
// so the API key SeedAPIKey creates below has a real owner to reference.
func seedUser(t *testing.T, db *gorm.DB, username string) uint {
	t.Helper()
	now := time.Now().UTC()
	u := model.User{
		Username:  username,
		Role:      model.RoleMember,
		Status:    model.UserStatusEnabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed user %q: %v", username, err)
	}
	return u.ID
}

// SeedAPIKey inserts an account named owner plus a minimal api_keys row it
// owns (unique key_hash), returning the key ID and the owner's user ID.
func SeedAPIKey(t *testing.T, db *gorm.DB, owner string) (uint, uint) {
	t.Helper()
	now := time.Now().UTC()
	userID := seedUser(t, db, owner)
	ak := model.APIKey{
		KeyHash:   "test-hash-" + owner + "-" + now.Format("150405.000000000"),
		KeyPrefix: "sk-xx-",
		UserID:    userID,
		Status:    model.APIKeyStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(&ak).Error; err != nil {
		t.Fatalf("seed api_key %q: %v", owner, err)
	}
	return ak.ID, userID
}

// SeedProvider inserts a providers row and returns its ID, so provider_id
// on request_logs can JOIN to a real provider_name in list/detail tests.
func SeedProvider(t *testing.T, db *gorm.DB, name string) uint {
	t.Helper()
	p := model.Provider{
		Name: name, ProviderType: "openai", BaseURL: "https://example.com/v1",
		ManagementStatus: model.ProviderStatusEnabled,
	}
	if err := db.Create(&p).Error; err != nil {
		t.Fatalf("seed provider %q: %v", name, err)
	}
	return p.ID
}

// ProviderMasterKey returns the fixed 32-byte AES-256-GCM key the handler
// and router tests encrypt provider credentials with. One definition so a
// fixture that must decrypt what another fixture encrypted cannot drift.
func ProviderMasterKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

// ProviderSecrets is ProviderMasterKey boxed for constructors that take the
// SecretBox custodian; tests that exercise the wire format directly keep
// using the raw bytes.
func ProviderSecrets() crypto.SecretBox {
	return crypto.NewSecretBox(ProviderMasterKey())
}

// ownerSeq feeds SeedKeyOwner with unique usernames — several keys created
// in one test each get their own owner without username collisions.
var ownerSeq uint64

// SeedKeyOwner inserts a user to own a test key. CreateAPIKey refuses an
// ownerless key, so every create call in the api-key tests names an owner
// explicitly through this helper.
func SeedKeyOwner(t *testing.T, db *gorm.DB) uint {
	t.Helper()
	now := time.Now().UTC()
	u := &model.User{
		Username:  fmt.Sprintf("key-owner-%d", atomic.AddUint64(&ownerSeq, 1)),
		Role:      model.RoleAdmin,
		Status:    model.UserStatusEnabled,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := db.Create(u).Error; err != nil {
		t.Fatalf("seed key owner: %v", err)
	}
	return u.ID
}

// BlockTableWrites installs a SQLite trigger that turns every future
// statement of the given kind ("UPDATE"/"INSERT"/"DELETE") against table
// into an error, while leaving every other table (and every other
// statement kind on the same table) untouched. Used to force a repository
// call to fail without corrupting the schema outright (as DropTable does),
// so earlier reads in the same code path still succeed.
func BlockTableWrites(t *testing.T, db *gorm.DB, table, kind string) {
	t.Helper()
	stmt := fmt.Sprintf(
		"CREATE TRIGGER block_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END",
		strings.ToLower(kind), table, kind, table,
	)
	if err := db.Exec(stmt).Error; err != nil {
		t.Fatalf("create blocking trigger on %s: %v", table, err)
	}
}

// DropTable removes a table outright, forcing every subsequent read or
// write against it to fail — used for the "repository call errors for a
// reason that isn't gorm.ErrRecordNotFound" branches.
func DropTable(t *testing.T, db *gorm.DB, table string) {
	t.Helper()
	// Disable FK enforcement first: provider_keys.provider_id references
	// providers(id), and SQLite refuses to drop a table another table's FK
	// still points at while enforcement is on — tests need to drop just one
	// side (e.g. providers) while leaving the other (provider_keys) intact
	// and queryable.
	if err := db.Exec("PRAGMA foreign_keys = OFF").Error; err != nil {
		t.Fatalf("disable foreign_keys pragma: %v", err)
	}
	if err := db.Exec("DROP TABLE " + table).Error; err != nil {
		t.Fatalf("drop table %s: %v", table, err)
	}
}
