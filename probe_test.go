package probe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	fast = 100 * time.Millisecond
	slow = fast << 1
)

var (
	canceled  context.Context // a canceled context; set in init
	deadlined context.Context // a deadlined context; set in init
)

func init() {
	var cancel context.CancelFunc
	canceled, cancel = context.WithCancel(context.Background())
	cancel()
	if e := canceled.Err(); !errors.Is(e, context.Canceled) {
		// NOTE(@azazeal): sanity check
		panic(fmt.Errorf("invalid canceled error (%T): %w", e, e))
	}

	deadlined, cancel = context.WithTimeout(context.Background(), 0)
	cancel()
	if e := deadlined.Err(); !errors.Is(e, context.DeadlineExceeded) {
		// NOTE(@azazeal): sanity check
		panic(fmt.Errorf("invalid deadlined error (%T): %w", e, e))
	}
}

func TestHandlerRegister(t *testing.T) {
	var (
		ids        = []string{"", "#1", "1", "2", "2#1"}
		deregister = map[string]func(){}

		h Handler
	)

	if !t.Run("Register", func(t *testing.T) {
		startedAt := time.Now()

		for _, id := range ids {
			name, _, _ := strings.Cut(id, "#")

			deregister[id] = h.Register(name, func(context.Context) error { return errors.New(id) })
		}

		endedAt := time.Now()

		require.Len(t, h.components, len(ids))

		comps := map[string]*component{}
		for _, id := range ids {
			c := h.fetchComponentByID(id)
			require.NotNil(t, c, "id: %q", id)

			comps[id] = h.fetchComponentByID(id)
		}

		// we want each component to have been registered at an appropriate time and have the appropriate name
		for id, comp := range comps {
			assert.GreaterOrEqual(t, comp.timestamp, startedAt, "id: %q", id)
			assert.LessOrEqual(t, comp.timestamp, endedAt, "id: %q", id)

			haveID := comp.probe(context.Background()).Error()
			assert.Equal(t, id, haveID, "id: %q", id)
		}
	}) {
		return
	}

	if !t.Run("Deregister", func(t *testing.T) {
		// we also want deregister to remove the component from the handler
		for id, fn := range deregister {
			fn()

			assert.Nil(t, h.fetchComponentByID(id), "id: %q", id)
		}

		// and after all components have been deregistered, to be no components left
		assert.Len(t, h.components, 0)
	}) {
		return
	}
}

func TestDeregistrationDuplicates(t *testing.T) {
	// NOTE(@azazeal): See note in (*Handler).newDeregister

	var h Handler

	defer h.Register("comp", func(context.Context) error { return errors.New("") })()

	// register and deregister a component
	d1 := h.Register("comp", nil)
	d1()
	require.Nil(t, h.fetchComponentByID("comp#1"))

	// now register another component with the same name so they end up getting the same ID
	defer h.Register("comp", nil)()
	c2 := h.fetchComponentByID("comp#1")
	require.NotNil(t, c2)

	d1()                                               // deregister the _first_ component
	assert.Same(t, c2, h.fetchComponentByID("comp#1")) // and ensure the second component is still there
}

func TestHandlerServeHTTPMethodNotAllowed(t *testing.T) {
	t.Parallel()

	_, c, rawurl := newServer(t)

	for _, method := range []string{
		http.MethodConnect,
		http.MethodDelete,
		http.MethodOptions,
		http.MethodPatch,
		http.MethodPost,
		http.MethodPut,
		http.MethodTrace,
	} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()

			req, err := http.NewRequestWithContext(context.Background(), method, rawurl, http.NoBody)
			require.NoError(t, err)

			res, err := c.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()

			body, err := io.ReadAll(res.Body)
			require.NoError(t, err)

			_ = assert.Equal(t, http.StatusMethodNotAllowed, res.StatusCode) &&
				assert.Equal(t, http.StatusText(http.StatusMethodNotAllowed)+"\n", string(body))
		})
	}
}

