// Package gateway: caller-facing model discovery.
//
// GET /v1/models and GET /v1/models/:model list/retrieve the enabled models
// the authenticated API key may call. The response shape follows the
// caller's wire protocol: the Anthropic SDK always sends anthropic-version,
// so its presence selects the Anthropic envelope; every other client gets
// the OpenAI shape. These handlers intentionally bypass the relay pipeline
// (no request_log, no spend, no RPM/budget/concurrency gates) — model
// discovery is a read-only, no-spend operation that only needs credential
// resolution (APIKeyAuth) plus a revoked/expiry re-check.
package gateway

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/protocols"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// ownedByTag is the owner reported for every model this service exposes.
// A model is a virtual, externally-facing name that may route to several
// upstream provider candidates, so it is not attributed to any one provider.
const ownedByTag = "yolorouter"

// gatewayAPIKeyFromContext reads the APIKey APIKeyAuth stored on the context.
// Same lookup PostChatCompletions inlines; extracted so both discovery
// handlers share one read site.
func gatewayAPIKeyFromContext(c *gin.Context) (*model.APIKey, bool) {
	v, ok := c.Get(gatewayAPIKeyKey)
	if !ok {
		return nil, false
	}
	k, ok := v.(*model.APIKey)
	return k, ok
}

// rejectInvalidKey mirrors the revoked/expired branches of the relay's
// pre-call state check: APIKeyAuth only resolves the credential (it does
// not filter Status/ExpiresAt), so a revoked or expired key would otherwise
// still discover models. Returns true when the key was rejected and the
// error response already written.
func rejectInvalidKey(c *gin.Context, proto protocols.ProtocolID, apiKey *model.APIKey, rid string) bool {
	if apiKey.Status == model.APIKeyStatusRevoked {
		WriteIngressError(c, proto, http.StatusUnauthorized, errTypeAuthentication, "API key revoked", rid)
		return true
	}
	if apiKey.ExpiresAt != nil && apiKey.ExpiresAt.Before(time.Now().UTC()) {
		WriteIngressError(c, proto, http.StatusUnauthorized, errTypeAuthentication, "API key expired", rid)
		return true
	}
	return false
}

func openAIModelObject(m model.Model) gin.H {
	return gin.H{
		"id":       m.Name,
		"object":   "model",
		"created":  m.CreatedAt.Unix(),
		"owned_by": ownedByTag,
	}
}

