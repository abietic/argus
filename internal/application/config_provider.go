package application

import (
	"context"

	"github.com/abietic/argus/internal/reviewconfig"
)

// ConfigProvider is the application port for lifecycle-governed
// configuration. Implementations must resolve only published revisions for
// the exact supplied context. Service revalidates the returned bundle and
// freezes it before target execution.
type ConfigProvider interface {
	ResolvePublished(
		context.Context,
		reviewconfig.ResolutionContext,
	) (reviewconfig.ConfigBundle, error)
}

// GovernedConfigProvider extends the legacy published-bundle port with the
// exact lifecycle receipt required by formal AgentStagePlan compilation.
// Deterministic review remains compatible with ConfigProvider; formal agent
// execution must use this stronger port and freeze both artifacts.
type GovernedConfigProvider interface {
	ConfigProvider
	ResolvePublishedWithReceipt(
		context.Context,
		reviewconfig.ResolutionContext,
	) (reviewconfig.ConfigBundle, reviewconfig.ConfigResolutionReceipt, error)
}
