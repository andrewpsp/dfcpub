// Package dfc is a scalable object-storage based caching system with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
)

// NOTE: This code can be cleaned up and optimized in many ways.
// It is expected that a bit portion of it will be refactored/rewritten.
// For now, this implementation is a cheap way of prototyping
// and testing DFC => DFC (i.e. multi-tier) relationships.

const (
	// URL of the tier-2 DFC proxy
	proxyURL    = "http://localhost:8082"
	tier2Bucket = "nvdfc"
)

// The following five APIs are symmetric with ones provided in aws.go and gcp.go, except for these missing APIs:
// 1. getbucketnames
// 2. putobj

func (t *targetrunner) dfcListBucket(ct context.Context, bucket string, r *http.Request) (jsbytes []byte, errstr string, errcode int) {
	var (
		url = proxyURL + URLPath(Rversion, Rbuckets, bucket)
	)

	req, err := http.NewRequest("GET", url, r.Body)
	if err != nil {
		return []byte{}, err.Error(), 1
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := t.httprunner.httpclientLongTimeout.Do(req)
	if err != nil {
		b, e := ioutil.ReadAll(resp.Body)
		if e != nil {
			return []byte{}, e.Error(), 2
		}
		return b, err.Error(), 3
	}

	if resp.StatusCode >= http.StatusBadRequest {
		b, e := ioutil.ReadAll(resp.Body)
		if e != nil {
			return []byte{}, e.Error(), 4
		}
		return b, fmt.Sprintf("HTTP error %d, message = %v", resp.StatusCode, string(b)), 5
	}

	jsbytes, err = ioutil.ReadAll(resp.Body)
	if err != nil {
		return []byte{}, err.Error(), 6
	}
	return
}

func (t *targetrunner) dfcHeadBucket(ct context.Context, bucket string) (bucketprops simplekvs, errstr string, errcode int) {
	var (
		url = proxyURL + URLPath(Rversion, Rbuckets, bucket)
	)
	bucketprops = make(simplekvs)

	r, err := t.httprunner.httpclientLongTimeout.Head(url)
	if err != nil {
		return bucketprops, err.Error(), 1
	}

	if r != nil && r.StatusCode >= http.StatusBadRequest {
		b, err := ioutil.ReadAll(r.Body)
		if err != nil {
			err = fmt.Errorf("failed to read response body, err = %s", err)
			return bucketprops, err.Error(), 2
		}
		err = fmt.Errorf("head bucket: %s failed, HTTP status code: %d, HTTP response body: %s",
			bucket, r.StatusCode, string(b))
		return bucketprops, err.Error(), 3
	}
	bucketprops[CloudProvider] = r.Header.Get(CloudProvider)
	bucketprops[Versioning] = r.Header.Get(Versioning)
	return
}

func (t *targetrunner) dfcHeadObject(ct context.Context, bucket string, objname string) (objmeta simplekvs, errstr string, errcode int) {
	var (
		url = proxyURL + URLPath(Rversion, Robjects, bucket, objname)
	)
	objmeta = make(simplekvs)

	r, err := t.httprunner.httpclientLongTimeout.Head(url)
	if err != nil {
		return objmeta, err.Error(), 1
	}
	if r != nil && r.StatusCode >= http.StatusBadRequest {
		b, ioErr := ioutil.ReadAll(r.Body)
		if ioErr != nil {
			err = fmt.Errorf("failed to read response body, err = %s", ioErr)
			return objmeta, err.Error(), 2
		}
		err = fmt.Errorf("head bucket/object: %s/%s failed, HTTP status code: %d, HTTP response body: %s",
			bucket, objname, r.StatusCode, string(b))
		return objmeta, err.Error(), 3
	}
	objmeta[CloudProvider] = r.Header.Get(CloudProvider)
	if s := r.Header.Get(Size); s != "" {
		objmeta[Size] = s
	}
	if v := r.Header.Get(Version); v != "" {
		objmeta[Version] = v
	}
	return
}

func (t *targetrunner) dfcGetObject(ct context.Context, fqn, bucket, objname string) (props *objectProps, errstr string, errcode int) {
	var (
		url = proxyURL + URLPath(Rversion, Robjects, bucket, objname)
	)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err.Error(), 1
	}

	resp, err := t.httprunner.httpclientLongTimeout.Do(req)
	if err != nil {
		return nil, err.Error(), 2
	}

	if resp.StatusCode >= http.StatusBadRequest {
		b, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err.Error(), 3
		}
		return nil, fmt.Sprintf("HTTP error %d, message = %v", resp.StatusCode, string(b)), 4
	}

	props = &objectProps{}
	_, props.nhobj, props.size, errstr = t.receive(fqn, false, objname, "", nil, resp.Body)
	return
}

func (t *targetrunner) dfcDeleteObj(ct context.Context, bucket, objname string) (errstr string, errcode int) {
	var (
		url = proxyURL + URLPath(Rversion, Robjects, bucket, objname)
	)

	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		return err.Error(), 1
	}

	resp, err := t.httprunner.httpclientLongTimeout.Do(req)
	if err != nil {
		return err.Error(), 2
	}

	if resp.StatusCode >= http.StatusBadRequest {
		b, e := ioutil.ReadAll(resp.Body)
		if e != nil {
			return e.Error(), 3
		}
		return fmt.Sprintf("HTTP error %d, message = %v", resp.StatusCode, string(b)), 2
	}
	return
}
