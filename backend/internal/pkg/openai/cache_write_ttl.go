package openai

import "github.com/tidwall/gjson"

// Only reported usage determines the lifetime. Missing, partial or inconsistent
// details keep the aggregate's legacy price; request TTLs cannot prove writes.
func CacheCreationTTLTokensFromUsage(value gjson.Result, aggregate int) (int, int) {
	for _, path := range []string{"input_tokens_details", "prompt_tokens_details"} {
		details := value.Get(path + ".cache_write_tokens_details")
		if !details.Exists() {
			continue
		}
		short, long := details.Get("ttl_30m_tokens"), details.Get("ttl_12h_tokens")
		if short.Type != gjson.Number || long.Type != gjson.Number {
			return 0, 0
		}
		a, b := short.Int(), long.Int()
		if a < 0 || b < 0 || float64(a) != short.Float() || float64(b) != long.Float() ||
			a > int64(aggregate) || b != int64(aggregate)-a {
			return 0, 0
		}
		return int(a), int(b)
	}
	return 0, 0
}
