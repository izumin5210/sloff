package main

// buildVersion is the sloff binary version exposed as the OpenTelemetry
// `service.version` resource attribute and surfaced through the `version`
// subcommand. tagpr rewrites this literal on every release PR; goreleaser
// additionally injects the tag-derived value at build time via
// `-ldflags "-X main.buildVersion=..."`.
var buildVersion = "0.0.0"
