// Package probe offers an implementation for an [http.Handler] that's compatible with Kubernetes'
// liveness, health and startup probe specifications, along with an HTTP client for it.
//
// [http.Handler]: https://pkg.go.dev/net/http#Handler
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/azazeal/singleflight"
)

// New initializes and returns a new [*Handler], for the provided particulars.
func New(options ...Option) *Handler {
	h := &Handler{}
	for _, opt := range options {
		opt.apply(h)
	}
	return h
}

// Option wraps the set of configuration options that [New] accepts.
type Option interface {
	apply(*Handler)
}

// WithTimeout returns an [Option] specifying that when the [Handler] serves an [http.Request],
// carrying a [context.Context] without a deadline, it should use the provided [time.Duration]
// to set a new deadline, in case the provided [time.Duration] is positive.
func WithTimeout(t time.Duration) Option {
	return optionFunc(func(h *Handler) { h.timeout = t })
}

type optionFunc func(*Handler)

func (fn optionFunc) apply(h *Handler) { fn(h) }

// Handler implements an [http.Handler] that's compatible with Kubernetes' startup, readiness and
// liveness endpoints. It determines the HTTP status code of the response it should render, based
// on whether any of the probes registered to it report an error (see: [handler.ServeHTTP]).
//
// Probes, i.e. implementations of [P], are callbacks mapping a [context.Context] to an error. They are registered
// to—and deregistered from—handlers, in the course of program execution, via the [Handler.Register] function.
//
// Handlers, when queried via an [http.MethodGet] [http.Request], also render a JSON document mapping each of their
// registered probes to the time at which the probe was registered and the error message it reported (if any).
//
// The default value of Handler is a valid one.
type Handler struct {
	_ [0]func() // not compareable

	timeout time.Duration

	mu         sync.RWMutex // protects probes
	components map[string]*component

	// compresses calls to response
	caller singleflight.Caller[struct{}, map[string]*Status]
}

// Register registers the named [P] to the [Handler] and returns a function that reverses the
// registration.
//
// The returned callback is safe for use by concurrent callers, but any calls to it after the first
// will be a noop. Callers should always call the returned callback when the provided [P] should no
// longer be considered when determining the overall probe status of the [Handler].
//
// Register is safe for use by concurrent callers.
func (h *Handler) Register(name string, p P) func() {
	h.mu.Lock()
	defer h.mu.Unlock()

	// map the name to a unique identifier
	var count uint64
	var salt string
	for {
		id := name + salt
		if _, exists := h.components[id]; !exists {
			name = id

			break
		}
		count++
		salt = "#" + strconv.FormatUint(count, 10)
	}

	if h.components == nil {
		h.components = make(map[string]*component)
	}
	comp := &component{
		timestamp: time.Now(),
		probe:     p,
	}
	h.components[name] = comp

	return h.newDeregister(name, comp)
}

func (h *Handler) newDeregister(id string, comp *component) func() {
	var once sync.Once

	return func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()

			if h.components[id] == comp {
				// NOTE(@azazeal): names end up being IDs, by having a salt appended to them.
				//
				// This means that, as components are registered and deregistered, two different components may
				// end up being identified by the same ID.
				//
				// In turn, this means that if a caller uses the deregistration function of a component that's
				// already been deregistered, after another component of the same ID is registered, they'll up,
				// incorrectly, deregistering the component added later.
				delete(h.components, id)
			}
		})
	}
}

