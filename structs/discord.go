package resttunnel

// TooManyRequests is the response structure when being ratelimited
type TooManyRequests struct {
	Global     bool   `json:"global"`
	Message    string `json:"message"`
	RetryAfter int64  `json:"retry_after"`
}
