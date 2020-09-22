package resttunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/tevino/abool"
	"github.com/valyala/fasthttp"
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
const CallbackExpiration = time.Minute * 2

// ErrCallbackNotFinished is raised when attempting to retrieve the callback on a request that is not done
var ErrCallbackNotFinished = xerrors.New("Callback '%s' has not completed request")

// ErrCallbackDoesNotExist is raised when referencing a callback that does not exist
var ErrCallbackDoesNotExist = xerrors.New("Callback '%s' does not exist")

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
	Buckets   map[string]*Bucket `json:"buckets"`

	queuesMu sync.RWMutex
	Queues   map[string]*Queue `json:"queue"`

	callbacksMu sync.RWMutex
	Callbacks   map[uuid.UUID]*TunnelResponse `json:"callbacks"`

	// Analytics for ratelimits
	AnalyticsHit  *Accumulator
	AnalyticsMiss *Accumulator

	// Requests waiting and an atomic cache
	AnalyticsWaiting *Accumulator
	analyticsWaiting *int64

	// Analytics for requests and callbacks buffer
	AnalyticsRequests  *Accumulator
	AnalyticsCallbacks *Accumulator

	// Uses total response time and requests to calculate average
	AnalyticsAverageResponse *Accumulator
	analyticsResponseTotal   *int64
	analyticsRequests        *int64
}

