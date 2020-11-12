package resttunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accumulator "github.com/TheRockettek/RestTunnel/pkg/accumulator"
	bucket "github.com/TheRockettek/RestTunnel/pkg/bucket"
	structs "github.com/TheRockettek/RestTunnel/structs"
	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tevino/abool"
	"github.com/valyala/fasthttp"
	"github.com/valyala/fasthttp/fasthttpadaptor"
	"golang.org/x/xerrors"
	"gopkg.in/natefinch/lumberjack.v2"
	"gopkg.in/yaml.v2"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

// VERSION respects semantic versioning
const VERSION = "1.0"

// ConfigurationPath is the path to the file the configration will be located
// at.
const ConfigurationPath = "restTunnel.yaml"

// CallbackExpiration is the duration a callback can exist before it is treated as stale and removed
const CallbackExpiration = time.Minute * 1

// QueueExpiration is the duration a callback can exist before it is treated as stale and removed
const QueueExpiration = time.Minute * 4

// ErrCallbackNotFinished is raised when attempting to retrieve the callback on a request that is not done
var ErrCallbackNotFinished = "Callback '%s' has not completed request"

// ErrCallbackDoesNotExist is raised when referencing a callback that does not exist
var ErrCallbackDoesNotExist = "Callback '%s' does not exist"

// Interval between each analytic sample
const Interval = time.Second * 15

// Samples to hold. 5 seconds and 720 samples is 1 hour
const Samples = 720

// Known domains to lookup
var discordDomains = map[string]bool{
	"discord.com":    true,
	"discordapp.com": true,
}

// TunnelConfiguration represents the configuration for RestTunnel
type TunnelConfiguration struct {
	Host string `json:"host" yaml:"host"`

	ReverseRoute struct {
		Enabled bool   `json:"enabled" yaml:"enabled"`
		Host    string `json:"host" yaml:"host"`
	} `json:"reverse_route" yaml:"reverse_route"`

	Logging struct {
		ConsoleLoggingEnabled bool `json:"console_logging" yaml:"console_logging"`
		FileLoggingEnabled    bool `json:"file_logging" yaml:"file_logging"`

		EncodeAsJSON bool `json:"encode_as_json" yaml:"encode_as_json"` // Make the framework log as json

		Directory  string `json:"directory" yaml:"directory"`     // Directory to log into
		Filename   string `json:"filename" yaml:"filename"`       // Name of logfile
		MaxSize    int    `json:"max_size" yaml:"max_size"`       /// Size in MB before a new file
		MaxBackups int    `json:"max_backups" yaml:"max_backups"` // Number of files to keep
		MaxAge     int    `json:"max_age" yaml:"max_age"`         // Number of days to keep a logfile
	} `json:"logging" yaml:"logging"`
}

// RestTunnel represents the global application state
type RestTunnel struct {
	ctx    context.Context
	cancel func()

	Configuration *TunnelConfiguration `json:"configuration"`

	Logger zerolog.Logger `json:"-"`

	Start time.Time `json:"uptime"`

	HTTP *fasthttp.Client `json:"-"`

	bucketsMu sync.RWMutex
	Buckets   map[string]*bucket.Bucket `json:"buckets"`

	queuesMu sync.RWMutex
	Queues   map[string]*Queue `json:"queue"`

	callbacksMu sync.RWMutex
	Callbacks   map[uuid.UUID]*structs.TunnelResponse `json:"callbacks"`

	// Analytics for ratelimits
	AnalyticsHit  *accumulator.Accumulator
	AnalyticsMiss *accumulator.Accumulator

	// Requests waiting and an atomic cache
	AnalyticsWaiting *accumulator.Accumulator
	analyticsWaiting *int64

	// Analytics for requests and callbacks buffer
	AnalyticsRequests  *accumulator.Accumulator
	AnalyticsCallbacks *accumulator.Accumulator

	// Uses total response time and requests to calculate average
	AnalyticsAverageResponse *accumulator.Accumulator
	analyticsResponseTotal   *int64
	analyticsRequests        *int64

	Router *MethodRouter
}

// Queue represents a Deque with a priority queue
type Queue struct {
	Expiration time.Time

	JobActive   *abool.AtomicBool
	JobsHandled *int64

	Bucket *bucket.Bucket

	events   *int64
	priority *int64

	PriorityEvents chan *structs.TunnelRequest
	Events         chan *structs.TunnelRequest
}

// ErrorResponse is the structure error messages are returned to by the client. We do not
// have a Success response as the raw response is sent.
type ErrorResponse struct {
	Error   string    `json:"error"`
	Success bool      `json:"success"`
	Queued  bool      `json:"queued,omitempty"` // Boolean if the request has already been queued
	UUID    uuid.UUID `json:"uuid,omitempty"`
}

