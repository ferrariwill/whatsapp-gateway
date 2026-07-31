package provider

import "net/http"

// IsMetaRateLimited reports Meta HTTP 429 or known throttle Graph codes.
func IsMetaRateLimited(err error) bool {
	me, ok := AsMetaAPIError(err)
	return ok && me != nil && (me.Status == http.StatusTooManyRequests ||
		me.Code == 130429 || me.Code == 80007 || me.Code == 4)
}

// IsMetaRetryable is true for rate limits and 5xx (outbound DLQ without busy-loop).
func IsMetaRetryable(err error) bool {
	me, ok := AsMetaAPIError(err)
	if !ok || me == nil {
		return false
	}
	if IsMetaRateLimited(err) {
		return true
	}
	return me.Status >= 500 && me.Status <= 599
}