// Queue represents a Deque with a priority queue
type Queue struct {
	// We use a channel to painlessly control how many Jobs should be active. If we have 4
	// jobs active and only want 2 running, we send 2 booleans down JobClosure and if we
	// wanted to add 1 more we would just create a Queue job normally. Reference
	// TotalJobsActive for how many are active and use JobsActive to wait when closing
	// a queue. BalancerActive signifys if the balancer is enabled and limits to only one
	// being ran at a time. The balancer simply creates more queue jobs when necessary.
	BalancerActive *abool.AtomicBool

	TotalJobsActive *int64
	JobsActive      *sync.WaitGroup
	JobClosure      chan bool

	JobsHandled *int64

	Bucket *Bucket

	events   *int64
	priority *int64

	PriorityEvents chan *TunnelRequest
	Events         chan *TunnelRequest
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
		Buckets:   make(map[string]*Bucket),

		queuesMu: sync.RWMutex{},
		Queues:   make(map[string]*Queue),

		callbacksMu: sync.RWMutex{},
		Callbacks:   make(map[uuid.UUID]*TunnelResponse),

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

	rt.AnalyticsHit = NewAccumulator(rt.ctx, Samples, Interval)
	rt.AnalyticsHit.Label = "Ratelimit Hits"
	rt.AnalyticsMiss = NewAccumulator(rt.ctx, Samples, Interval)
	rt.AnalyticsMiss.Label = "Ratelimit Misses"
	rt.AnalyticsRequests = NewAccumulator(rt.ctx, Samples, Interval)
	rt.AnalyticsRequests.Label = "Total Requests"
	rt.AnalyticsCallbacks = NewAccumulator(rt.ctx, Samples, Interval)
	rt.AnalyticsCallbacks.Label = "Callbacks in buffer"
	rt.AnalyticsAverageResponse = NewAccumulator(rt.ctx, Samples, Interval)
	rt.AnalyticsAverageResponse.Label = "Average response time"
	rt.AnalyticsWaiting = NewAccumulator(rt.ctx, Samples, Interval)
	rt.AnalyticsWaiting.Label = "Waiting Requests"

	go rt.LazyCallbackJob()
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

	err = fasthttp.ListenAndServe(rt.Configuration.Host, rt.HandleRequest)
	fmt.Printf("Serving on %s (Press CTRL+C to quit)", rt.Configuration.Host)

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

// HandleQueueJob handles a TunnelRequest gathered from a queue
func (rt *RestTunnel) HandleQueueJob(tr *TunnelRequest) {
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
	rt.callbacksMu.Lock()
	rt.Callbacks[tr.ID] = &TunnelResponse{
		expiration: time.Now().UTC().Add(time.Minute * 5),
		Complete:   false,
		ID:         tr.ID,
	}
	rt.callbacksMu.Unlock()

	stage = "aqrq"
	req := fasthttp.AcquireRequest()
	req.SetRequestURIBytes(tr.URI)

	stage = "hdrs"
	tr.Headers.VisitAll(func(key []byte, value []byte) {
		req.Header.SetBytesKV(key, value)
	})

	// Asks discord to send higher precision reset times
	req.Header.Set("X-RateLimit-Precision", "millisecond")

	var resp *fasthttp.Response
	var hit bool
	var err error

	rt.bucketsMu.RLock()
	bucket := rt.Buckets[tr.Bucket]
	var globalBucket *Bucket
	if bucket.global != "" {
		globalBucket = rt.Buckets[bucket.global]
	}
	rt.bucketsMu.RUnlock()

	for {
		stage = "bckt"

		hit = bucket.Lock()
		if bucket.global != "" {
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
				if bucket.global != "" {
					retryAfter, err := strconv.Atoi(string(resp.Header.Peek("Retry-After")))
					if err != nil {
						// Convert ms to ns (x1e6)
						globalBucket.Exhaust(time.Now().UnixNano() + int64(retryAfter*1000000))
					}
				} else {
					rt.Logger.Warn().Str("bucket", bucket.name).Msg("Hit global ratelimit with global bucket set.")
				}
			} else {
				// We ignore X-RateLimit-Reset as we already have Reset-Afdter
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

				// Convert seconds to nanoseconds (x1e9) (we add on about 500ms just incase)
				rlReset := time.Now().UnixNano() + int64(rlResetRaw*float64(1000000000)) + 500000000
				bucket.Exhaust(rlReset)
				bucket.Modify(int32(rlLimit), atomic.LoadInt64(bucket.duration), rlBucket, bucket.global)
			}
		} else {
			rlBucket := string(resp.Header.Peek("X-RateLimit-Bucket"))
			// If we receive no X-RateLimit-Bucket it is likely not discord and we will just discard anyway
			if rlBucket != "" {
				rt.Logger.Debug().Msgf("Received bucket '%s'", rlBucket)
				if rlBucket != bucket.alias && rlBucket != "" {
					rt.Logger.Info().Msgf("Discovered alias bucket '%s' for '%s'", rlBucket, bucket.name)
					bucket.Modify(
						atomic.LoadInt32(bucket.limit),
						atomic.LoadInt64(bucket.duration),
						rlBucket,
						bucket.global,
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
					bucket.Modify(int32(rlLimit), time.Duration(rlReset*1000000000).Nanoseconds(), rlBucket, bucket.global)
				}
			}
			break
		}
	}

	tunnelResponse := &TunnelResponse{
		expiration: time.Now().UTC().Add(CallbackExpiration),

		Complete:     true,
		ID:           tr.ID,
		RatelimitHit: hit,
		Response:     resp,
		Error:        err,
	}

	stage = "clbk"
	rt.callbacksMu.Lock()
	rt.Callbacks[tr.ID] = tunnelResponse
	rt.callbacksMu.Unlock()

	stage = "chan"
	if tr.ResponseType == RepondWithResponse {
		tr.Callback <- true
	}
	stage = "done"
}

// SetQueueCount changes how many queue jobs are running
func (rt *RestTunnel) SetQueueCount(q *Queue, count int64) {
	change := count - atomic.LoadInt64(q.TotalJobsActive)

	if change == 0 {
		return
	}

	if change > 0 {
		// Create more jobs
		for i := int64(0); i < change; i++ {
			go rt.StartQueueJob(q)
		}
	} else {
		// Send JobClosure events to stop a single running task
		for i := int64(0); i < change; i++ {
			q.JobClosure <- true
		}
	}
}

// StartQueueJob starts a queue job
func (rt *RestTunnel) StartQueueJob(q *Queue) {
	atomic.AddInt64(q.TotalJobsActive, 1)
	q.JobsActive.Add(1)
	defer func() {
		q.JobsActive.Done()
		atomic.AddInt64(q.TotalJobsActive, -1)
	}()

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
		case <-q.JobClosure:
			return
		}
	}
}