// NewTunnel creates a RestTunnel instance.
func NewTunnel(logger io.Writer) (rt *RestTunnel, err error) {

	ctx, cancel := context.WithCancel(context.Background())
	rt = &RestTunnel{
		ctx:    ctx,
		cancel: cancel,

		Logger: zerolog.New(logger).With().Timestamp().Logger(),

		HTTP: &fasthttp.Client{
			Name:                     "RestTunnel",
			NoDefaultUserAgentHeader: true,
			MaxIdleConnDuration:      time.Second * 5,
			MaxConnDuration:          time.Second * 5,
			ReadTimeout:              time.Second * 5,
			WriteTimeout:             time.Second * 5,
		},
		bucketsMu: sync.RWMutex{},
		Buckets:   make(map[string]*bucket.Bucket),

		queuesMu: sync.RWMutex{},
		Queues:   make(map[string]*Queue),

		callbacksMu: sync.RWMutex{},
		Callbacks:   make(map[uuid.UUID]*structs.TunnelResponse),

		analyticsWaiting:       new(int64),
		analyticsResponseTotal: new(int64),
		analyticsRequests:      new(int64),
	}

	configuration, err := rt.LoadConfiguration(ConfigurationPath)
	if configuration.Host == "" {
		return nil, xerrors.New("No host provided")
	}
	if err != nil {
		return nil, xerrors.Errorf("new resttunnel: %w", err)
	}
	rt.Configuration = configuration

	rt.AnalyticsHit = accumulator.NewAccumulator(rt.ctx, Samples, Interval, "Ratelimit Hits")
	rt.AnalyticsMiss = accumulator.NewAccumulator(rt.ctx, Samples, Interval, "Ratelimit Misses")
	rt.AnalyticsRequests = accumulator.NewAccumulator(rt.ctx, Samples, Interval, "Total Requests")
	rt.AnalyticsCallbacks = accumulator.NewAccumulator(rt.ctx, Samples, Interval, "Callbacks in buffer")
	rt.AnalyticsAverageResponse = accumulator.NewAccumulator(rt.ctx, Samples, Interval, "Average response time")
	rt.AnalyticsWaiting = accumulator.NewAccumulator(rt.ctx, Samples, Interval, "Waiting Requests")

	go rt.TakeOutTheTrash()
	go func() {
		e := time.NewTicker(Interval)
		for {
			select {
			case <-e.C:
				now := time.Now().UTC().Round(time.Second)
				if analyticsRequests := atomic.LoadInt64(rt.analyticsRequests); analyticsRequests == 0 {
					rt.AnalyticsAverageResponse.IncrementBy(0)
				} else {
					rt.AnalyticsAverageResponse.IncrementBy(
						atomic.LoadInt64(rt.analyticsResponseTotal) / analyticsRequests,
					)
				}
				rt.AnalyticsAverageResponse.RunOnce(now)

				wait := int64(0)
				rt.queuesMu.Lock()
				for _, queue := range rt.Queues {
					wait += int64(len(queue.Events) + len(queue.PriorityEvents))
				}
				rt.queuesMu.Unlock()
				atomic.StoreInt64(rt.analyticsWaiting, wait)
				rt.AnalyticsAverageResponse.IncrementBy(wait)
				rt.AnalyticsWaiting.RunOnce(now)

				rt.AnalyticsHit.RunOnce(now)
				rt.AnalyticsMiss.RunOnce(now)
				rt.AnalyticsRequests.RunOnce(now)
				rt.AnalyticsCallbacks.RunOnce(now)
			case <-rt.ctx.Done():
				return
			}
		}
	}()

	var writers []io.Writer
	if rt.Configuration.Logging.ConsoleLoggingEnabled {
		writers = append(writers, logger)
	}
	if rt.Configuration.Logging.FileLoggingEnabled {
		if err := os.MkdirAll(rt.Configuration.Logging.Directory, 0744); err != nil {
			log.Error().Err(err).Str("path", rt.Configuration.Logging.Directory).Msg("Unable to create log directory")
		} else {
			lumber := &lumberjack.Logger{
				Filename:   path.Join(rt.Configuration.Logging.Directory, rt.Configuration.Logging.Filename),
				MaxBackups: rt.Configuration.Logging.MaxBackups,
				MaxSize:    rt.Configuration.Logging.MaxSize,
				MaxAge:     rt.Configuration.Logging.MaxAge,
			}
			if rt.Configuration.Logging.EncodeAsJSON {
				writers = append(writers, lumber)
			} else {
				writers = append(writers, zerolog.ConsoleWriter{
					Out:        lumber,
					TimeFormat: time.Stamp,
					NoColor:    true,
				})
			}
		}
	}
	mw := io.MultiWriter(writers...)
	rt.Logger = zerolog.New(mw).With().Timestamp().Logger()
	rt.Logger.Info().Msg("Logging configured")

	return
}

