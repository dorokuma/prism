package proxy

import (
	"context"
	"fmt"
	"strings"

	"github.com/dorokuma/prism/internal/cache"
	"github.com/dorokuma/prism/internal/config"
)

// modelCacheCtxKey carries the *cache.ModelCache into the request context so
// the chat/responses/messages handlers can resolve the aggregate provider
// without changing their signatures (58+ direct test call sites construct
// requests without a handler; the context value is simply absent there and
// aggregate resolution is skipped, matching provider_routing off).
type modelCacheCtxKey struct{}

func withModelCache(ctx context.Context, mc *cache.ModelCache) context.Context {
	return context.WithValue(ctx, modelCacheCtxKey{}, mc)
}

func modelCacheFromContext(ctx context.Context) *cache.ModelCache {
	mc, _ := ctx.Value(modelCacheCtxKey{}).(*cache.ModelCache)
	return mc
}

// aggregateResolution is the outcome of provider_routing: auto model
// resolution for one request.
type aggregateResolution struct {
	// provider is non-empty iff the request should route to it.
	provider string
	// normalizedModel is the client-requested model name after strings.TrimSpace (virtual name, not remapped).
	normalizedModel string
	// routeModel is the model name used for upstream routing (remapped if model_remap_enabled).
	routeModel string
	// code is "" (resolved), "ambiguous_provider" or "unknown_model".
	code string
	// message is the structured error detail when code is non-empty.
	message string
	// candidates lists the providers advertising the model (resolution
	// observability; empty when an override or unique candidate decided).
	candidates []string
}

// resolveAggregateProvider implements the aggregate entry resolution chain:
// model_provider_overrides > unique registry candidate > provider_priority.
// Ambiguous (multiple providers, no disambiguation) and unknown (no provider
// advertises the model) outcomes are fail-closed: the caller must reject with
// a structured error and never guess. Resolution runs on the POST-remap model
// name: the registry owns real upstream model ids (a virtual model_remap
// name is not advertised).
//
// Callers must only invoke it when the request carries no explicit
// X-Prism-Provider header; pinned requests bypass aggregate entirely.
func resolveAggregateProvider(cfg *config.Config, mc *cache.ModelCache, model string) aggregateResolution {
	if cfg == nil || mc == nil || cfg.ProviderRouting != "auto" {
		return aggregateResolution{}
	}
	normalized := strings.TrimSpace(model)
	if normalized == "" {
		return aggregateResolution{
			code:    "missing_model",
			message: "model is required",
		}
	}
	routeModel := normalized
	if cfg.ModelRemapEnabled {
		if remapped := cfg.RemapModel(normalized); remapped != "" {
			routeModel = remapped
		}
	}
	provider, candidates, status := mc.ResolveProvider(routeModel)
	switch status {
	case cache.RoutingResolved:
		return aggregateResolution{
			provider:        provider,
			normalizedModel: normalized,
			routeModel:      routeModel,
			candidates:      candidates,
		}
	case cache.RoutingAmbiguous:
		return aggregateResolution{
			code:            "ambiguous_provider",
			normalizedModel: normalized,
			routeModel:      routeModel,
			message: fmt.Sprintf(
				"model %q is served by multiple providers; configure model_provider_overrides or provider_priority",
				routeModel),
			candidates: candidates,
		}
	case cache.RoutingModelMissing:
		return aggregateResolution{
			code:    "missing_model",
			message: "model is required",
		}
	default:
		return aggregateResolution{
			code:            "unknown_model",
			normalizedModel: normalized,
			routeModel:      routeModel,
			message:         fmt.Sprintf("model %q is not served by any configured provider", routeModel),
		}
	}
}