// BalanceQueueJobs is the task that ensures that the queue is able to keep up.
// This might not even be used as a QueueJob is made for each endpoint so its
// unlikely a single endpoint will get overwhelmed.
// TODO: Do something with this
func (rt *RestTunnel) BalanceQueueJobs(q *Queue) {
	// Duration how long we should account for
	preserveTime := time.Minute
	// How often we should check
	interval := 30 * time.Second

	// Minimum a job can do on its own. If we try launch 10 jobs when we only
	// process 40 requests, it will limit to only 2 because the minEventsPerJob
	// is 30. Increase to make it less likely jobs will be removed.
	minEventsPerJob := 30

	events := int64(0)

	// Used to extrapolate how many jobs should be running
	timeScale := float64(preserveTime) / float64(interval)

	// Initial
	rt.SetQueueCount(q, 1)

	t := time.NewTicker(interval)
	for {
		<-t.C

		jobsSince := atomic.LoadInt64(q.JobsHandled)
		change := jobsSince - events

		minuteHandle := float64(change) * timeScale
		waitingOn := float64(len(q.PriorityEvents) + len(q.Events))
		jobScale := (minuteHandle + waitingOn) / minuteHandle
		jobsNeeded := math.Floor(float64(atomic.LoadInt64(q.TotalJobsActive)) * jobScale)

		actualJobsNeeded := int64(math.Min(jobsNeeded, math.Floor(minuteHandle/float64(minEventsPerJob))))

		// println("Handling", minuteHandle)
		// println("Waiting On", waitingOn)
		// println("Expected jobs needed", jobsNeeded, "scale of", jobScale)
		// println("Minimum", math.Floor(minuteHandle/float64(minEventsPerJob)))
		// println("Chose", actualJobsNeeded)

		rt.SetQueueCount(q, actualJobsNeeded)
	}
}

// createLineChart creates a LineChart from an accumulator
func createLineChart(ac *Accumulator, background string, border string) (chart LineChart) {
	data := make([]interface{}, 0, len(ac.Samples))
	for _, sample := range ac.Samples {
		data = append(data, DataStamp{sample.StoredAt, sample.Value})
	}
	chart = LineChart{
		Datasets: []Dataset{{
			Label:            ac.Label,
			BackgroundColour: background,
			BorderColour:     border,
			Data:             data,
		}},
	}
	return chart
}