// Open starts up a HTTP server
func (rt *RestTunnel) Open() (err error) {

	//    ______________________
	//   /                      \
	//  /  ____________________  \ 	   RestTunnel 0
	// |  / \_              _/ \  |
	// |  |   *\_        _/*   |  |	   HTTP: 127.0.0.1:8000
	// |  |      \      /      |  |
	// |  |      *\____/*      |  |	   placeholder
	// |  |       |    |       |  |

	rt.Start = time.Now().UTC()
	rt.Logger.Info().Msgf("Starting RestTunnel\n\n    ______________________   \n   /                      \\  \n  /  ____________________  \\ 	   RestTunnel %s\n |  / \\_              _/ \\  |\n |  |   *\\_        _/*   |  |	   HTTP: %s\n |  |      \\      /      |  |\n |  |      *\\____/*      |  |	   %s\n |  |       |    |       |  |\n",
		VERSION, rt.Configuration.Host, "You are being ratelimited")

	rt.Router = createEndpoints(rt)

	fmt.Printf("Serving on %s (Press CTRL+C to quit)\n", rt.Configuration.Host)
	err = fasthttp.ListenAndServe(rt.Configuration.Host, rt.HandleRequest)

	return
}

// LoadConfiguration loads the configuration for RestTunnel
func (rt *RestTunnel) LoadConfiguration(path string) (configuration *TunnelConfiguration, err error) {
	rt.Logger.Debug().Msg("Loading configuration")

	file, err := ioutil.ReadFile(path)
	if err != nil {
		return configuration, xerrors.Errorf("load configuration readfile: %w", err)
	}

	configuration = &TunnelConfiguration{}
	err = yaml.Unmarshal(file, &configuration)
	if err != nil {
		return configuration, xerrors.Errorf("load configuration unmarshal: %w", err)
	}

	return
}

// TakeOutTheTrash handles cleaning old entries
func (rt *RestTunnel) TakeOutTheTrash() {
	t := time.NewTicker(time.Second)

	for {
		select {
		case <-t.C:
			callbackCleaned, callbackDuration := rt.CollectCallbacks()
			queueCleaned, queueDuration := rt.CollectQueues()

			var waitDuration time.Duration
			if callbackDuration < queueDuration {
				waitDuration = callbackDuration
			} else {
				waitDuration = queueDuration
			}

			if waitDuration < (time.Second * 5) {
				waitDuration = time.Second * 5
			}

			t.Reset(waitDuration)

			if callbackCleaned+queueCleaned > 0 {
				rt.Logger.Info().Int("callbacks", callbackCleaned).Int("queues", queueCleaned).Dur("until", waitDuration).Msg("Cleaned stale entries")
			}
		case <-rt.ctx.Done():
			return
		}
	}
}

// CollectQueues handles cleaning old queues
func (rt *RestTunnel) CollectQueues() (cleaned int, dur time.Duration) {
	now := time.Now().UTC()
	interval := QueueExpiration
	removals := make([]string, 0, len(rt.Queues))

	rt.queuesMu.RLock()
	for queueName, queue := range rt.Queues {
		if queue.Expiration.Before(now) {
			removals = append(removals, queueName)
			continue
		}
		timeUntil := queue.Expiration.Sub(now)
		// If this queue will expire in less than the current
		// interval, set the interval to the new value so we
		// can wait until the most next expiration or wait a
		// minute.
		if timeUntil < interval {
			interval = timeUntil
		}
	}
	rt.queuesMu.RUnlock()
	if len(removals) > 0 {
		rt.queuesMu.Lock()
		for _, queueName := range removals {
			delete(rt.Queues, queueName)
		}
		rt.queuesMu.Unlock()
	}
	return len(removals), interval
}

// CollectCallbacks handles cleaning old callbacks
func (rt *RestTunnel) CollectCallbacks() (cleaned int, dur time.Duration) {
	now := time.Now().UTC()
	interval := CallbackExpiration
	removals := make([]uuid.UUID, 0, len(rt.Callbacks))

	rt.callbacksMu.RLock()
	for callbackID, callback := range rt.Callbacks {
		if callback.Expiration.Before(now) {
			removals = append(removals, callbackID)
			continue
		}
		timeUntil := callback.Expiration.Sub(now)

		// If this queue will expire in less than the current
		// interval, set the interval to the new value so we
		// can wait until the most next expiration or wait a
		// minute.
		if timeUntil < interval {
			interval = timeUntil
		}
	}
	rt.callbacksMu.RUnlock()
	if len(removals) > 0 {
		rt.callbacksMu.Lock()
		for _, callbackID := range removals {
			delete(rt.Callbacks, callbackID)
		}
		rt.callbacksMu.Unlock()
	}
	return len(removals), interval
}

// StartQueueJob starts a queue job
func (rt *RestTunnel) StartQueueJob(q *Queue) {
	q.JobActive.Set()
	defer q.JobActive.UnSet()

	for {
		timeout := time.NewTicker(time.Second * 5)
		// Gives channel PriorityEvents priority
		select {
		case tr := <-q.PriorityEvents:
			atomic.AddInt64(q.JobsHandled, 1)
			rt.HandleQueueJob(tr)
			continue
		default:
		}
		select {
		case tr := <-q.PriorityEvents:
			atomic.AddInt64(q.JobsHandled, 1)
			rt.HandleQueueJob(tr)
			continue
		case tr := <-q.Events:
			atomic.AddInt64(q.JobsHandled, 1)
			rt.HandleQueueJob(tr)
			continue
		case <-timeout.C:
			return
		}
	}
}

