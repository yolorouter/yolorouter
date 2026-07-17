// Package errcode defines system error codes.
package errcode

import "errors"

const (
	Success = 0

	// === Account/session errors (10xxx) — 设计文档 §5"认证与账号安全模型" ===
	AccountInvalidCredentials = 10001
	AccountDisabled           = 10002
	AccountSessionInvalid     = 10003
	AccountCSRFInvalid        = 10004
	AccountLoginLocked        = 10005 // 登录失败次数超限，临时锁定
	AccountLastAdminProtected = 10006 // 操作会导致零个 active 管理员，直接拒绝
	AccountSetupAlreadyDone   = 10007 // 首次启动向导已完成，不可再创建首个管理员
	AccountSetupTokenInvalid  = 10008 // 首次启动向导 token 缺失或不正确
	AccountPageForbidden      = 10009 // 页面级 RBAC：所在用户组无权访问该后台页面

	// === API Key errors (11xxx) — 设计文档 §5"API Key 安全模型" ===
	APIKeyNotFound        = 11001
	APIKeyInvalid         = 11002
	APIKeyExpired         = 11003
	APIKeyRevoked         = 11004
	APIKeyRateLimitedRPM  = 11005
	APIKeyRateLimitedTPM  = 11006
	APIKeyRateLimitedConc = 11007
	APIKeyBudgetExceeded  = 11008

	// === Provider errors (12xxx) ===
	ProviderNotFound         = 12001
	ProviderNameTaken        = 12002
	ProviderDisabled         = 12003
	ProviderTestFailed       = 12004
	ProviderHasModels        = 12005 // 下面还有模型，不能删除
	ProviderNoTestableModel  = 12006 // openai/anthropic 类型测试连接需要至少一个已启用模型
	ProviderMasterKeyMissing = 12007 // AES-256-GCM 主密钥未配置，无法加解密上游 API Key
	ProviderHasRequestLogs   = 12008 // 已产生过请求日志，不能删除（FK 会拒绝，这里提前给出明确错误）

	ProviderModelNotFound   = 12101
	ProviderModelAliasTaken = 12102

	// === User group errors (13xxx) — 设计文档 §5 用户组"一身三任" ===
	UserGroupNotFound       = 13001
	UserGroupNameTaken      = 13002
	UserGroupHasMembers     = 13003 // 组内还有成员，不能删除
	UserGroupInvalidPage    = 13004 // page_permissions 里出现了不存在的页面 key
	UserGroupHasRequestLogs = 13005 // 已产生过请求日志（成本快照），不能删除
	UserNotFound            = 13101
	UserUsernameTaken       = 13102
	UserDisabled            = 13103 // 目标用户已被禁用，不可为其执行该操作（如签发新 API Key）

	// === Relay/gateway errors (14xxx) — 设计文档 §3、§5 ===
	// 网关响应用的是上游原生 wire format（design doc §3），不走 pkg/response/
	// pkg/errcode 信封，所以这个分段目前只有 RequestLogNotFound 这一个真正
	// 被用到的码——之前占位的 RelayModelNotAllowed/RelayUnsupportedField/
	// RelayUpstreamError/RelayNoAvailableProvider 从未被 internal/relay 引用
	// 过（relay 包直接硬编码 OpenAI 错误字符串），是 /simplify 审查发现的死
	// 代码，已删除。
	RequestLogNotFound = 14005 // Phase 8 请求日志详情查询，id 不存在

	// === System internal errors (50001-50099) ===
	InternalError      = 50001
	DatabaseError      = 50002
	InvalidParam       = 50003
	ConfigError        = 50004
	ServiceUnavailable = 50005
)

// Route-level generic errors (M0 infrastructure, not tied to any specific
// domain). InternalError already exists above (50001) for route-level 500
// responses too, so only these three are actually new.
const (
	RouteNotFound         = 90001
	MethodNotAllowed      = 90002
	RequestEntityTooLarge = 90003
)

