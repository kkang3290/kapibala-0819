package parameters

// Values is a JSON-compatible key-value dictionary.
type Values map[string]any

// AtTaskStart applies the literal group overrides to a copy of base.
func AtTaskStart(base, group Values) Values {
	merged := clone(base)
	for key, value := range group {
		merged[key] = value
	}
	return merged
}

// AtStep applies sticky step overrides. An empty string means no override.
func AtStep(current, overrides Values) Values {
	merged := clone(current)
	for key, value := range overrides {
		if text, ok := value.(string); ok && text == "" {
			continue
		}
		merged[key] = value
	}
	return merged
}

func clone(values Values) Values {
	copy := make(Values, len(values))
	for key, value := range values {
		copy[key] = value
	}
	return copy
}