// HandleQueueJob handles a TunnelRequest gathered from a queue
func (rt *RestTunnel) HandleQueueJob(tr *structs.TunnelRequest) {
	// This goroutine shows a job that is taking too long.
	stage := "init"
	fin := make(chan bool)
	go func() {
		waitTime := time.Second * 5
		since := time.Now()
		t := time.NewTimer(waitTime)
		for {
			select {
			case <-fin:
				return
			case <-t.C:
				rt.Logger.Warn().Str("stage", stage).Msgf("Job for %s has been in stage '%s' for too long. Been executing for %f seconds. Possible deadlock?", string(tr.URI), stage, time.Now().Sub(since).Round(time.Second).Seconds())
				t.Reset(waitTime)
			}
		}
	}()
	defer func() {
		close(fin)
	}()

	stage = "cbki"

	tunnelResponse := &structs.TunnelResponse{
		Expiration: time.Now().UTC().Add(CallbackExpiration),
		CompleteC:  make(chan bool),
		Complete:   false,
		ID:         tr.ID,
	}

	rt.callbacksMu.Lock()
	rt.Callbacks[tr.ID] = tunnelResponse
	rt.callbacksMu.Unlock()

	stage = "aqrq"
	req := fasthttp.AcquireRequest()
	tr.Req.Request.CopyTo(req)
	req.SetBody(tr.Req.Request.Body())
	req.SetRequestURIBytes(tr.URI)
	req.URI().SetQueryStringBytes(tr.QueryString)

	// Asks discord to send higher precision reset times
	req.Header.Set("X-RateLimit-Precision", "millisecond")

	req.Header.VisitAll(func(key []byte, value []byte) {
		if strings.HasPrefix(strings.ToLower(string(key)), "rt-") {
			req.Header.DelBytes(key)
		}
	})

	var resp *fasthttp.Response
	var hit bool
	var err error

	rt.bucketsMu.RLock()
	_bucket := rt.Buckets[tr.Bucket]
	var globalBucket *bucket.Bucket
	if _bucket.Global != "" {
		globalBucket = rt.Buckets[_bucket.Global]
	}
	rt.bucketsMu.RUnlock()

	for {
		stage = "bckt"

		hit = _bucket.Lock()
		if _bucket.Global != "" {
			hit = globalBucket.Lock() || hit
		}

		if hit {
			rt.AnalyticsHit.Increment()
		} else {
			rt.AnalyticsMiss.Increment()
		}

		stage = "aqrs"
		resp = fasthttp.AcquireResponse()

		stage = "dorq"
		err = rt.HTTP.Do(req, resp)
		rt.AnalyticsRequests.Increment()

		stage = "rtlm"
		if resp.StatusCode() == 429 {
			// ratelimit
			global, err := strconv.ParseBool(string(resp.Header.Peek("X-RateLimit-Global")))
			if err == nil && global {
				// Global
				rt.Logger.Warn().Msg("Hit global ratelimit")
				if _bucket.Global != "" {
					retryAfter, err := strconv.Atoi(string(resp.Header.Peek("Retry-After")))
					if err != nil {
						// Convert ms to ns (x1e6)
						globalBucket.Exhaust(time.Now().UnixNano() + int64(retryAfter*1000000))
					}
				} else {
					rt.Logger.Warn().Str("bucket", _bucket.Name).Msg("Hit global ratelimit with global bucket set.")
				}
			} else {
				// We ignore X-RateLimit-Reset as we already have Reset-After
				rt.Logger.Debug().Msg("Hit endpoint ratelimit")
				rlLimit, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Limit")))
				if err != nil {
					rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Limit '%s' to int", string(resp.Header.Peek("X-RateLimit-Limit")))
				}
				rlResetRaw, err := strconv.ParseFloat(string(resp.Header.Peek("X-RateLimit-Reset-After")), 64)
				if err != nil {
					rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Reset-After '%s' to float", string(resp.Header.Peek("X-RateLimit-Reset-After")))
				}
				rlBucket := string(resp.Header.Peek("X-RateLimit-Bucket"))
				// If we have received 429 we already know it is 0
				// rlRemaining, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Remaining")))
				// if err != nil {
				// 	rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Remaining '%s' to int", string(resp.Header.Peek("X-RateLimit-Remaining")))
				// }

				// Convert seconds to nanoseconds (x1e9) (we add on about 500ms just in case)
				rlReset := time.Now().UnixNano() + int64(rlResetRaw*float64(1000000000)) + 500000000
				_bucket.Exhaust(rlReset)
				_bucket.Modify(int32(rlLimit), atomic.LoadInt64(_bucket.Duration), rlBucket, _bucket.Global)
			}
		} else {
			rlBucket := string(resp.Header.Peek("X-RateLimit-Bucket"))
			// If we receive no X-RateLimit-Bucket it is likely not discord and we will just discard anyway
			if rlBucket != "" {
				rt.Logger.Debug().Msgf("Received bucket '%s'", rlBucket)
				if rlBucket != _bucket.Alias && rlBucket != "" {
					rt.Logger.Debug().Msgf("Discovered alias bucket '%s' for '%s'", rlBucket, _bucket.Name)
					_bucket.Modify(
						atomic.LoadInt32(_bucket.Limit),
						atomic.LoadInt64(_bucket.Duration),
						rlBucket,
						_bucket.Global,
					)
				}

				rlLimit, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Limit")))
				if err != nil {
					rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Limit '%s' to int", string(resp.Header.Peek("X-RateLimit-Limit")))
				}
				rlRemaining, err := strconv.Atoi(string(resp.Header.Peek("X-RateLimit-Remaining")))
				if err != nil {
					rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Remaining '%s' to int", string(resp.Header.Peek("X-RateLimit-Remaining")))
				}
				rlReset, err := strconv.ParseFloat(string(resp.Header.Peek("X-RateLimit-Reset-After")), 64)
				if err != nil {
					rt.Logger.Warn().Msgf("Failed to convert X-RateLimit-Reset-After '%s' to float", string(resp.Header.Peek("X-RateLimit-Reset-After")))
				}

				if rlRemaining == rlLimit-1 {
					// We have just received a new RateLimit so we can now infer the ratelimit
					_bucket.Modify(int32(rlLimit), time.Duration(rlReset*1000000000).Nanoseconds(), rlBucket, _bucket.Global)
				}
			}
			break
		}
	}

	close(tunnelResponse.CompleteC)
	tunnelResponse.Expiration = time.Now().UTC().Add(CallbackExpiration)
	tunnelResponse.Complete = true
	tunnelResponse.RatelimitHit = hit
	tunnelResponse.Response = resp
	tunnelResponse.Error = err

	stage = "clbk"
	rt.callbacksMu.Lock()
	rt.Callbacks[tr.ID] = tunnelResponse
	rt.callbacksMu.Unlock()

	stage = "chan"
	if tr.ResponseType == structs.RespondWithResponse {
		tr.Callback <- true
	}
	stage = "done"
}

