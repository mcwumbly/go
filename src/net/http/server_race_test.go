// +build race

// Copyright 2020 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Server race tests

package http_test

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestExpectedContinueRace is used to validate the existence of a race when
// too many, concurrent HTTP clients are sent an 100-continue header. The race
// may be validated with the following command:
//
//         GORACE="halt_on_error=1" go test -race \
//           -v -run TestExpectedContinueRace net/http
//
// The above command allows up to five minutes for the race to occur, although
// it usually does not take that long. The environment variable
// GORACE="halt_on_error=1" ensures the test will exit with a non-zero exit
// code when the race occurs, otherwise the test would continue executing.
//
// The patch to correct the race is activated via an environment variable, and
// when used in conjunction with this test can validate the race no longer
// occurs:
//
//         NET_HTTP_SERVER_SYNC_BUFIO_WRITER=1 \
//           GORACE="halt_on_error=1" go test -race \
//           -v -run TestExpectedContinueRace net/http
//
// After five minutes without the race, the test completes successfully.
func TestExpectedContinueRace(t *testing.T) {
	const (
		concurrentClients = 10
		httpResponseValue = "this call was relayed by the reverse proxy"

		// printSuccessEveryN is used to determine when a "success" message is
		// printed. If printSuccessEveryN is divided into the total number of
		// requests with no remainder, then a message is emitted.
		// This prevents the screen from being cluttered with output.
		printSuccessEveryN = uint64(1000)
	)

	var (
		requestCounter uint64
		started        = time.Now()
		activeClients  sync.WaitGroup
	)

	// Track when the clients are no longer active.
	activeClients.Add(concurrentClients)

	cst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, httpResponseValue)
	}))
	defer cst.Close()

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("Failed to find available port: %s\n", err)
	}
	defer ln.Close()

	backendURL, _ := url.Parse(cst.URL)
	rpxy := httputil.NewSingleHostReverseProxy(backendURL)
	addr := ln.Addr().String()

	// Start the web server.
	go func() {
		isValidErr := func(err error) bool {
			if err == nil {
				return true
			}
			if err == http.ErrServerClosed {
				return true
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				return true
			}
			return false
		}
		if err := http.Serve(ln, rpxy); !isValidErr(err) {
			t.Fatalf("Reverse proxy serve error: %s\n", err)
		}
	}()

	// Create a context that times out after five minutes. If after five minutes
	// no errors have occurred, then the program exits with a successful exit
	// code.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
	defer cancel()

	print := func(reqNum uint64, format string, args ...interface{}) {
		elapsed := time.Since(started)
		message := fmt.Sprintf(format, args...)
		t.Logf("%s\t%d\t%s\n", elapsed, reqNum, message)
	}

	runClients := func(ctx context.Context, numClients int, addr string) {
		var (
			client = &http.Client{}
			url    = "http://" + addr

			// Request needs a larger body to see the error happen more quickly.
			// It is reproducable with smaller body sizes, but it takes longer to
			// fail
			bodyString = strings.Repeat("a", 4096)
		)

		doRequest := func(req *http.Request) {
			reqNum := atomic.AddUint64(&requestCounter, 1)
			resp, err := client.Do(req)
			if err != nil {
				print(reqNum, "failed: %s", err)
				return
			}
			defer resp.Body.Close()
			if text, err := ioutil.ReadAll(resp.Body); err != nil {
				print(reqNum, "failed to read response: %s", err)
			} else if string(text) != httpResponseValue {
				print(reqNum, "unexpected response: %s", text)
			} else {
				if reqNum%printSuccessEveryN == 0 {
					print(reqNum, "success")
				}
			}
		}

		for i := 0; i < numClients; i++ {
			go func() {
				defer activeClients.Done()
				for {
					select {
					case <-ctx.Done():
						return
					default:
						buf := bytes.NewBufferString(bodyString)
						req, _ := http.NewRequest("POST", url, buf)
						req.Header.Add("Expect", "100-continue")
						doRequest(req)
					}
				}
			}()
		}
	}

	// Run the concurrent clients.
	runClients(ctx, concurrentClients, addr)

	// Wait until the context times out.
	<-ctx.Done()

	// Wait until there are no longer any active clients.
	activeClients.Wait()
}