// HandleRequest handles a HTTP request given to RestTunnel
func (rt *RestTunnel) HandleRequest(ctx *fasthttp.RequestCtx) {
	startTime := time.Now().UTC()
	var path string

	defer func() {
		ms := time.Now().UTC().Sub(startTime).Milliseconds()
		// Only log if they have not requested / and only
		// count response time if they have requested /
		if path != "/" {
			rt.Logger.Info().Msgf("%s %s %s %d %dms",
				ctx.RemoteAddr(),
				ctx.Request.Header.Method(),
				path,
				ctx.Response.StatusCode(),
				ms)
		} else {
			atomic.AddInt64(rt.analyticsResponseTotal, ms)
			atomic.AddInt64(rt.analyticsRequests, 1)
		}
	}()

	path = string(ctx.Request.URI().Path())
	switch path {
	case "/":
		id := uuid.New()

		hasPriority, err := strconv.ParseBool(string(ctx.Request.Header.Peek("RT-Priority")))
		if err != nil {
			hasPriority = false
		}
		responseType, _ := ParseResponse(string(ctx.Request.Header.Peek("RT-ResponseType")))

		requestURI := ctx.Request.Header.Peek("RT-URL")
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

		// We then traverse the bucket incase it does not exist or it has an alias as some paths may use the same bucket
		bucket, bucketStack, err := rt.TraverseBucket(initialBucketName)

		if err != nil {
			rt.Logger.Debug().Err(err).Msgf("Bucket '%s' does not exist yet", initialBucketName)
		} else {
			rt.Logger.Debug().Str("bucket", bucket.name).Msgf("Traversed bucket: %v", bucketStack)
		}

		var bucketName string
		var queue *Queue

		switch err {
		case ErrBucketCircularAlias:
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
		case ErrBucketDoesNotExist:
			// Bucket will be created when request is completed so signify by leaving it blank
			bucketName = initialBucketName
		default:
			bucketName = bucket.name
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
			bucket = CreateBucket(bucketName, 10, time.Second*10, "", globalBucket)
			rt.Buckets[bucketName] = bucket
		}
		if _, ok := rt.Buckets[globalBucket]; !ok {
			rt.Logger.Debug().Msgf("Creating global bucket '%s'", globalBucket)
			bucket = CreateBucket(globalBucket, 50, time.Second, "", "")
			rt.Buckets[globalBucket] = bucket
		}
		rt.bucketsMu.Unlock()

		// Initialize queue if it does not exist
		rt.queuesMu.Lock()
		if _, ok := rt.Queues[bucketName]; !ok {
			rt.Logger.Debug().Msgf("Creating queue '%s'", bucketName)
			queue = &Queue{
				BalancerActive:  abool.New(),
				TotalJobsActive: new(int64),
				JobsActive:      &sync.WaitGroup{},
				JobClosure:      make(chan bool),
				JobsHandled:     new(int64),
				Bucket:          bucket,
				events:          new(int64),
				priority:        new(int64),
				PriorityEvents:  make(chan *TunnelRequest, 64),
				Events:          make(chan *TunnelRequest, 64),
			}
			rt.Queues[bucketName] = queue
		} else {
			queue = rt.Queues[bucketName]
		}
		rt.queuesMu.Unlock()

		tunnelRequest := TunnelRequest{
			ID: id,

			URI:     URI.FullURI(),
			Headers: &requestHeaders,

			ResponseType: responseType,
			Priority:     hasPriority,

			Bucket:   bucketName,
			Callback: make(chan bool),
		}

		// If the queue we have added to has no jobs running, start a job
		if totalJobs := atomic.LoadInt64(queue.TotalJobsActive); totalJobs == 0 {
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

		if responseType == NoResponse {
			go queueEvent()
			return
		}

		queueEvent()

		// RestTunnel-UUID: UUID of Request
		// RestTunnel-Ratelimit-Hit: True if a ratelimit occured
		// RestTunnel-Ratelimit-Bucket: Name of the final bucket that was used
		// RestTunnel-Ratelimit-Buckets: Name of all traversed buckets (seperated by ;) origional;alias;alias;final

		ctx.Response.Header.Set("RT-Hit", "false")

		switch responseType {
		case RepondWithResponse:
			// Wait for the callback channel to say the request has been completed then
			// retrieve from callbacks and remove.
			<-tunnelRequest.Callback

			rt.callbacksMu.RLock()
			tunnelResponse, ok := rt.Callbacks[tunnelRequest.ID]
			close(tunnelRequest.Callback)
			delete(rt.Callbacks, tunnelRequest.ID)
			rt.callbacksMu.RUnlock()

			if !ok {
				rt.Logger.Warn().Msg("Received callback but not in callback table. Ignoring...")
			} else {
				ctx.Response.Header.Set("RT-Hit", strconv.FormatBool(tunnelResponse.RatelimitHit))

				// Copy TunnelResponse to fasthttp.Response
				tunnelResponse.Response.Header.VisitAll(func(key []byte, value []byte) {
					ctx.Response.Header.SetBytesKV(key, value)
				})

				ctx.Response.ResetBody()
				body, _ := rt.DecodeBody(tunnelResponse.Response)
				ctx.Response.Header.Set("Content-Encoding", "")

				ctx.Write(body)
				ctx.SetStatusCode(tunnelResponse.Response.StatusCode())
			}
		case RespondWithUUIDCallback:
			// We do not need to do anything as it is automatically added to the callbacks.
			// Return redirect to callback page. We will wait 250ms for the request to possibly
			// finish else it will just immediately send a redirect which will never be completed
			// immediately.
			time.Sleep(time.Millisecond * 250)
			ctx.Redirect("/callbacks/"+tunnelRequest.ID.String(), 303)
		}

		ctx.Response.Header.Set("RT-UUID", tunnelRequest.ID.String())
		ctx.Response.Header.Set("RT-Bucket", tunnelRequest.Bucket)
		ctx.Response.Header.Set("RT-Buckets", strings.Join(bucketStack, ";"))
	default:
		if strings.HasPrefix(path, "/alive") {
			res, err := json.Marshal(AliveResponse{VERSION})
			if err == nil {
				ctx.Write(res)
			}
			return
		}
		if strings.HasPrefix(path, "/api/analytics") {
			// rt.AnalyticsHit.RunOnce(now)
			// rt.AnalyticsMiss.RunOnce(now)
			// rt.AnalyticsRequests.RunOnce(now)
			// rt.AnalyticsCallbacks.RunOnce(now)
			// rt.AnalyticsAverageResponse.RunOnce(now)

			now := time.Now()
			ar := AnalyticResponse{
				Success: true,
				Error:   "",
				Uptime:  DurationTimestamp(now.Sub(rt.Start)),
			}

			background, border := "rgba(149, 165, 165, 0.5)", "#7E8C8D"
			ar.Charts.Hits = createLineChart(rt.AnalyticsHit, background, border)
			ar.Charts.Misses = createLineChart(rt.AnalyticsMiss, background, border)
			ar.Charts.Waiting = createLineChart(rt.AnalyticsWaiting, background, border)
			ar.Charts.Requests = createLineChart(rt.AnalyticsRequests, background, border)
			ar.Charts.Callbacks = createLineChart(rt.AnalyticsCallbacks, background, border)
			ar.Charts.AverageResponse = createLineChart(rt.AnalyticsAverageResponse, background, border)

			ar.Numbers.Hits = rt.AnalyticsHit.GetAllSamples().Sum()
			ar.Numbers.Misses = rt.AnalyticsMiss.GetAllSamples().Sum()
			ar.Numbers.Waiting = rt.AnalyticsWaiting.GetAllSamples().Sum()
			ar.Numbers.Requests = rt.AnalyticsRequests.GetAllSamples().Sum()

			res, err := json.Marshal(ar)
			if err == nil {
				ctx.Write(res)
			} else {
				res, err := json.Marshal(ErrorResponse{
					Error:   err.Error(),
					Success: false,
				})
				if err == nil {
					ctx.Write(res)
				}
			}
			return
		}
		if strings.HasPrefix(path, "/callbacks") {
			id, err := uuid.ParseBytes(ctx.Request.URI().LastPathSegment())
			if err != nil {
				ctx.SetStatusCode(400)
				rterr := err.Error()
				ctx.Response.Header.Set("Rt-URL", rterr)
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

			rt.callbacksMu.RLock()
			tunnelResponse, ok := rt.Callbacks[id]
			delete(rt.Callbacks, id)
			rt.callbacksMu.RUnlock()

			if !ok {
				ctx.SetStatusCode(410)
				rterr := xerrors.Errorf(ErrCallbackDoesNotExist.Error(), id).Error()
				ctx.Response.Header.Set("Rt-URL", rterr)
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
			if !tunnelResponse.Complete {
				ctx.SetStatusCode(301)
				ctx.Response.Header.Set("Retry-After", "5") // Tell client to retry in 5 seconds
				rterr := xerrors.Errorf(ErrCallbackNotFinished.Error(), id).Error()
				ctx.Response.Header.Set("Rt-URL", rterr)
				res, err := json.Marshal(ErrorResponse{
					Error:   rterr,
					Success: false,
					Queued:  true,
					UUID:    id,
				})
				if err == nil {
					ctx.Write(res)
				}
				return
			}

			ctx.Response.Header.Set("RT-Hit", strconv.FormatBool(tunnelResponse.RatelimitHit))
			ctx.Response.Header.Set("RT-UUID", tunnelResponse.ID.String())

			// Copy TunnelResponse to fasthttp.Response
			tunnelResponse.Response.Header.VisitAll(func(key []byte, value []byte) {
				ctx.Response.Header.SetBytesKV(key, value)
			})

			ctx.Response.ResetBody()
			body, _ := rt.DecodeBody(tunnelResponse.Response)
			ctx.Response.Header.Set("Content-Encoding", "")

			ctx.Write(body)
			ctx.SetStatusCode(tunnelResponse.Response.StatusCode())
			return
		}
	}

	return
}

// TraverseBucket returns the origional bucket from the bucket alias.
// Returns current bucket, slice of buckets traversed through and error.
func (rt *RestTunnel) TraverseBucket(bucketStr string) (bucket *Bucket, bucketStack []string, err error) {
	rt.bucketsMu.RLock()
	defer rt.bucketsMu.RUnlock()

	stack := make(map[string]bool)
	for {
		bucketStack = append(bucketStack, bucketStr)
		if _, ok := stack[bucketStr]; ok {
			return nil, bucketStack, ErrBucketCircularAlias
		}
		stack[bucketStr] = true

		bucket, ok := rt.Buckets[bucketStr]
		if !ok {
			return nil, bucketStack, ErrBucketDoesNotExist
		}

		if bucket.alias != "" && bucket.alias != bucket.name {
			bucketStr = bucket.alias
		} else {
			return bucket, bucketStack, nil
		}
	}
}

// DecodeBody returns a decoded fasthttp.Responce body using the
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

// func MapRandomKeyGet2(mapI map[string]int) (string, int) {
// 	i := rand.Intn(len(mapI))
// 	for k, v := range mapI {
// 		if i == 0 {
// 			return k, v
// 		}
// 		i--
// 	}
// 	panic("error")
// }

// randomCallback returns a random callback from callbacks
func (rt *RestTunnel) randomCallback() (k uuid.UUID, v *TunnelResponse) {
	rt.callbacksMu.RLock()
	defer rt.callbacksMu.RUnlock()

	if len(rt.Callbacks) < 1 {
		return
	}

	i := rand.Intn(len(rt.Callbacks))
	for k, v = range rt.Callbacks {
		if i == 0 {
			return k, v
		}
		i--
	}
	return
}

// LazyCallbackJob handles clearing old entries in callback
func (rt *RestTunnel) LazyCallbackJob() {
	interval := time.Second * 5
	t := time.NewTicker(interval)
	for {
		<-t.C
		now := time.Now().UTC()
		deletions := []uuid.UUID{}
		for {
			id, tunnelResponse := rt.randomCallback()
			if tunnelResponse != nil {
				if tunnelResponse.expiration.Sub(now) <= 0 {
					deletions = append(deletions, id)
					continue
				}
			}
			break
		}
		rt.callbacksMu.Lock()
		for _, uuid := range deletions {
			delete(rt.Callbacks, uuid)
		}
		rt.callbacksMu.Unlock()
	}
}

// Utilised Headers
// RestTunnel-URL: The url you are requesting
// RestTunnel-Method: the response type that is wanted (response/callback/none) (defaults to response)
// RestTunnel-Priority: boolean if the request is high priority
// Any other headers passed are sent to the request (except encoding however is still handled serverside normally so there is no difference)

// Response Headers
// RestTunnel-Timing-Request: unix of request received
// RestTunnel-Timing-Queued: unix of request added to queue/prioritised
// RestTunnel-Timing-Limited: unix of request waiting for ratelimit
// RestTunnel-Timing-Executed: unix of when request was actually executed
// RestTunnel-Timing-Completed: unix of when request was completed
// (This might be cut down into either a single int with ms to complete all or split into waiting for cache, ratelimit and execution split with ;)

// RestTunnel-UUID: UUID of Request
// RestTunnel-Ratelimit-Hit: True if a ratelimit occured
// RestTunnel-Ratelimit-Bucket: Name of the final bucket that was used
// RestTunnel-Ratelimit-Buckets: Name of all traversed buckets (seperated by ;) origional;alias;alias;final

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
// 	When passing this type, RestTunnel will return a UUID to the client reguardless
// 	if the event has been processed yet.
// -  NoResponse
// 	If passed, RestTunnel will simply acknowledge the request and process it without
// 	disclosing its response or status to the client.

// Ontop of providing how the response is given to the client, the client can also
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
// to the client then execute the request seperately. It is up to the client to poll
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