// // LazyCallbackJob handles clearing old entries in callback
// func (rt *RestTunnel) LazyCallbackJob() {
// 	interval := time.Second * 5
// 	t := time.NewTicker(interval)
// 	for {
// 		<-t.C
// 		now := time.Now().UTC()
// 		deletions := []uuid.UUID{}
// 		for {
// 			id, tunnelResponse := rt.randomCallback()
// 			if tunnelResponse != nil {
// 				if tunnelResponse.Expiration.Sub(now) <= 0 {
// 					deletions = append(deletions, id)
// 					continue
// 				}
// 			}
// 			break
// 		}
// 		rt.callbacksMu.Lock()
// 		for _, uuid := range deletions {
// 			delete(rt.Callbacks, uuid)
// 		}
// 		rt.callbacksMu.Unlock()
// 	}
// }

// HandleRequest handles a HTTP request given to RestTunnel
func (rt *RestTunnel) HandleRequest(ctx *fasthttp.RequestCtx) {
	startTime := time.Now().UTC()
	var path string

	defer func() {
		ms := time.Now().UTC().Sub(startTime).Milliseconds()
		// Only log if they have not requested / and only
		// count response time if they have requested /
		if path == "/api/analytics" {
			return
		}

		if path != "/" {
			rt.Logger.Info().Msgf("%s %s %s %d %dms",
				ctx.RemoteAddr(),
				ctx.Request.Header.Method(),
				path,
				ctx.Response.StatusCode(),
				ms)
		} else {
			rt.Logger.Info().Msgf("Tunnel: %s %s %dms",
				ctx.Request.Header.Method(),
				ctx.Request.Header.Peek("RT-URL"),
				ms)
			atomic.AddInt64(rt.analyticsResponseTotal, ms)
			atomic.AddInt64(rt.analyticsRequests, 1)
		}
	}()

	path = string(ctx.Request.URI().Path())

	fasthttpadaptor.NewFastHTTPHandler(rt.Router)(ctx)
	if ctx.Response.StatusCode() != 404 {
		ctx.SetContentType("application/json;charset=utf8")
	}
	// If there is no matching router route, we will create a tunnel request
	if ctx.Response.StatusCode() == 404 {
		ctx.Response.Reset()
		rt.TunnelHTTPRequest(ctx)
	}

	return
}