// ServeHTTP implements [http.Handler] for [Handler]. It responds with:
//
//   - [http.StatusMethodNotAllowed]: when the provided [*http.Request]'s [http.Request.Method] is neither
//     [http.MethodGet] nor [http.MethodHead].
//   - [http.StatusServiceUnavailable]: when the provided [*http.Request]'s [http.Request.Method] is either
//     [http.MethodGet] or [http.MethodHead] and either no [P] is registered to it or any of the [P]s that are report an
//     error.
//   - [http.StatusNoContent]: when the provided [*http.Request]'s [http.Request.Method] is [http.MethodHead] and none
//     of the [P]s registered to it report an error.
//   - [http.StatusOK]: when the provided [*http.Request]'s [http.Request.Method] is [http.Get] and none
//     of the [P]s registered to it report an error.
//
// Whenever the provided [*http.Request]'s [http.Request.Method] is [http.MethodGet], the [Handler] also
// marshals a JSON document, in the provided [http.ResponseWriter], mapping each registered [P]'s ID to its respective
// [Status].
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	default:
		http.Error(w, methodNotAllowedBody, http.StatusMethodNotAllowed)

		return
	case http.MethodHead, http.MethodGet:
		break // method allowed
	}

	ctx := r.Context()
	if _, ok := ctx.Deadline(); !ok && h.timeout > 0 {
		// since the context carries no deadline, add one
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.timeout)
		defer cancel()
	}
	statuses, _ := h.caller.Call(ctx, struct{}{}, h.statuses)

	ok := len(statuses) > 0
	for _, s := range statuses {
		if ok = ok && s.Error == nil; !ok {
			break
		}
	}

	if r.Method == http.MethodHead {
		if ok {
			w.WriteHeader(http.StatusNoContent)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		return
	}

	buf := getBuffer()
	defer putBuffer(buf)

	enc := json.NewEncoder(buf)
	enc.SetIndent("", "\t")
	if err := enc.Encode(statuses); err != nil {
		http.Error(w, internalServerErrorBody, http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	if ok {
		w.WriteHeader(http.StatusOK)
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_, _ = buf.WriteTo(w)
}

var (
	internalServerErrorBody = http.StatusText(http.StatusInternalServerError)
	methodNotAllowedBody    = http.StatusText(http.StatusMethodNotAllowed)

	bufPool = sync.Pool{
		New: func() any {
			b := new(bytes.Buffer)
			b.Grow(512)
			return b
		},
	}
)

func getBuffer() (b *bytes.Buffer) {
	return bufPool.Get().(*bytes.Buffer)
}

func putBuffer(b *bytes.Buffer) {
	b.Reset()

	bufPool.Put(b)
}

func (h *Handler) statuses(ctx context.Context) (map[string]*Status, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if len(h.components) == 0 {
		return emptyStatuses, nil
	}

	var mu sync.Mutex
	statuses := make(map[string]*Status, len(h.components))

	var wg sync.WaitGroup
	wg.Add(len(h.components))
	for id, c := range h.components {
		go func() {
			defer wg.Done()

			e := c.probe.query(ctx)

			mu.Lock()
			statuses[id] = &Status{
				RegisteredAt: c.timestamp,
				Error:        errorToMessage(e),
			}
			mu.Unlock()
		}()
	}

	wg.Wait()

	return statuses, nil
}

func errorToMessage(err error) *string {
	if err == nil {
		return nil
	}

	var sb strings.Builder
	_, _ = io.WriteString(&sb, err.Error())
	s := sb.String()

	return &s
}

var emptyStatuses = map[string]*Status{}

// P wraps the set of probes, i.e. mappers of [context.Context] to [error]. Implementations of P
// should their results within the validity of the provided [context.Context].
type P func(context.Context) error

// query attempts to fetch the probe's status, while the provided [context.Context]'s is not
// invalidated.
func (p P) query(ctx context.Context) error {
	if p == nil {
		return errUndefinedProbe
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	c := make(chan error, 1)
	go func() {
		c <- p(ctx)
		close(c)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-c:
		return err
	}
}

var errUndefinedProbe = errors.New("undefined probe")

// component wraps the set of component registrations.
type component struct {
	timestamp time.Time
	probe     P
}

// Status wraps the set of component statuses.
type Status struct {
	RegisteredAt time.Time `json:"registered_at"`
	Error        *string   `json:"error"`
}

// FromAtomicBool maps the provided [atomic.Bool] to its [P] equivalent. The returned [P] will
// return a non-nil error when called and the [atomic.Bool] is unset, i.e. its stored value is
// false, and a nil one in the alternative.
func FromAtomicBool(b *atomic.Bool) P {
	return func(context.Context) error {
		if !b.Load() {
			return errFail
		}
		return nil
	}
}

var errFail = errors.New("probe: fail")

// Head returns the status that an instance of [Handler] serving at the provided URL reports, via
// an HTTP HEAD request dispatched by the provided [http.Client] reference and [context.Context].
//
// The provided [http.Client] reference may be nil, in which case [http.DefaultClient] will be
// used in its place.
func Head(ctx context.Context, client *http.Client, rawurl string) (bool, error) {
	req, err := newRequest(ctx, http.MethodHead, rawurl)
	if err != nil {
		return false, err
	}

	res, err := determineClient(client).Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	default:
		return false, &UnexpectedStatusCodeError{Code: res.StatusCode}
	case http.StatusServiceUnavailable:
		return false, nil
	case http.StatusNoContent:
		return true, nil
	}
}

// UnexpectedStatusCodeError wraps the set of errors reported by [Get] & [Head] when the remote
// server responds with an expected status code.
type UnexpectedStatusCodeError struct {
	Code int // Code denotes the status code the remote server responded with.
}

func (e *UnexpectedStatusCodeError) Error() string {
	return fmt.Sprintf("probe: server responded with an unexpected status code (%d)", e)
}

// Get returns the overall and individual statuses of the components that make up the instance of
// [Handler] serving at the provided URL, via an HTTP GET request dispatched by the provided
// [http.Client] reference with the provided [context.Context].
//
// The provided [http.Client] reference may be nil, in which case [http.DefaultClient] will be
// used in its place.
func Get(ctx context.Context, client *http.Client, rawurl string) (map[string]Status, bool, error) {
	req, err := newRequest(ctx, http.MethodGet, rawurl)
	if err != nil {
		return nil, false, err
	}

	res, err := determineClient(client).Do(req)
	if err != nil {
		return nil, false, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	default:
		return nil, false, &UnexpectedStatusCodeError{Code: res.StatusCode}
	case http.StatusOK, http.StatusServiceUnavailable:
		break
	}

	dec := json.NewDecoder(res.Body)

	var s map[string]Status
	if err := dec.Decode(&s); err != nil {
		return nil, false, err
	}
	return s, res.StatusCode == http.StatusOK, nil
}

func newRequest(ctx context.Context, method, rawurl string) (req *http.Request, err error) {
	if req, err = http.NewRequestWithContext(ctx, method, rawurl, http.NoBody); err == nil {
		req.Header.Set("User-Agent", "prober/1.0.0")
	}
	return
}

func determineClient(client *http.Client) *http.Client {
	if client == nil {
		return http.DefaultClient
	}

	return client
}
