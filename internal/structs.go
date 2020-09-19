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