// TunnelHTTPRequest handles creating a tunnel request
func (rt *RestTunnel) TunnelHTTPRequest(ctx *fasthttp.RequestCtx) {
	id := uuid.New()

	hasPriority, err := strconv.ParseBool(string(ctx.Request.Header.Peek("RT-Priority")))
	if err != nil {
		hasPriority = false
	}
	responseType, _ := structs.ParseResponse(string(ctx.Request.Header.Peek("RT-ResponseType")))

	var requestURI []byte
	if rt.Configuration.ReverseRoute.Enabled {
		requestURI = append([]byte(rt.Configuration.ReverseRoute.Host), ctx.Path()...)
	} else {
		requestURI = ctx.Request.Header.Peek("RT-URL")
		if len(requestURI) == 0 {
			ctx.SetStatusCode(400)
			rterr := "Missing URL"
			ctx.Response.Header.Set("Rt-Error", rterr)
			res, err := json.Marshal(ErrorResponse{
				Error:   rterr,
				Success: false,
				Queued:  false,
				UUID:    id,
			})
			if err == nil {
				ctx.Write(res)
			}
			return
		}
	}

	URI := fasthttp.AcquireURI()
	err = URI.Parse(nil, requestURI)
	if err != nil {
		ctx.SetStatusCode(400)
		rterr := "Invalid URL"
		ctx.Response.Header.Set("Rt-Error", rterr)
		res, err := json.Marshal(ErrorResponse{
			Error:   rterr,
			Success: false,
			Queued:  false,
			UUID:    id,
		})
		if err == nil {
			ctx.Write(res)
		}
		return
	}

	rt.Logger.Debug().Str("response", responseType.String()).Bool("priority", hasPriority).Msg("Received request")

	requestHeaders := fasthttp.RequestHeader{}
	ctx.Request.Header.CopyTo(&requestHeaders)

	requestHeaders.Set("Accept-Encoding", "gzip, deflate, br")
	requestHeaders.Set("User-Agent", fmt.Sprintf("restTunnel/%s; %s", VERSION, string(requestHeaders.Peek("User-Agent"))))

	// Create a bucket with <HOST>:<PATH + AUTHORIZATION with SHA256>
	pathHash := sha256.New()
	pathHash.Write(URI.Path()[:])
	pathHash.Write(requestHeaders.Peek("Authorization"))
	initialBucketName := string(URI.Host()) + ":" + hex.EncodeToString(pathHash.Sum(nil))

	// We then traverse the bucket in case it does not exist or it has an alias as some paths may use the same bucket
	_bucket, bucketStack, err := rt.TraverseBucket(initialBucketName)

	if err != nil {
		rt.Logger.Debug().Err(err).Msgf("Bucket '%s' does not exist yet", initialBucketName)
	} else {
		rt.Logger.Debug().Str("bucket", _bucket.Name).Msgf("Traversed bucket: %v", bucketStack)
	}

	var bucketName string
	var queue *Queue

	switch err {
	case bucket.ErrBucketCircularAlias:
		ctx.SetStatusCode(409)
		rterr := fmt.Sprintf(err.Error(), initialBucketName, bucketStack)
		ctx.Response.Header.Set("Rt-Error", rterr)
		res, err := json.Marshal(ErrorResponse{
			Error:   rterr,
			Success: false,
			Queued:  false,
			UUID:    id,
		})
		if err == nil {
			ctx.Write(res)
		}
		return
	case bucket.ErrBucketDoesNotExist:
		// Bucket will be created when request is completed so signify by leaving it blank
		bucketName = initialBucketName
	default:
		bucketName = _bucket.Name
	}

	globalBucket := ""
	// We will create a global ratelimiter when we attempt to use a discord API
	if IsDiscordAPIURI(URI) {
		pathHash := sha256.New()
		pathHash.Write(requestHeaders.Peek("Authorization"))
		globalBucket = hex.EncodeToString(pathHash.Sum(nil))
	}

	// Create bucket if it does not exist
	rt.bucketsMu.Lock()
	if _, ok := rt.Buckets[bucketName]; !ok {
		rt.Logger.Debug().Msgf("Creating bucket '%s'", bucketName)
		_bucket = bucket.CreateBucket(bucketName, 10, time.Second*10, "", globalBucket)
		rt.Buckets[bucketName] = _bucket
	}
	if _, ok := rt.Buckets[globalBucket]; !ok {
		rt.Logger.Debug().Msgf("Creating global bucket '%s'", globalBucket)
		_bucket = bucket.CreateBucket(globalBucket, 50, time.Second, "", "")
		rt.Buckets[globalBucket] = _bucket
	}
	rt.bucketsMu.Unlock()

	// Initialize queue if it does not exist
	rt.queuesMu.Lock()
	if _, ok := rt.Queues[bucketName]; !ok {
		rt.Logger.Debug().Msgf("Creating queue '%s'", bucketName)
		queue = &Queue{
			Expiration:     time.Now().UTC().Add(QueueExpiration),
			JobActive:      abool.NewBool(false),
			JobsHandled:    new(int64),
			Bucket:         _bucket,
			events:         new(int64),
			priority:       new(int64),
			PriorityEvents: make(chan *structs.TunnelRequest, 64),
			Events:         make(chan *structs.TunnelRequest, 64),
		}
		rt.Queues[bucketName] = queue
	} else {
		queue = rt.Queues[bucketName]
		queue.Expiration = time.Now().UTC().Add(QueueExpiration)
	}
	rt.queuesMu.Unlock()

	tunnelRequest := structs.TunnelRequest{
		ID:           id,
		Req:          ctx,
		URI:          URI.FullURI(),
		QueryString:  ctx.QueryArgs().QueryString(),
		ResponseType: responseType,
		Priority:     hasPriority,
		Bucket:       bucketName,
		Callback:     make(chan bool),
	}

	// If the queue we have added to has no jobs running, start a job
	if queue.JobActive.IsNotSet() {
		rt.Logger.Debug().Msgf("Creating job for queue '%s'", bucketName)
		go rt.StartQueueJob(queue)
	}

	queueEvent := func() {
		if hasPriority {
			queue.PriorityEvents <- &tunnelRequest
		} else {
			queue.Events <- &tunnelRequest
		}
	}

	// Add tunnelRequest to job queue
	if responseType == structs.NoResponse {
		go queueEvent()
		return
	}
	queueEvent()

	// RestTunnel-UUID: UUID of Request
	// RestTunnel-Ratelimit-Hit: True if a ratelimit occurred
	// RestTunnel-Ratelimit-Bucket: Name of the final bucket that was used
	// RestTunnel-Ratelimit-Buckets: Name of all traversed buckets (separated by ;) original;alias;alias;final

	ctx.Response.Header.Set("RT-Hit", "false")

	switch responseType {
	case structs.RespondWithResponse:
		// Wait for the callback channel to say the request has been completed then
		// retrieve from callbacks and remove.
		<-tunnelRequest.Callback

		rt.callbacksMu.Lock()
		tunnelResponse, ok := rt.Callbacks[tunnelRequest.ID]
		close(tunnelRequest.Callback)
		delete(rt.Callbacks, tunnelRequest.ID)
		rt.callbacksMu.Unlock()

		if !ok {
			rt.Logger.Warn().Msg("Received callback but not in callback table. Ignoring...")
		} else {
			ctx.Response.Header.Set("RT-Hit", strconv.FormatBool(tunnelResponse.RatelimitHit))

			ctx.Response.ResetBody()
			body := tunnelResponse.Response.Body()

			// Copy TunnelResponse to fasthttp.Response
			tunnelResponse.Response.Header.VisitAll(func(key []byte, value []byte) {
				ctx.Response.Header.SetBytesKV(key, value)
			})

			ctx.Response.Header.Set("RT-UUID", tunnelRequest.ID.String())
			ctx.Response.Header.Set("RT-Bucket", tunnelRequest.Bucket)
			ctx.Response.Header.Set("RT-Buckets", strings.Join(bucketStack, ";"))

			ctx.Write(body)
			ctx.SetStatusCode(tunnelResponse.Response.StatusCode())
		}
	case structs.RespondWithUUIDCallback:
		ctx.Response.Header.Set("RT-UUID", tunnelRequest.ID.String())
		ctx.Response.Header.Set("RT-Bucket", tunnelRequest.Bucket)
		ctx.Response.Header.Set("RT-Buckets", strings.Join(bucketStack, ";"))

		ctx.Redirect("/callbacks/"+tunnelRequest.ID.String(), 303)
	}
}

