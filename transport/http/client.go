package http

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	gkit "github.com/bobobox-id/gkit/core"
)

// HTTPClient is an interface that models *http.Client.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client wraps a URL and provides a method that implements endpoint.Endpoint.
type Client[Req, Res any] struct {
	client         HTTPClient
	req            gkit.EncodeDecodeFunc[Req, *http.Request]
	dec            gkit.EncodeDecodeFunc[*http.Response, Res]
	before         []RequestFunc
	after          []ClientResponseFunc
	finalizer      []ClientFinalizerFunc
	bufferedStream bool
}

// NewClient constructs a usable Client for a single remote method.
func NewClient[Req, Res any](method string, tgt *url.URL, enc EncodeRequestFunc[Req], dec gkit.EncodeDecodeFunc[*http.Response, Res], options ...ClientOption[Req, Res]) *Client[Req, Res] {
	return NewExplicitClient[Req, Res](makeCreateRequestFunc(method, tgt, enc), dec, options...)
}

// NewExplicitClient is like NewClient but uses a CreateRequestFunc instead of a
// method, target URL, and EncodeRequestFunc, which allows for more control over
// the outgoing HTTP request.
func NewExplicitClient[Req, Res any](req gkit.EncodeDecodeFunc[Req, *http.Request], dec gkit.EncodeDecodeFunc[*http.Response, Res], options ...ClientOption[Req, Res]) *Client[Req, Res] {
	c := &Client[Req, Res]{
		client: http.DefaultClient,
		req:    req,
		dec:    dec,
	}
	for _, option := range options {
		option(c)
	}
	return c
}

// ClientOption sets an optional parameter for clients.
type ClientOption[Req, Res any] gkit.Option[*Client[Req, Res]]

// SetClient sets the underlying HTTP client used for requests.
// By default, http.DefaultClient is used.
func SetClient[Req, Res any](client HTTPClient) ClientOption[Req, Res] {
	return func(c *Client[Req, Res]) { c.client = client }
}

// ClientBefore adds one or more RequestFuncs to be applied to the outgoing HTTP
// request before it's invoked.
func ClientBefore[Req, Res any](before ...RequestFunc) ClientOption[Req, Res] {
	return func(c *Client[Req, Res]) { c.before = append(c.before, before...) }
}

// ClientAfter adds one or more ClientResponseFuncs, which are applied to the
// incoming HTTP response prior to it being decoded. This is useful for
// obtaining anything off of the response and adding it into the context prior
// to decoding.
func ClientAfter[Req, Res any](after ...ClientResponseFunc) ClientOption[Req, Res] {
	return func(c *Client[Req, Res]) { c.after = append(c.after, after...) }
}

// ClientFinalizer adds one or more ClientFinalizerFuncs to be executed at the
// end of every HTTP request. Finalizers are executed in the order in which they
// were added. By default, no finalizer is registered.
func ClientFinalizer[Req, Res any](f ...ClientFinalizerFunc) ClientOption[Req, Res] {
	return func(s *Client[Req, Res]) { s.finalizer = append(s.finalizer, f...) }
}

// BufferedStream sets whether the HTTP response body is left open, allowing it
// to be read from later. Useful for transporting a file as a buffered stream.
// That body has to be drained and closed to properly end the request.
func BufferedStream[Req, Res any](buffered bool) ClientOption[Req, Res] {
	return func(c *Client[Req, Res]) { c.bufferedStream = buffered }
}

// Endpoint returns a usable Go kit endpoint that calls the remote HTTP endpoint.
func (c Client[Req, Res]) Endpoint() gkit.Endpoint[Req, Res] {
	return func(ctx context.Context, request Req) (Res, error) {
		ctx, cancel := context.WithCancel(ctx)

		var (
			resp     *http.Response
			response Res
			err      error
		)
		if c.finalizer != nil {
			defer func() {
				if resp != nil {
					ctx = context.WithValue(ctx, ContextKeyResponseHeaders, resp.Header)
					ctx = context.WithValue(ctx, ContextKeyResponseSize, resp.ContentLength)
				}
				for _, f := range c.finalizer {
					f(ctx, err)
				}
			}()
		}

		req, err := c.req(ctx, request)
		if err != nil {
			cancel()
			return response, err
		}

		for _, f := range c.before {
			ctx = f(ctx, req)
		}

		resp, err = c.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			return response, err
		}

		// If the caller asked for a buffered stream, we don't cancel the
		// context when the endpoint returns. Instead, we should call the
		// cancel func when closing the response body.
		if c.bufferedStream {
			resp.Body = bodyWithCancel{ReadCloser: resp.Body, cancel: cancel}
		} else {
			defer resp.Body.Close()
			defer cancel()
		}

		for _, f := range c.after {
			ctx = f(ctx, resp)
		}

		response, err = c.dec(ctx, resp)
		if err != nil {
			return response, err
		}

		return response, nil
	}
}

// bodyWithCancel is a wrapper for an io.ReadCloser with also a
// cancel function which is called when the Close is used
type bodyWithCancel struct {
	io.ReadCloser

	cancel context.CancelFunc
}

func (bwc bodyWithCancel) Close() error {
	bwc.ReadCloser.Close()
	bwc.cancel()
	return nil
}

// ClientFinalizerFunc can be used to perform work at the end of a client HTTP
// request, after the response is returned. The principal
// intended use is for error logging. Additional response parameters are
// provided in the context under keys with the ContextKeyResponse prefix.
// Note: err may be nil. There maybe also no additional response parameters
// depending on when an error occurs.
type ClientFinalizerFunc func(ctx context.Context, err error)

// EncodeJSONRequest is an EncodeRequestFunc that serializes the request as a
// JSON object to the Request body. Many JSON-over-HTTP services can use it as
// a sensible default. TODO: If the request implements Headerer, the provided headers
// will be applied to the request.
func EncodeJSONRequest[Req any](c context.Context, r *http.Request, request Req) error {
	r.Header.Set("Content-Type", "application/json; charset=utf-8")

	if headerer, ok := any(request).(Headerer); ok {
		for k := range headerer.Headers() {
			r.Header.Set(k, headerer.Headers().Get(k))
		}
	}

	var b bytes.Buffer
	r.Body = io.NopCloser(&b)

	return json.NewEncoder(&b).Encode(request)
}

func makeCreateRequestFunc[Req any](method string, target *url.URL, enc EncodeRequestFunc[Req]) gkit.EncodeDecodeFunc[Req, *http.Request] {
	return func(ctx context.Context, request Req) (*http.Request, error) {
		req, err := http.NewRequest(method, target.String(), nil)
		if err != nil {
			return nil, err
		}

		if err = enc(ctx, req, request); err != nil {
			return nil, err
		}

		return req, nil
	}
}
