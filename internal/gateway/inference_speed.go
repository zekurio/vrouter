package gateway

import (
	"fmt"
	"net/http"
	"strings"
)

const claudeFastBeta = "fast-mode-2026-02-01"

func inferenceFastMode(payload map[string]any, protocol wireProtocol) (bool, error) {
	field := "service_tier"
	if protocol == messagesProtocol {
		field = "speed"
	}
	value := payload[field]
	if value == nil {
		return false, nil
	}
	speed, ok := value.(string)
	if !ok || speed == "" {
		return false, fmt.Errorf("%s must be a non-empty string", field)
	}
	if protocol == messagesProtocol {
		if speed != "fast" && speed != "standard" {
			return false, fmt.Errorf("speed must be fast or standard")
		}
		return speed == "fast", nil
	}
	return speed == "fast" || speed == "priority", nil
}

func translateInferenceSpeed(in, out map[string]any, source, target wireProtocol) error {
	if _, err := inferenceFastMode(in, source); err != nil {
		return err
	}
	if source == messagesProtocol {
		// Claude's priority scheduling tier is separate from its fast speed.
		// Do not silently turn that scheduling preference into OpenAI fast mode.
		if tier := in["service_tier"]; tier != nil && tier != "auto" {
			return unsupported("Claude service_tier; use speed: fast or standard to select response speed")
		}
		switch in["speed"] {
		case "fast":
			out["service_tier"] = "priority"
		case "standard":
			out["service_tier"] = "default"
		}
		return nil
	}
	tier, _ := in["service_tier"].(string)
	if tier == "" {
		return nil
	}
	if target == responsesProtocol {
		// Chat Completions and Responses share service_tier semantics.
		out["service_tier"] = tier
		return nil
	}
	switch tier {
	case "auto":
		out["service_tier"] = "auto"
	case "default":
		out["speed"] = "standard"
	case "fast", "priority":
		out["speed"] = "fast"
	default:
		return unsupported(fmt.Sprintf("service_tier %q to Claude; use auto, default, fast, or priority", tier))
	}
	return nil
}

func addAnthropicBeta(req *http.Request, beta string) {
	for _, existing := range strings.Split(req.Header.Get("anthropic-beta"), ",") {
		if strings.TrimSpace(existing) == beta {
			return
		}
	}
	value := req.Header.Get("anthropic-beta")
	if value != "" {
		value += ","
	}
	req.Header.Set("anthropic-beta", value+beta)
}

// Keep the provider's sanitized message, type, code and request ID. These
// distinguish throttling, disabled access, unsupported models and credit errors.
func explainFastModeError(body map[string]any, provider string, status int) {
	err := object(body["error"])
	message := str(err["message"])
	if message == "" || message == http.StatusText(status) || message == providerGenericMessage {
		switch status {
		case http.StatusBadRequest:
			message = "The provider rejected the fast-mode request. Check that the selected model supports fast mode, or disable it."
		case http.StatusUnauthorized, http.StatusForbidden:
			message = "The provider denied the fast-mode request. Check this account's credentials and fast-mode access."
		case http.StatusTooManyRequests:
			message = "The provider rate limited or refused fast mode for this account. Retry after the cooldown or disable fast mode."
			if provider == "claude" {
				message += " Claude fast mode requires enabled access and paid usage credits."
			}
		default:
			message = fmt.Sprintf("The provider could not complete the fast-mode request (HTTP %d). Retry later or disable fast mode.", status)
		}
	}
	err["message"] = providerLabel(provider) + " fast mode: " + message
}
