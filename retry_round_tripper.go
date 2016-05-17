package retryhttp

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"syscall"
	"time"

	"github.com/pivotal-golang/lager"
)

//go:generate counterfeiter . Sleeper

type Sleeper interface {
	Sleep(time.Duration)
}

//go:generate counterfeiter . RetryPolicy

type RetryPolicy interface {
	DelayFor(uint) (time.Duration, bool)
}

//go:generate counterfeiter . Retryer
//common interface between HijakcRetry and StreamRetry
type Retryer interface {
	RoundTrip(*http.Request) (*http.Response, error)
}

type HijackRetry struct {
	Client httputil.ClientConn
}

func (r *HijackRetry) RoundTrip(request *http.Request) (*http.Response, error) {
	return r.Client.Do(request)
}

//TODO: circular reference
type StreamRetry struct {
	Client Retryer
}

func (r *StreamRetry) RoundTrip(request *http.Request) (*http.Response, error) {
	return r.Client.RoundTrip(request)
}

type RetryRoundTripper struct {
	Logger      lager.Logger
	Sleeper     Sleeper
	RetryPolicy RetryPolicy
	Retryer     Retryer
}

type RetryReadCloser struct {
	io.ReadCloser
	IsRead bool
}

func (rrc *RetryReadCloser) Read(p []byte) (n int, err error) {
	rrc.IsRead = true
	return rrc.ReadCloser.Read(p)
}

func (d *RetryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	retryLogger := d.Logger.Session("retry")
	startTime := time.Now()

	retryReadCloser := &RetryReadCloser{request.Body, false}
	request.Body = retryReadCloser

	var err error
	var failedAttempts uint
	for {
		var response *http.Response

		//response, err = d.RoundTripper.RoundTrip(request)
		//response, err = d.ConnClient.Retry(request)
		response, err = d.Retryer.RoundTrip(request)
		if err == nil {
			return response, nil
		}

		if retryReadCloser.IsRead {
			break
		}

		if !retryable(request, err) {
			break
		}

		failedAttempts++

		delay, keepRetrying := d.RetryPolicy.DelayFor(failedAttempts)
		if !keepRetrying {
			retryLogger.Error("giving-up", errors.New("giving up"), lager.Data{
				"total-failed-attempts": failedAttempts,
				"ran-for":               time.Now().Sub(startTime).String(),
			})

			break
		}

		retryLogger.Info("retrying", lager.Data{
			"failed-attempts": failedAttempts,
			"next-attempt-in": delay.String(),
			"ran-for":         time.Now().Sub(startTime).String(),
		})

		d.Sleeper.Sleep(delay)
	}

	return nil, err
}

func retryable(request *http.Request, err error) bool {
	if neterr, ok := err.(net.Error); ok {
		if neterr.Temporary() {
			return true
		}
	}

	s := err.Error()
	for _, retryableError := range retryableErrors {
		if strings.HasSuffix(s, retryableError.Error()) {
			return true
		}
	}

	return false
}

var retryableErrors = []error{
	syscall.ECONNREFUSED,
	syscall.ECONNRESET,
	syscall.ETIMEDOUT,
	errors.New("i/o timeout"),
	errors.New("no such host"),
	errors.New("remote error: handshake failure"),
}