// ErrorMessages maps error codes to human-readable messages.
var ErrorMessages = map[int]string{
	Success: "success",

	AccountInvalidCredentials: "invalid username or password",
	AccountDisabled:           "account is disabled",
	AccountSessionInvalid:     "session invalid or expired",
	AccountCSRFInvalid:        "csrf check failed",
	AccountLoginLocked:        "account temporarily locked due to repeated login failures",
	AccountLastAdminProtected: "operation refused: would leave no active administrator",
	AccountSetupAlreadyDone:   "first-run setup already completed",
	AccountSetupTokenInvalid:  "setup token invalid or missing",
	AccountPageForbidden:      "your user group does not have access to this page",

	APIKeyNotFound:        "api key not found",
	APIKeyInvalid:         "api key invalid",
	APIKeyExpired:         "api key expired",
	APIKeyRevoked:         "api key revoked",
	APIKeyRateLimitedRPM:  "rate limit exceeded (requests per minute)",
	APIKeyRateLimitedTPM:  "rate limit exceeded (tokens per minute)",
	APIKeyRateLimitedConc: "rate limit exceeded (concurrent requests)",
	APIKeyBudgetExceeded:  "budget limit exceeded",

	ProviderNotFound:         "provider not found",
	ProviderNameTaken:        "provider name already taken",
	ProviderDisabled:         "provider is disabled",
	ProviderTestFailed:       "provider connection test failed",
	ProviderHasModels:        "provider still has models, remove them first",
	ProviderNoTestableModel:  "provider has no enabled model to test with",
	ProviderMasterKeyMissing: "provider master key not configured",
	ProviderHasRequestLogs:   "provider has existing request logs, cannot be deleted",

	ProviderModelNotFound:   "model not found",
	ProviderModelAliasTaken: "model alias already taken",

	UserGroupNotFound:       "user group not found",
	UserGroupNameTaken:      "user group name already taken",
	UserGroupHasMembers:     "user group still has members, reassign them first",
	UserGroupInvalidPage:    "page_permissions contains an unrecognized page key",
	UserGroupHasRequestLogs: "user group has existing request logs, cannot be deleted",
	UserNotFound:            "user not found",
	UserUsernameTaken:       "username already taken",
	UserDisabled:            "target user is disabled",

	RequestLogNotFound: "request log not found",

	InternalError:      "internal error",
	DatabaseError:      "database error",
	InvalidParam:       "invalid parameter",
	ConfigError:        "configuration error",
	ServiceUnavailable: "service unavailable",

	RouteNotFound:         "route not found",
	MethodNotAllowed:      "method not allowed",
	RequestEntityTooLarge: "request entity too large",
}

// Sentinel errors for service layer comparisons. Text is derived from
// ErrorMessages so each message string has exactly one source of truth.
var (
	ErrAccountInvalidCredentials = errors.New(ErrorMessages[AccountInvalidCredentials])
	ErrAccountDisabled           = errors.New(ErrorMessages[AccountDisabled])
	ErrAccountSessionInvalid     = errors.New(ErrorMessages[AccountSessionInvalid])
	ErrAccountLoginLocked        = errors.New(ErrorMessages[AccountLoginLocked])
	ErrAccountLastAdminProtected = errors.New(ErrorMessages[AccountLastAdminProtected])

	ErrAPIKeyNotFound        = errors.New(ErrorMessages[APIKeyNotFound])
	ErrAPIKeyInvalid         = errors.New(ErrorMessages[APIKeyInvalid])
	ErrAPIKeyExpired         = errors.New(ErrorMessages[APIKeyExpired])
	ErrAPIKeyRevoked         = errors.New(ErrorMessages[APIKeyRevoked])
	ErrAPIKeyRateLimitedRPM  = errors.New(ErrorMessages[APIKeyRateLimitedRPM])
	ErrAPIKeyRateLimitedTPM  = errors.New(ErrorMessages[APIKeyRateLimitedTPM])
	ErrAPIKeyRateLimitedConc = errors.New(ErrorMessages[APIKeyRateLimitedConc])
	ErrAPIKeyBudgetExceeded  = errors.New(ErrorMessages[APIKeyBudgetExceeded])

	ErrProviderNotFound         = errors.New(ErrorMessages[ProviderNotFound])
	ErrProviderNameTaken        = errors.New(ErrorMessages[ProviderNameTaken])
	ErrProviderDisabled         = errors.New(ErrorMessages[ProviderDisabled])
	ErrProviderTestFailed       = errors.New(ErrorMessages[ProviderTestFailed])
	ErrProviderHasModels        = errors.New(ErrorMessages[ProviderHasModels])
	ErrProviderNoTestableModel  = errors.New(ErrorMessages[ProviderNoTestableModel])
	ErrProviderMasterKeyMissing = errors.New(ErrorMessages[ProviderMasterKeyMissing])
	ErrProviderHasRequestLogs   = errors.New(ErrorMessages[ProviderHasRequestLogs])

	ErrProviderModelNotFound   = errors.New(ErrorMessages[ProviderModelNotFound])
	ErrProviderModelAliasTaken = errors.New(ErrorMessages[ProviderModelAliasTaken])

	ErrUserGroupNotFound       = errors.New(ErrorMessages[UserGroupNotFound])
	ErrUserGroupNameTaken      = errors.New(ErrorMessages[UserGroupNameTaken])
	ErrUserGroupHasMembers     = errors.New(ErrorMessages[UserGroupHasMembers])
	ErrUserGroupInvalidPage    = errors.New(ErrorMessages[UserGroupInvalidPage])
	ErrUserGroupHasRequestLogs = errors.New(ErrorMessages[UserGroupHasRequestLogs])
	ErrUserNotFound            = errors.New(ErrorMessages[UserNotFound])
	ErrUserUsernameTaken       = errors.New(ErrorMessages[UserUsernameTaken])
	ErrUserDisabled            = errors.New(ErrorMessages[UserDisabled])

	ErrRequestLogNotFound = errors.New(ErrorMessages[RequestLogNotFound])
)

// GetMessage returns the message for the given error code.
func GetMessage(code int) string {
	if msg, ok := ErrorMessages[code]; ok {
		return msg
	}
	return "unknown error"
}