// TraverseBucket returns the original bucket from the bucket alias.
// Returns current bucket, slice of buckets traversed through and error.
func (rt *RestTunnel) TraverseBucket(bucketStr string) (_bucket *bucket.Bucket, bucketStack []string, err error) {
	rt.bucketsMu.RLock()
	defer rt.bucketsMu.RUnlock()

	stack := make(map[string]bool)
	for {
		bucketStack = append(bucketStack, bucketStr)
		if _, ok := stack[bucketStr]; ok {
			return nil, bucketStack, bucket.ErrBucketCircularAlias
		}
		stack[bucketStr] = true

		_bucket, ok := rt.Buckets[bucketStr]
		if !ok {
			return nil, bucketStack, bucket.ErrBucketDoesNotExist
		}

		if _bucket.Alias != "" && _bucket.Alias != _bucket.Name {
			bucketStr = _bucket.Alias
		} else {
			return _bucket, bucketStack, nil
		}
	}
}

// DecodeBody returns a decoded fasthttp.Response body using the
// Content-Encoding header
func (rt *RestTunnel) DecodeBody(resp *fasthttp.Response) (body []byte, err error) {
	contentEncoding := string(resp.Header.Peek("Content-Encoding"))
	body = resp.Body()

	if contentEncoding == "" {
		return body, nil
	}

	// Decode the body using the Content-Encoding header
	for _, encoding := range strings.Split(contentEncoding, ";") {
		switch encoding {
		case "gzip":
			body, err = resp.BodyGunzip()
			if err != nil {
				rt.Logger.Error().Err(err).Msg("Failed to Gunzip body")
			}
		case "deflate":
			body, err = resp.BodyInflate()
			if err != nil {
				rt.Logger.Error().Err(err).Msg("Failed to Inflate body")
			}
		case "br":
			body, err = resp.BodyUnbrotli()
			if err != nil {
				rt.Logger.Error().Err(err).Msg("Failed to Unbrotli body")
			}
		default:
			rt.Logger.Warn().Msgf("Unexpected encoding '%s'", encoding)
		}
	}
	return body, err
}

