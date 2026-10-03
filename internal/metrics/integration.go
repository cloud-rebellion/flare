package metrics

import "time"

// RecordIntegration records one outbound integration attempt with a fixed,
// process-wide label vocabulary. Callers pass constants describing the
// provider boundary; unknown values collapse to "other" so configuration or
// provider data cannot create unbounded metric series.
func RecordIntegration(provider, operation, channel, outcome string, elapsed time.Duration) {
	labels := map[string]string{
		"provider":  canonicalIntegrationProvider(provider),
		"operation": canonicalIntegrationOperation(provider, operation),
		"channel":   canonicalIntegrationChannel(provider, channel),
		"outcome":   canonicalIntegrationOutcome(outcome),
	}
	Inc("flare_integration_requests", labels)
	Observe("flare_integration_request", labels, elapsed.Seconds())
}

func canonicalIntegrationProvider(provider string) string {
	switch provider {
	case "alerts", "github", "ai", "oidc", "hubspot", "slack", "openai":
		return provider
	default:
		return "other"
	}
}

func canonicalIntegrationOperation(provider, operation string) string {
	switch provider {
	case "alerts":
		if operation == "dispatch" {
			return operation
		}
	case "github":
		if operation == "create_issue" || operation == "user" {
			return operation
		}
	case "ai":
		if operation == "complete" {
			return operation
		}
	case "oidc":
		switch operation {
		case "discover", "exchange", "fetch_userinfo":
			return operation
		}
	case "hubspot":
		if operation == "account_details" {
			return operation
		}
	case "slack":
		if operation == "auth.test" {
			return operation
		}
	case "openai":
		if operation == "models" {
			return operation
		}
	}
	return "other"
}

func canonicalIntegrationChannel(provider, channel string) string {
	switch provider {
	case "alerts":
		switch channel {
		case "email", "log", "slack", "webhook", "automation_webhook":
			return channel
		}
	case "ai":
		switch channel {
		case "openai", "anthropic":
			return channel
		}
	case "github", "oidc", "hubspot", "slack", "openai":
		if channel == "http" {
			return channel
		}
	}
	return "other"
}

func canonicalIntegrationOutcome(outcome string) string {
	if outcome == "ok" || outcome == "error" {
		return outcome
	}
	return "error"
}
