package gateway

import (
	"context"

	"go.uber.org/zap"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/settings"
	"github.com/yolorouter/yolorouter/pkg/logger"
)

// SettingsProvider is the read-only window the gateway has into the cached
// global custom system prompt and input-compression switch. Implemented by
// the system settings service and injected into Service by the router.
// The gateway imports only the neutral settings DTO (not the service
// package), so there is no import cycle.
type SettingsProvider interface {
	CustomSystemPrompt(ctx context.Context) (settings.CustomSystemPromptSetting, int64, error)
	GetInputCompression(ctx context.Context) (bool, int64, error)
	GetVisionFallback(ctx context.Context) (settings.VisionFallbackSetting, int64, error)
}

// requestSettings is every setting one request needs, resolved once. Each
// field follows the same two-level rule: a per-key override wins outright
// (short-circuit, so a stalled settings read can never block an override
// key); otherwise the global cached value applies. A failed global read
// keeps whatever the provider returned alongside the error — its
// last-known-good snapshot, or the zero value on a cold cache. A settings
// hiccup must not silently flip a configured feature off, and must never
// block the request.
type requestSettings struct {
	CompressEnabled           bool
	CustomSystemPromptEnabled bool
	CustomSystemPrompt        string
	VisionFallbackModel       string
	VisionFallbackPrompt      string
}

// resolveRequestSettings resolves every field of requestSettings in one
// call, so the entry point receives already-resolved values instead of
// doing the settings resolution inline. Vision fallback has no per-key
// layer; it joins as the natural no-override case. Read failures are
// logged with the request id and follow the fail-open rule documented on
// requestSettings.
func resolveRequestSettings(ctx context.Context, sp SettingsProvider, apiKey *model.APIKey, requestID string) requestSettings {
	var out requestSettings

	if apiKey.CompressEnabledOverride {
		out.CompressEnabled = apiKey.CompressEnabled
	} else if sp != nil {
		enabled, _, err := sp.GetInputCompression(ctx)
		if err != nil {
			logger.Warn("gateway: input compression read failed",
				zap.String("request_id", requestID), zap.Error(err))
		}
		// Assigned even on a read error: fail-open per the requestSettings
		// doc, the value being the provider's last-known-good (or zero on a
		// cold cache).
		out.CompressEnabled = enabled
	}

	if apiKey.CustomSystemPromptEnabledOverride {
		out.CustomSystemPromptEnabled = apiKey.CustomSystemPromptEnabled
		out.CustomSystemPrompt = apiKey.CustomSystemPrompt
	} else if sp != nil {
		g, _, err := sp.CustomSystemPrompt(ctx)
		if err != nil {
			logger.Warn("gateway: custom system prompt read failed",
				zap.String("request_id", requestID), zap.Error(err))
		}
		// Applied even on a read error: fail-open per the requestSettings doc.
		out.CustomSystemPromptEnabled = g.Enabled
		out.CustomSystemPrompt = g.Text
	}

	if sp != nil {
		vf, _, err := sp.GetVisionFallback(ctx)
		if err != nil {
			logger.Warn("gateway: vision fallback settings read failed",
				zap.String("request_id", requestID), zap.Error(err))
		}
		// Assigned even on a refresh error: fail-open per the
		// requestSettings doc — dropping the value here would mean stripping
		// images a configured model could have described.
		out.VisionFallbackModel = vf.Model
		out.VisionFallbackPrompt = vf.Prompt
	}

	return out
}
