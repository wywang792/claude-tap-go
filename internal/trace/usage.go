package trace

// NormalizeUsage maps provider-specific token field names to shared field names.
func NormalizeUsage(usage map[string]interface{}) map[string]interface{} {
	if usage == nil {
		return map[string]interface{}{}
	}

	normalized := make(map[string]interface{}, len(usage))
	for k, v := range usage {
		normalized[k] = v
	}

	if _, ok := normalized["input_tokens"]; !ok {
		if v, ok := usage["prompt_tokens"]; ok {
			normalized["input_tokens"] = v
		}
		if v, ok := usage["promptTokenCount"]; ok {
			normalized["input_tokens"] = v
		}
	}

	if _, ok := normalized["output_tokens"]; !ok {
		if v, ok := usage["completion_tokens"]; ok {
			normalized["output_tokens"] = v
		}
		if v, ok := usage["candidatesTokenCount"]; ok {
			normalized["output_tokens"] = v
		}
	}

	if _, ok := normalized["cache_read_input_tokens"]; !ok {
		var cached interface{}
		switch {
		case usage["cached_tokens"] != nil:
			cached = usage["cached_tokens"]
		case usage["cachedContentTokenCount"] != nil:
			cached = usage["cachedContentTokenCount"]
		default:
			for _, detailsKey := range []string{"input_tokens_details", "prompt_tokens_details"} {
				if details, ok := usage[detailsKey].(map[string]interface{}); ok {
					if v, ok := details["cached_tokens"]; ok {
						cached = v
						break
					}
				}
			}
		}
		if cached != nil {
			normalized["cache_read_input_tokens"] = cached
		}
	}

	return normalized
}
