/*
 * @Author: rui.li
 * @Date: 2024-02-27 17:49:17
 * @LastEditors: rui.li
 * @LastEditTime: 2024-02-28 10:57:09
 * @FilePath: /DynamicBucketTusd/pkg/models/context.go
 */
package models

import (
	"context"
	"net/http"
	"time"

	"golang.org/x/exp/slog"
)

// httpContext is wrapper around context.Context that also carries the
// corresponding HTTP request and response writer, as well as an
// optional body reader
type HttpContext struct {
	context.Context

	// res and req are the native request and response instances
	res  http.ResponseWriter
	resC *http.ResponseController
	req  *http.Request

	// body is nil by default and set by the user if the request body is consumed.
	Body *BodyReader

	// cancel allows a user to cancel the internal request context, causing
	// the request body to be closed.
	cancel context.CancelCauseFunc

	// log is the logger for this request. It gets extended with more properties as the
	// request progresses and is identified.
	Log *slog.Logger
}

func NewHttpContext(ctx context.Context, req *http.Request, res http.ResponseWriter, resC *http.ResponseController, cancel context.CancelCauseFunc, log *slog.Logger) *HttpContext {
	return &HttpContext{
		Context: ctx,
		res:     res,
		resC:    resC,
		req:     req,
		Body:    nil, // body can be filled later for PATCH requests
		cancel:  cancel,
		Log:     log,
	}
}

func (c HttpContext) GetReq() *http.Request {
	return c.req

}

func (c HttpContext) GetRes() http.ResponseWriter {
	return c.res
}

func (c HttpContext) GetResC() *http.ResponseController {
	return c.resC
}

func (c HttpContext) GetCancel() context.CancelCauseFunc {
	return c.cancel
}

func (c HttpContext) Value(key any) any {
	// We overwrite the Value function to ensure that the values from the request
	// context are returned because c.Context does not contain any values.
	return c.req.Context().Value(key)
}

// newDelayedContext returns a context that is cancelled with a delay. If the parent context
// is done, the new context will also be cancelled but only after waiting the specified delay.
// Note: The parent context MUST be cancelled or otherwise this will leak resources. In the
// case of http.Request.Context, the net/http package ensures that the context is always cancelled.
func NewDelayedContext(parent context.Context, delay time.Duration) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-parent.Done()
		<-time.After(delay)
		cancel()
	}()

	return ctx
}
