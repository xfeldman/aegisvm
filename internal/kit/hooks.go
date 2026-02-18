package kit

import (
	"github.com/xfeldman/aegis/internal/registry"
)

// Hooks defines kit-specific lifecycle hooks. Real implementations
// will be provided by Famiglia, OpenClaw, etc. For M3, DefaultHooks
// provides pass-through no-op behavior.
type Hooks interface {
	RenderEnv(app *registry.App, secrets map[string]string) (map[string]string, error)
	ValidateConfig(appConfig map[string]string) error
	OnPublish(app *registry.App, release *registry.Release) error
}

// DefaultHooks is a no-op implementation of Hooks.
type DefaultHooks struct{}

func (d *DefaultHooks) RenderEnv(app *registry.App, secrets map[string]string) (map[string]string, error) {
	return secrets, nil
}

func (d *DefaultHooks) ValidateConfig(appConfig map[string]string) error {
	return nil
}

func (d *DefaultHooks) OnPublish(app *registry.App, release *registry.Release) error {
	return nil
}
