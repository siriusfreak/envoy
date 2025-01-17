/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package http

/*
// ref https://github.com/golang/go/issues/25832

#cgo CFLAGS: -I../../../../../../common/go/api -I../api
#cgo linux LDFLAGS: -Wl,-unresolved-symbols=ignore-all
#cgo darwin LDFLAGS: -Wl,-undefined,dynamic_lookup

#include <stdlib.h>
#include <string.h>

#include "api.h"

*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
)

var ErrDupRequestKey = errors.New("dup request key")

var Requests = &requestMap{}

var (
	initialized      = false
	envoyConcurrency uint32
)

// EnvoyConcurrency returns the concurrency Envoy was set to run at. This can be used to optimize HTTP filters that need
// memory per worker thread to avoid locks.
//
// Note: Do not use inside of an `init()` function, the value will not be populated yet. Use within the filters
// `StreamFilterConfigFactory` or `StreamFilterConfigParser`
func EnvoyConcurrency() uint32 {
	if !initialized {
		panic("concurrency has not yet been initialized, do not access within an init()")
	}
	return envoyConcurrency
}

type requestMap struct {
	initOnce sync.Once
	requests []map[*C.httpRequest]*httpRequest
}

func (f *requestMap) initialize(concurrency uint32) {
	f.initOnce.Do(func() {
		initialized = true
		envoyConcurrency = concurrency
		f.requests = make([]map[*C.httpRequest]*httpRequest, concurrency)
		for i := uint32(0); i < concurrency; i++ {
			f.requests[i] = map[*C.httpRequest]*httpRequest{}
		}
	})
}

func (f *requestMap) StoreReq(key *C.httpRequest, req *httpRequest) error {
	m := f.requests[key.worker_id]
	if _, ok := m[key]; ok {
		return ErrDupRequestKey
	}
	m[key] = req
	return nil
}

func (f *requestMap) GetReq(key *C.httpRequest) *httpRequest {
	return f.requests[key.worker_id][key]
}

func (f *requestMap) DeleteReq(key *C.httpRequest) {
	delete(f.requests[key.worker_id], key)
}

func (f *requestMap) Clear() {
	for idx := range f.requests {
		f.requests[idx] = map[*C.httpRequest]*httpRequest{}
	}
}

func requestFinalize(r *httpRequest) {
	r.Finalize(api.NormalFinalize)
}

func createRequest(r *C.httpRequest) *httpRequest {
	req := &httpRequest{
		req: r,
	}
	req.cond.L = &req.waitingLock
	// NP: make sure filter will be deleted.
	runtime.SetFinalizer(req, requestFinalize)

	err := Requests.StoreReq(r, req)
	if err != nil {
		panic(fmt.Sprintf("createRequest failed, err: %s", err.Error()))
	}

	configId := uint64(r.configId)
	filterFactory := getOrCreateHttpFilterFactory(req.pluginName(), configId)
	f := filterFactory(req)
	req.httpFilter = f

	return req
}

func getRequest(r *C.httpRequest) *httpRequest {
	return Requests.GetReq(r)
}

//export envoyGoFilterOnHttpHeader
func envoyGoFilterOnHttpHeader(r *C.httpRequest, endStream, headerNum, headerBytes uint64) uint64 {
	var req *httpRequest
	phase := api.EnvoyRequestPhase(r.phase)
	// early SendLocalReply or OnLogDownstreamStart may run before the header handling
	req = getRequest(r)
	if req == nil {
		req = createRequest(r)
	}

	if req.pInfo.paniced {
		// goroutine panic in the previous state that could not sendLocalReply, delay terminating the request here,
		// to prevent error from spreading.
		req.sendPanicReply(req.pInfo.details)
		return uint64(api.LocalReply)
	}
	defer req.RecoverPanic()
	f := req.httpFilter

	var status api.StatusType
	switch phase {
	case api.DecodeHeaderPhase:
		header := &requestHeaderMapImpl{
			requestOrResponseHeaderMapImpl{
				headerMapImpl{
					request:     req,
					headerNum:   headerNum,
					headerBytes: headerBytes,
				},
			},
		}
		status = f.DecodeHeaders(header, endStream == 1)
	case api.DecodeTrailerPhase:
		header := &requestTrailerMapImpl{
			requestOrResponseTrailerMapImpl{
				headerMapImpl{
					request:     req,
					headerNum:   headerNum,
					headerBytes: headerBytes,
				},
			},
		}
		status = f.DecodeTrailers(header)
	case api.EncodeHeaderPhase:
		header := &responseHeaderMapImpl{
			requestOrResponseHeaderMapImpl{
				headerMapImpl{
					request:     req,
					headerNum:   headerNum,
					headerBytes: headerBytes,
				},
			},
		}
		status = f.EncodeHeaders(header, endStream == 1)
	case api.EncodeTrailerPhase:
		header := &responseTrailerMapImpl{
			requestOrResponseTrailerMapImpl{
				headerMapImpl{
					request:     req,
					headerNum:   headerNum,
					headerBytes: headerBytes,
				},
			},
		}
		status = f.EncodeTrailers(header)
	}

	if endStream == 1 && (status == api.StopAndBuffer || status == api.StopAndBufferWatermark) {
		panic("received wait data status when there is no data, please fix the returned status")
	}

	return uint64(status)
}

//export envoyGoFilterOnHttpData
func envoyGoFilterOnHttpData(r *C.httpRequest, endStream, buffer, length uint64) uint64 {
	req := getRequest(r)
	if req.pInfo.paniced {
		// goroutine panic in the previous state that could not sendLocalReply, delay terminating the request here,
		// to prevent error from spreading.
		req.sendPanicReply(req.pInfo.details)
		return uint64(api.LocalReply)
	}
	defer req.RecoverPanic()

	f := req.httpFilter
	isDecode := api.EnvoyRequestPhase(r.phase) == api.DecodeDataPhase

	buf := &httpBuffer{
		request:             req,
		envoyBufferInstance: buffer,
		length:              length,
	}

	var status api.StatusType
	if isDecode {
		status = f.DecodeData(buf, endStream == 1)
	} else {
		status = f.EncodeData(buf, endStream == 1)
	}
	return uint64(status)
}

//export envoyGoFilterOnHttpLog
func envoyGoFilterOnHttpLog(r *C.httpRequest, logType uint64) {
	req := getRequest(r)
	if req == nil {
		req = createRequest(r)
	}

	defer req.RecoverPanic()

	v := api.AccessLogType(logType)

	f := req.httpFilter
	switch v {
	case api.AccessLogDownstreamStart:
		f.OnLogDownstreamStart()
	case api.AccessLogDownstreamPeriodic:
		f.OnLogDownstreamPeriodic()
	case api.AccessLogDownstreamEnd:
		f.OnLog()
	default:
		api.LogErrorf("access log type %d is not supported yet", logType)
	}
}

//export envoyGoFilterOnHttpDestroy
func envoyGoFilterOnHttpDestroy(r *C.httpRequest, reason uint64) {
	req := getRequest(r)
	// do nothing even when req.panic is true, since filter is already destroying.
	defer req.RecoverPanic()

	req.resumeWaitCallback()

	v := api.DestroyReason(reason)

	f := req.httpFilter
	f.OnDestroy(v)

	// Break circular references between httpRequest and StreamFilter,
	// since Finalizers don't work with circular references,
	// otherwise, it will leads to memory leaking.
	req.httpFilter = nil

	Requests.DeleteReq(r)
}

//export envoyGoRequestSemaDec
func envoyGoRequestSemaDec(r *C.httpRequest) {
	req := getRequest(r)
	defer req.RecoverPanic()
	req.resumeWaitCallback()
}