func TestHandlerServeHTTPWhenEmpty(t *testing.T) {
	t.Parallel()

	_, c, rawurl := newServer(t)

	t.Run(http.MethodHead, func(t *testing.T) {
		ok, err := Head(context.Background(), c, rawurl)

		_ = assert.NoError(t, err) &&
			assert.False(t, ok)
	})

	t.Run(http.MethodGet, func(t *testing.T) {
		statuses, ok, err := Get(context.Background(), c, rawurl)

		_ = assert.NoError(t, err) &&
			assert.False(t, ok) &&
			assert.Empty(t, statuses)
	})
}

func TestHandlerServeHTTPViaHead(t *testing.T) {
	t.Parallel()

	for caseIndex, kase := range serveCases {
		t.Run(strconv.Itoa(caseIndex), func(t *testing.T) {
			t.Parallel()

			h, c, rawurl := newServer(t)

			defer h.Register("p1", kase.p1)()
			defer h.Register("p2", kase.p2)()
			defer h.Register("p3", kase.p3)()

			ok, err := Head(context.Background(), c, rawurl)
			require.NoError(t, err)

			assert.Equal(t, kase.wantOK, ok)
		})
	}
}

func TestHandlerServeHTTPViaGet(t *testing.T) {
	t.Parallel()

	for caseIndex, kase := range serveCases {
		t.Run(strconv.Itoa(caseIndex), func(t *testing.T) {
			t.Parallel()

			h, c, rawurl := newServer(t)

			defer h.Register("p1", kase.p1)()
			defer h.Register("p2", kase.p2)()
			defer h.Register("p3", kase.p3)()

			statuses, ok, err := Get(context.Background(), c, rawurl)
			require.NoError(t, err)
			require.Equal(t, kase.wantOK, ok)

			for id, s := range statuses {
				s.RegisteredAt = s.RegisteredAt.In(time.Local)
				statuses[id] = s
			}

			exp := map[string]Status{
				"p1": {Error: kase.e1},
				"p2": {Error: kase.e2},
				"p3": {Error: kase.e3},
			}
			for id, v := range exp {
				v.RegisteredAt = h.fetchComponentByID(id).timestamp.Round(0).In(time.Local)

				exp[id] = v
			}

			assert.Equal(t, exp, statuses)
		})
	}
}

var serveCases = []struct {
	p1           P
	e1           *string
	p2           P
	e2           *string
	p3           P
	e3           *string
	wantOK       bool
	wantMessages map[string]*string
}{
	0: {
		fromBool(true), nil,
		fromBool(true), nil,
		fromBool(true), nil,
		true,
		map[string]*string{
			"p1": nil,
			"p2": nil,
			"p3": nil,
		},
	},
	1: {
		fromBool(true), nil,
		fromBool(false), pointer(errFail.Error()),
		fromBool(true), nil,
		false,
		map[string]*string{
			"p1": nil,
			"p2": pointer(errFail.Error()),
			"p3": nil,
		},
	},
	2: {
		fromBool(true), nil,
		fromBool(false), pointer(errFail.Error()),
		fromBool(true), nil,
		false,
		map[string]*string{
			"p1": nil,
			"p2": pointer(errFail.Error()),
			"p3": nil,
		},
	},
}

func TestProbeQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		P           // probe
		contextFunc // probe's execution context
		error       // expected error
	}{
		// always return errUndefinedProbe for nil probes
		0: {nil, withBackground, errUndefinedProbe},
		1: {nil, withCanceled, errUndefinedProbe},
		2: {nil, withDeadlined, errUndefinedProbe},
		3: {nil, withCancelAfter(t, time.Second), errUndefinedProbe},
		4: {nil, withTimeoutAfter(t, time.Second), errUndefinedProbe},

		// return the context's error when queried with an invalid context
		5:  {newProbe(0, assert.AnError), withCanceled, context.Canceled},
		6:  {newProbe(0, assert.AnError), withDeadlined, context.DeadlineExceeded},
		7:  {newProbe(0, nil), withCanceled, context.Canceled},
		8:  {newProbe(0, nil), withDeadlined, context.DeadlineExceeded},
		9:  {newProbe(time.Second, assert.AnError), withCanceled, context.Canceled},
		10: {newProbe(time.Second, assert.AnError), withDeadlined, context.DeadlineExceeded},
		11: {newProbe(time.Second, nil), withCanceled, context.Canceled},
		12: {newProbe(time.Second, nil), withDeadlined, context.DeadlineExceeded},

		// return the probe's error when it responds within the validity period of the context
		13: {newProbe(fast, nil), withCancelAfter(t, slow), nil},
		14: {newProbe(fast, assert.AnError), withCancelAfter(t, slow), assert.AnError},
		15: {newProbe(fast, nil), withTimeoutAfter(t, slow), nil},
		16: {newProbe(fast, assert.AnError), withTimeoutAfter(t, slow), assert.AnError},

		// return the context's error when the probe fails to respond within the context's validity period
		17: {newProbe(slow, nil), withCancelAfter(t, fast), context.Canceled},
		18: {newProbe(slow, assert.AnError), withCancelAfter(t, fast), context.Canceled},
		19: {newProbe(slow, nil), withTimeoutAfter(t, fast), context.DeadlineExceeded},
		20: {newProbe(slow, assert.AnError), withTimeoutAfter(t, fast), context.DeadlineExceeded},
	}

	for caseIndex, kase := range cases {
		t.Run(strconv.Itoa(caseIndex), func(t *testing.T) {
			t.Parallel()

			ctx := kase.contextFunc(t)

			err := kase.P.query(ctx)

			if kase.error == nil {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, kase.error, err)
			}
		})
	}
}

func TestFromAtomicBool(t *testing.T) {
	var (
		b atomic.Bool

		p = FromAtomicBool(&b)
	)

	b.Store(true)
	assert.NoError(t, p.query(context.Background()))

	b.Store(false)
	assert.ErrorIs(t, errFail, p.query(context.Background()))
}

func TestDetermineClient(t *testing.T) {
	assert.Same(t, http.DefaultClient, determineClient(nil))

	exp := &http.Client{}
	assert.Same(t, exp, determineClient(exp))
}

func TestWithTimeout(t *testing.T) {
	h, c, rawurl := newServer(t, WithTimeout(fast))

	defer h.Register("slow", func(ctx context.Context) error {
		time.Sleep(slow)

		return nil
	})()

	statuses, ok, err := Get(context.Background(), c, rawurl)
	require.NoError(t, err, context.DeadlineExceeded)
	require.False(t, ok)
	require.Len(t, statuses, 1)
	s := statuses["slow"]

	_ = assert.NotNil(t, s.Error) &&
		assert.Equal(t, *s.Error, context.DeadlineExceeded.Error())
}

func (h *Handler) fetchComponentByID(id string) *component {
	return h.components[id]
}

type contextFunc func(*testing.T) context.Context

func withBackground(*testing.T) context.Context { return context.Background() }

func withCanceled(*testing.T) context.Context { return canceled }

func withDeadlined(*testing.T) context.Context { return deadlined }

func withCancelAfter(t *testing.T, after time.Duration) contextFunc {
	t.Helper()

	return func(t *testing.T) context.Context {
		t.Helper()

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(after)

			cancel()
		}()

		return ctx
	}
}

func withTimeoutAfter(t *testing.T, after time.Duration) contextFunc {
	t.Helper()

	return func(t *testing.T) context.Context {
		t.Helper()

		ctx, cancel := context.WithTimeout(context.Background(), after)
		t.Cleanup(cancel)

		return ctx
	}
}

func newProbe(wait time.Duration, err error) P {
	return func(context.Context) error {
		time.Sleep(wait)

		return err
	}
}

func fromBool(v bool) P {
	return func(context.Context) error {
		if !v {
			return errFail
		}
		return nil
	}
}

func newServer(t *testing.T, opts ...Option) (h *Handler, c *http.Client, rawurl string) {
	t.Helper()

	h = New(opts...)

	srv := httptest.NewServer(h)
	c = srv.Client()
	t.Cleanup(srv.Close)

	rawurl = srv.URL + "/"

	return
}

func pointer[T any](v T) *T { return &v }
