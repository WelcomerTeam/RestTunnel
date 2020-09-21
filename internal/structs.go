package resttunnel

import (
	"time"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
	"golang.org/x/xerrors"
)

// ResponseType represents the type of response the client wants
type ResponseType int8

const (
	// RepondWithResponse defines the client wants the response.
	RepondWithResponse ResponseType = iota
	// RespondWithUUIDCallback defines the client will want the response at a later time.
	RespondWithUUIDCallback
	// NoResponse defines the client does not want the response.
	NoResponse
)

func (rt ResponseType) String() string {
	switch rt {
	case RepondWithResponse:
		return "RespondWithResponse"
	case RespondWithUUIDCallback:
		return "RespondWithUUIDCallback"
	case NoResponse:
		return "NoResponse"
	}
	return ""
}

// ParseResponse converts a response string into a ResponseType value.
// returns an error if the input string does not match known values.
func ParseResponse(responseStr string) (ResponseType, error) {
	switch responseStr {
	case RepondWithResponse.String():
		return RepondWithResponse, nil
	case RespondWithUUIDCallback.String():
		return RespondWithUUIDCallback, nil
	case NoResponse.String():
		return NoResponse, nil
	}
	return RepondWithResponse, xerrors.Errorf("Unknown ResponseType String: '%s', defaulting to RespondWithResponse", responseStr)
}

// AliveResponse represents the response to /alive
type AliveResponse struct {
	Version string `json:"version"`
}

// TunnelRequest represents a RestTunnel request
type TunnelRequest struct {
	ID uuid.UUID

	URI     []byte
	Headers *fasthttp.RequestHeader

	ResponseType ResponseType
	Priority     bool

	Bucket   string
	Callback chan bool
}

// TunnelResponse represents a RestTunnel response
type TunnelResponse struct {
	expiration time.Time

	Complete     bool
	ID           uuid.UUID
	RatelimitHit bool
	Response     *fasthttp.Response
	Error        error
}

// RateLimitResponse represents the structure of a ratelimit
// message from discord
type RateLimitResponse struct {
	Message    string        `json:"message"`
	RetryAfter time.Duration `json:"retry_after"`
	Global     bool          `json:"global"`
}

// DataStamp is a struct to store a time and a corresponding value
type DataStamp struct {
	Time  interface{} `json:"x"`
	Value interface{} `json:"y"`
}

// LineChart is a struct to store LineChart data easier
type LineChart struct {
	Labels   []string  `json:"labels,omitempty"`
	Datasets []Dataset `json:"datasets"`
}

// Dataset is a struct to store data for a Chart
type Dataset struct {
	Label            string        `json:"label"`
	BackgroundColour string        `json:"backgroundColor,omitempty"`
	BorderColour     string        `json:"borderColor,omitempty"`
	Data             []interface{} `json:"data"`
}

// AnalyticResponse returns the response to an api/analytics request
type AnalyticResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Uptime  string `json:"uptime"`
	Charts  struct {
		Hits            LineChart `json:"hits"`
		Misses          LineChart `json:"misses"`
		Waiting         LineChart `json:"waiting"`
		Requests        LineChart `json:"requests"`
		Callbacks       LineChart `json:"callbacks"`
		AverageResponse LineChart `json:"average_response"`
	} `json:"charts"`
	Numbers struct {
		Hits     int64 `json:"hits"`
		Misses   int64 `json:"misses"`
		Requests int64 `json:"requests"`
		Waiting  int64 `json:"waiting"`
	} `json:"numbers"`
}
