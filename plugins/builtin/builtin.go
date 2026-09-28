// Package builtin selects the plugins compiled into the default suxen
// distribution. Each plugin is imported from its own build-tagged file, so a
// custom build can exclude it with -tags (for example
// "-tags suxen_no_gcs,suxen_no_maven"). The default build, and the published
// container image, include every plugin.
//
// Custom distributions that want a different set import the individual plugin
// packages directly instead of this package.
package builtin