// Utilized Headers
// RestTunnel-URL: The url you are requesting
// RestTunnel-Method: the response type that is wanted (response/callback/none) (defaults to response)
// RestTunnel-Priority: boolean if the request is high priority
// Any other headers passed are sent to the request (except encoding however is still handled serverside normally so there is no difference)

// Response Headers
// RestTunnel-Timing-Request: unix of request received
// RestTunnel-Timing-Queued: unix of request added to queue/prioritized
// RestTunnel-Timing-Limited: unix of request waiting for ratelimit
// RestTunnel-Timing-Executed: unix of when request was actually executed
// RestTunnel-Timing-Completed: unix of when request was completed
// (This might be cut down into either a single int with ms to complete all or split into waiting for cache, ratelimit and execution split with ;)

// RestTunnel-UUID: UUID of Request
// RestTunnel-Ratelimit-Hit: True if a ratelimit occurred
// RestTunnel-Ratelimit-Bucket: Name of the final bucket that was used
// RestTunnel-Ratelimit-Buckets: Name of all traversed buckets (separated by ;) original;alias;alias;final

// The process of handling a request with RestTunnel is fairly basic and is split
// into multiple sections:
// - Buckets
// - Queues
// - Callbacks

// The order and how these 3 systems are handled are completely based upon the
// ResponseType that the client initiating the request has told RestTunnel. These are:
// -  RespondWithResponse
//  When passing this type, RestTunnel will wait until the request has finished
// 	before returning a response to the client.
// -  RespondWithUUIDCallback
// 	When passing this type, RestTunnel will return a UUID to the client regardless
// 	if the event has been processed yet.
// -  NoResponse
// 	If passed, RestTunnel will simply acknowledge the request and process it without
// 	disclosing its response or status to the client.

// On top of providing how the response is given to the client, the client can also
// provide if the request is high priority. By specifying true to RestTunnel-Priority,
// the execution of any other events are halted and the high priority request is
// executed beforehand. If high priority is not provided, the request will be added to
// the queue which performs as a deque.

// As this is the case, it is recommended you only use RespondwithResponse with
// priority requests as it is the case where it may have to wait for other requests
// to process first which could cause the connection to timeout.

// When priority is enabled, instead of adding the job to the Queue deque, it will
// just lock and add 1 to the Priority WaitGroup 1 which is removed when its done to
// signal the queue job to yield.

// When receiving a RespondWithResponse, it will execute a request then read from the
// TunnelRequest.Callback channel to signify that the request has been completed in
// which the TunnelRequest.Response is given to the client.

// When receiving a RespondWithUUIDCallback, it will return the TunnelRequest UUID
// to the client then execute the request separately. It is up to the client to poll
// /callback/uuid or subscribe in gateway to retrieve the response.

// The Queue has a dedicated task which will loop until there are no more requests
// in the queue in which it will stop execution. On every loop it will first wait
// for the priority event WaitGroup which will ensure any high priority events are
// queued first.

// +---+
// |   v
// |   Wait for Priority event WaitGroup
// |   Retries the length of the queue with an RLock then RUnlocks
// |   +- Queue length is 0 -> Stop job
// |   Lock the queue, Pop a request from the queue then Unlock the queue
// |   Execute the TunnelRequest job
// |   Add the response to the struct and then add to the Callbacks table
// |   Broadcast to the TunnelRequest.Callback channel to signal RespondWithResponse
// |   +- CallbackTTL job is not running -> Start job
// +---+

// The CallbackTTL job is a loop that runs every x seconds (by default 60) to remove
// old callbacks from the cache. It is expected that if a client expects a callback
// that it is requested within 2 minutes then it is deleted. This loop checks all
// entries and will delete any keys that have expired. It will also store the time
// of the smallest time in the future that has not expired and will wait until then
// or 60 seconds (whatever is smaller).

// +---+
// |   v
// |   Set expirationCounter to 60
// |   Iterate entries
// |   +---+
// |   |   v
// |   |   +- Expired -> Mark entry for deletion -> `continue`
// |   |   +- time until < expirationCounter -> Set expirationCounter to expiration
// |   +---+                                    of entry
// |   Delete all entries marked for deletion
// |   +- Callbacks length is 0 -> Stop job
// |   Sleep for expirationCounter
// +---+