func anthropicModelObject(m model.Model) gin.H {
	return gin.H{
		"type":         "model",
		"id":           m.Name,
		"display_name": m.Name,
		"created_at":   m.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// writeModelList writes the protocol-appropriate listing envelope for the
// already-filtered, already-sorted models.
func writeModelList(c *gin.Context, proto protocols.ProtocolID, models []model.Model) {
	if proto == protocols.ProtocolClaude {
		data := make([]gin.H, 0, len(models))
		for _, m := range models {
			data = append(data, anthropicModelObject(m))
		}
		// Anthropic's list envelope uses null (not "") for the cursor fields
		// when there are no items; a typed nil serializes to JSON null.
		var first, last any
		if len(models) > 0 {
			first = models[0].Name
			last = models[len(models)-1].Name
		}
		c.JSON(http.StatusOK, gin.H{
			"data":     data,
			"has_more": false,
			"first_id": first,
			"last_id":  last,
		})
		return
	}
	data := make([]gin.H, 0, len(models))
	for _, m := range models {
		data = append(data, openAIModelObject(m))
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": data})
}

// writeModelObject writes the protocol-appropriate single-model envelope.
func writeModelObject(c *gin.Context, proto protocols.ProtocolID, m model.Model) {
	if proto == protocols.ProtocolClaude {
		c.JSON(http.StatusOK, anthropicModelObject(m))
		return
	}
	c.JSON(http.StatusOK, openAIModelObject(m))
}

// ListModels is the GET /v1/models handler. It returns the enabled models
// the authenticated key may call, in the caller's wire format. A key with
// AllowAllModels sees every enabled model; otherwise the intersection with
// the key's model allowlist (same rule the relay enforces at call time).
func ListModels(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		proto := IngressProtocolForContext(c)
		rid := requestIDFor(c)
		apiKey, ok := gatewayAPIKeyFromContext(c)
		if !ok {
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "missing gateway auth context", rid)
			return
		}
		if rejectInvalidKey(c, proto, apiKey, rid) {
			return
		}
		all, err := repository.ListModels(db)
		if err != nil {
			logger.Error("gateway: list models", zap.String("request_id", rid), zap.Error(err))
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "internal error", rid)
			return
		}
		var allowed map[uint]struct{}
		if !apiKey.AllowAllModels {
			ids, err := repository.FindAPIKeyModelIDs(db, apiKey.ID)
			if err != nil {
				logger.Error("gateway: list key allowlist", zap.String("request_id", rid), zap.Error(err))
				WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "internal error", rid)
				return
			}
			allowed = make(map[uint]struct{}, len(ids))
			for _, id := range ids {
				allowed[id] = struct{}{}
			}
		}
		out := make([]model.Model, 0, len(all))
		for _, m := range all {
			if m.ManagementStatus != model.ModelStatusEnabled {
				continue
			}
			if allowed != nil {
				if _, ok := allowed[m.ID]; !ok {
					continue
				}
			}
			out = append(out, m)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		writeModelList(c, proto, out)
	}
}

// RetrieveModel is the GET /v1/models/*model handler. A model disabled by
// an admin reports as "does not exist" (no existence leak), and a model
// outside the key's allowlist is rejected with 403 — both matching the
// relay's call-time enforcement so list and call never disagree.
func RetrieveModel(db *gorm.DB) gin.HandlerFunc {
	list := ListModels(db)
	return func(c *gin.Context) {
		// The route's catch-all param arrives with a leading "/" (and may
		// itself contain slashes — model ids like deepseek-ai/DeepSeek-V4
		// are one path's worth of name, not nested segments). Surrounding
		// slashes are ignored to keep the trailing-slash tolerance the old
		// single-segment route got from gin's redirect, and an all-slash
		// (empty) name falls back to the listing for the same reason —
		// "/v1/models/" used to redirect to "/v1/models".
		name := strings.Trim(c.Param("model"), "/")
		if name == "" {
			list(c)
			return
		}
		proto := IngressProtocolForContext(c)
		rid := requestIDFor(c)
		apiKey, ok := gatewayAPIKeyFromContext(c)
		if !ok {
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "missing gateway auth context", rid)
			return
		}
		if rejectInvalidKey(c, proto, apiKey, rid) {
			return
		}
		m, err := repository.FindModelByName(db, name)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				WriteIngressError(c, proto, http.StatusNotFound, errTypeNotFound, "model does not exist", rid)
				return
			}
			logger.Error("gateway: find model", zap.String("request_id", rid), zap.Error(err))
			WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "internal error", rid)
			return
		}
		if m.ManagementStatus != model.ModelStatusEnabled {
			WriteIngressError(c, proto, http.StatusNotFound, errTypeNotFound, "model does not exist", rid)
			return
		}
		if !apiKey.AllowAllModels {
			allowed, err := repository.HasAPIKeyModelAccess(db, apiKey.ID, m.ID)
			if err != nil {
				logger.Error("gateway: check model allowlist", zap.String("request_id", rid), zap.Error(err))
				WriteIngressError(c, proto, http.StatusInternalServerError, errTypeServer, "internal error", rid)
				return
			}
			if !allowed {
				WriteIngressError(c, proto, http.StatusForbidden, errTypePermission, "model is not in this API key's allowlist", rid)
				return
			}
		}
		writeModelObject(c, proto, *m)
	}
}
