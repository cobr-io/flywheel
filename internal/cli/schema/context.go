package schema

import "github.com/cobr-io/flywheel/internal/naming"

// Core is the flywheel.yaml projection shared by every template render path —
// `flywheel init` (client-skeleton), `flywheel up` (bootstrap tree) and
// `flywheel add app` (per-app + apps templates). Each path defines its own
// context struct that EMBEDS Core (so `{{ .ClientName }}` and friends resolve
// via Go's field promotion) and adds its own typed extras.
//
// NewCore is the single cfg→context constructor: any field derivable from a
// parsed flywheel.yaml is mapped here exactly once, so the field names can no
// longer drift between commands (the historical "Domain" vs "LocalDomain"
// split) and a missing/misnamed template placeholder is a compile-time struct
// error instead of a per-command runtime `missingkey=error`.
//
// Only fields that more than one render path shares live in Core; single-path
// values (cluster identity for init, image refs for bootstrap, per-app inputs
// for add-app) stay on the owning command's context struct.
type Core struct {
	ClientName        string // client.name
	Domain            string // local.domain — the Ingress host suffix
	AppsNamespace     string // namespaces.apps — the configured default apps namespace
	FlywheelNamespace string // fixed at naming.FlywheelNamespace (not client-configurable)
	FluxIntervalLocal string // flux.interval_local
}

// NewCore projects a parsed flywheel.yaml onto the shared render context. It is
// the ONE place cfg fields become context fields; command contexts call it and
// then layer on their own extras. `flywheel init`, which has no parsed config
// yet, synthesises the schema.File it is about to render and runs it through
// here too, so no command hand-rolls the mapping.
func NewCore(f *File) Core {
	return Core{
		ClientName:        f.Client.Name,
		Domain:            f.Local.Domain,
		AppsNamespace:     f.Namespaces.Apps,
		FlywheelNamespace: naming.FlywheelNamespace,
		FluxIntervalLocal: f.Flux.IntervalLocal,
	}
}
