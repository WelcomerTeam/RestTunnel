package resttunnel

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	structs "github.com/TheRockettek/RestTunnel/structs"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// BaseResponse is the structure of all REST requests
type BaseResponse struct {
	Error   string      `json:"error,omitempty"`
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Queued  *bool       `json:"queued,omitempty"`
	UUID    *uuid.UUID  `json:"uuid,omitempty"`
}

func passResponse(rw http.ResponseWriter, data interface{}, queued *bool, success bool, status int) {
	var resp []byte
	var err error
	if success {
		// If there is no error, we will instead use data as the queued boolean value as we should
		// not be queueing up an event if it errors. Hacky solution but works nonetheless.
		resp, err = json.Marshal(BaseResponse{
			Success: true,
			Data:    data,
			Queued:  queued,
		})
	} else {
		resp, err = json.Marshal(BaseResponse{
			Success: false,
			Error:   data.(string),
		})
	}

	if err != nil {
		resp, _ := json.Marshal(BaseResponse{
			Success: false,
			Error:   err.Error(),
		})
		http.Error(rw, string(resp), http.StatusInternalServerError)
		return
	}

	if success {
		rw.WriteHeader(status)
		rw.Write(resp)
	} else {
		http.Error(rw, string(resp), status)
	}
	return
}

// AliveHandler returns the RestTunnel version as a way of signifying it is ready to serve
func AliveHandler(rt *RestTunnel) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		passResponse(rw, structs.AliveResponse{
			Name:    "RestTunnel",
			Version: VERSION,
			Reverse: rt.Configuration.ReverseRoute.Enabled,
		}, nil, true, http.StatusOK)
	}
}

// AnalyticsHandler handles returning the RestTunnel analytics
func AnalyticsHandler(rt *RestTunnel) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		now := time.Now()
		ar := structs.AnalyticResponse{
			Uptime: DurationTimestamp(now.Sub(rt.Start)),
		}

		background, border := "rgba(149, 165, 165, 0.5)", "#7E8C8D"
		ar.Charts.Hits = createLineChart(rt.AnalyticsHit, background, border)
		ar.Charts.Misses = createLineChart(rt.AnalyticsMiss, background, border)
		ar.Charts.Waiting = createLineChart(rt.AnalyticsWaiting, background, border)
		ar.Charts.Requests = createLineChart(rt.AnalyticsRequests, background, border)
		ar.Charts.Callbacks = createLineChart(rt.AnalyticsCallbacks, background, border)
		ar.Charts.AverageResponse = createLineChart(rt.AnalyticsAverageResponse, background, border)

		ar.Numbers.Hits = rt.AnalyticsHit.Sum()
		ar.Numbers.Misses = rt.AnalyticsMiss.Sum()
		ar.Numbers.Waiting = rt.AnalyticsWaiting.Sum()
		ar.Numbers.Requests = rt.AnalyticsRequests.Sum()

		passResponse(rw, ar, nil, true, http.StatusOK)
	}
}

// CallbacksHandler handles returning a callback to a client
func CallbacksHandler(rt *RestTunnel) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)

		_id, ok := vars["id"]
		if !ok {
			passResponse(rw, "No UUID passed", nil, false, http.StatusBadRequest)
			return
		}

		id, err := uuid.Parse(_id)
		if err != nil {
			passResponse(rw, "Invalid UUID passed", nil, false, http.StatusBadRequest)
			return
		}

		rt.callbacksMu.RLock()
		callback, ok := rt.Callbacks[id]
		rt.callbacksMu.RUnlock()

		if !ok {
			passResponse(rw, fmt.Sprintf(ErrCallbackDoesNotExist, id), nil, false, http.StatusGone)
			return
		}

		if !callback.Complete {
			select {
			case <-callback.CompleteC:
				break
			case <-time.Tick(time.Second * 2):
				// If the callback does not finish in 2 seconds,
				// request the client to retry in 3 seconds.

				rw.Header().Set("Retry-After", "3")
				http.Redirect(rw, r, r.URL.String(), 301)
				return
			}

			rt.callbacksMu.RLock()
			callback, ok = rt.Callbacks[id]
			rt.callbacksMu.RUnlock()

			if !ok {
				passResponse(rw, fmt.Sprintf(ErrCallbackDoesNotExist, id), nil, false, http.StatusGone)
				return
			}
		}

		rw.Header().Set("RT-Hit", strconv.FormatBool(callback.RatelimitHit))
		rw.Header().Set("RT-UUID", callback.ID.String())

		callback.Response.Header.VisitAll(func(key []byte, value []byte) {
			rw.Header().Set(string(key), string(value))
		})

		body, _ := rt.DecodeBody(callback.Response)
		rw.Header().Set("Content-Encoding", "")

		rw.Write(body)
		rw.WriteHeader(callback.Response.StatusCode())
		return
	}
}
