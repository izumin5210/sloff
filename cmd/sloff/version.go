package main

// buildVersion is the sloff binary version exposed as the OpenTelemetry
// `service.version` resource attribute. Defaults to "dev" for unreleased
// builds; release pipelines override it via `-ldflags "-X main.buildVersion=..."`.
var buildVersion = "dev"
