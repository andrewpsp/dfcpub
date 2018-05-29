// Package dfc is a scalable object-storage based caching system with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */
package dfc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/golang/glog"
)

const (
	awsPutDfcHashType = "x-amz-meta-dfc-hash-type"
	awsPutDfcHashVal  = "x-amz-meta-dfc-hash-val"
	awsGetDfcHashType = "X-Amz-Meta-Dfc-Hash-Type"
	awsGetDfcHashVal  = "X-Amz-Meta-Dfc-Hash-Val"
	awsMultipartDelim = "-"
	awsMaxPageSize    = 1000
)

//======
//
// implements cloudif
//
//======
type awsimpl struct {
	t *targetrunner
}

//======
//
// session FIXME: optimize
//
//======
// A new session is created in two ways:
// 1. Authn is disabled or directory with credentials is not defined
//    In this case a session is created using default credentials from
//    configuration file in ~/.aws/credentials and environment variables
// 2. Authn is enabled and directory with credential files is set
//    The function looks for 'credentials' file in the directory.
//    A userID is retrieved from token. The userID section must exist
//    in credential file located in the given directory.
//    A userID section should look like this:
//    [userID]
//    region = us-east-1
//    aws_access_key_id = USERKEY
//    aws_secret_access_key = USERSECRET
// If creation of a session with provided directory and userID fails, it
// tries to create a session with default parameters
func createSession(ct context.Context) *session.Session {
	// TODO: avoid creating sessions for each request
	userID := userIDFromContext(ct)
	if userID == "" {
		// default session
		return session.Must(session.NewSessionWithOptions(session.Options{
			SharedConfigState: session.SharedConfigEnable}))
	}

	credFile, err := userCredsPathFromContext(ct, userID, ProviderAmazon)
	if err != nil {
		glog.Errorf("Failed to read user credentials: %v", err)
		return session.Must(session.NewSessionWithOptions(session.Options{
			SharedConfigState: session.SharedConfigEnable}))
	}

	creds := credentials.NewSharedCredentials(credFile, userID)
	_, err = creds.Get()
	if err != nil {
		glog.Errorf("Failed to read credentials from file: %v", err)
		return session.Must(session.NewSessionWithOptions(session.Options{
			SharedConfigState: session.SharedConfigEnable}))
	}

	conf := aws.Config{
		Credentials: creds,
	}
	return session.Must(session.NewSessionWithOptions(session.Options{
		// Applies user-base credentials
		Config: conf,
		// To enable reading Region from the provided credentilas file.
		// Disable means Region (in aws.Config) must be set manually,
		//    otherwise error 'MissingRegion' raises
		SharedConfigState: session.SharedConfigEnable,
		// Sets the file name with regions
		SharedConfigFiles: []string{credFile},
		// Sets the section of INIs to read Region and Credential
		Profile: userID,
	}))
}

func awsErrorToHTTP(awsError error) int {
	if reqErr, ok := awsError.(awserr.RequestFailure); ok {
		return reqErr.StatusCode()
	}

	return http.StatusInternalServerError
}

func awsIsVersionSet(version *string) bool {
	return version != nil && *version != "null" && *version != ""
}

//==================
//
// bucket operations
//
//==================
func (awsimpl *awsimpl) listbucket(ct context.Context, bucket string, msg *GetMsg) (jsbytes []byte, errstr string, errcode int) {
	if glog.V(4) {
		glog.Infof("listbucket %s", bucket)
	}
	sess := createSession(ct)
	svc := s3.New(sess)

	params := &s3.ListObjectsInput{Bucket: aws.String(bucket)}
	if msg.GetPrefix != "" {
		params.Prefix = aws.String(msg.GetPrefix)
	}
	if msg.GetPageMarker != "" {
		params.Marker = aws.String(msg.GetPageMarker)
	}
	if msg.GetPageSize != 0 {
		if msg.GetPageSize > awsMaxPageSize {
			glog.Warningf("AWS maximum page size is %d (%d requested). Returning the first %d keys",
				awsMaxPageSize, msg.GetPageSize, awsMaxPageSize)
			msg.GetPageSize = awsMaxPageSize
		}
		params.MaxKeys = aws.Int64(int64(msg.GetPageSize))
	}

	resp, err := svc.ListObjects(params)
	if err != nil {
		errstr = err.Error()
		errcode = awsErrorToHTTP(err)
		return
	}

	verParams := &s3.ListObjectVersionsInput{Bucket: aws.String(bucket)}
	if msg.GetPrefix != "" {
		verParams.Prefix = aws.String(msg.GetPrefix)
	}

	var versions map[string]*string
	if strings.Contains(msg.GetProps, GetPropsVersion) {
		verResp, err := svc.ListObjectVersions(verParams)
		if err != nil {
			errstr = err.Error()
			errcode = awsErrorToHTTP(err)
			return
		}

		versions = make(map[string]*string, initialBucketListSize)
		for _, vers := range verResp.Versions {
			if *(vers.IsLatest) && awsIsVersionSet(vers.VersionId) {
				versions[*(vers.Key)] = vers.VersionId
			}
		}
	}

	// var msg GetMsg
	var reslist = BucketList{Entries: make([]*BucketEntry, 0, initialBucketListSize)}
	for _, key := range resp.Contents {
		entry := &BucketEntry{}
		entry.Name = *(key.Key)
		if strings.Contains(msg.GetProps, GetPropsSize) {
			entry.Size = *(key.Size)
		}
		if strings.Contains(msg.GetProps, GetPropsCtime) {
			t := *(key.LastModified)
			switch msg.GetTimeFormat {
			case "":
				fallthrough
			case RFC822:
				entry.Ctime = t.Format(time.RFC822)
			default:
				entry.Ctime = t.Format(msg.GetTimeFormat)
			}
		}
		if strings.Contains(msg.GetProps, GetPropsChecksum) {
			omd5, _ := strconv.Unquote(*key.ETag)
			entry.Checksum = omd5
		}
		if strings.Contains(msg.GetProps, GetPropsVersion) {
			if val, ok := versions[*(key.Key)]; ok && awsIsVersionSet(val) {
				entry.Version = *val
			}
		}
		// TODO: other GetMsg props TBD
		reslist.Entries = append(reslist.Entries, entry)
	}
	if glog.V(4) {
		glog.Infof("listbucket count %d", len(reslist.Entries))
	}

	if *resp.IsTruncated {
		// For AWS, resp.NextMarker is only set when a query has a delimiter.
		// Without a delimiter, NextMarker should be the last returned key.
		reslist.PageMarker = reslist.Entries[len(reslist.Entries)-1].Name
	}

	jsbytes, err = json.Marshal(reslist)
	assert(err == nil, err)
	return
}

func (awsimpl *awsimpl) headbucket(ct context.Context, bucket string) (bucketprops map[string]string, errstr string, errcode int) {
	if glog.V(4) {
		glog.Infof("headbucket %s", bucket)
	}
	bucketprops = make(map[string]string)

	sess := createSession(ct)
	svc := s3.New(sess)
	input := &s3.HeadBucketInput{Bucket: aws.String(bucket)}

	_, err := svc.HeadBucket(input)
	if err != nil {
		errcode = awsErrorToHTTP(err)
		errstr = fmt.Sprintf("The bucket %s either does not exist or is not accessible, err: %v", bucket, err)
		return
	}
	bucketprops[CloudProvider] = ProviderAmazon

	inputVers := &s3.GetBucketVersioningInput{Bucket: aws.String(bucket)}
	result, err := svc.GetBucketVersioning(inputVers)
	if err != nil {
		errcode = awsErrorToHTTP(err)
		errstr = fmt.Sprintf("The bucket %s either does not exist or is not accessible, err: %v", bucket, err)
	} else {
		if result.Status != nil && *result.Status == s3.BucketVersioningStatusEnabled {
			bucketprops[Versioning] = VersionCloud
		} else {
			bucketprops[Versioning] = VersionNone
		}
	}
	return
}

func (awsimpl *awsimpl) getbucketnames(ct context.Context) (buckets []string, errstr string, errcode int) {
	sess := createSession(ct)
	svc := s3.New(sess)
	result, err := svc.ListBuckets(&s3.ListBucketsInput{})
	if err != nil {
		errcode = awsErrorToHTTP(err)
		errstr = fmt.Sprintf("Failed to list all buckets, err: %v", err)
		return
	}
	buckets = make([]string, 0, 16)
	for _, bkt := range result.Buckets {
		if glog.V(4) {
			glog.Infof("%s: created %v", aws.StringValue(bkt.Name), *bkt.CreationDate)
		}
		buckets = append(buckets, aws.StringValue(bkt.Name))
	}
	return
}

//============
//
// object meta
//
//============
func (awsimpl *awsimpl) headobject(ct context.Context, bucket string, objname string) (objmeta map[string]string, errstr string, errcode int) {
	if glog.V(4) {
		glog.Infof("headobject %s/%s", bucket, objname)
	}
	objmeta = make(map[string]string)

	sess := createSession(ct)
	svc := s3.New(sess)
	input := &s3.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(objname)}

	headOutput, err := svc.HeadObject(input)
	if err != nil {
		errcode = awsErrorToHTTP(err)
		errstr = fmt.Sprintf("Failed to retrieve %s/%s metadata, err: %v", bucket, objname, err)
		return
	}
	objmeta[CloudProvider] = ProviderAmazon
	if awsIsVersionSet(headOutput.VersionId) {
		objmeta["version"] = *headOutput.VersionId
	}
	return
}

//=======================
//
// object data operations
//
//=======================
func (awsimpl *awsimpl) getobj(ct context.Context, fqn, bucket, objname string) (props *objectProps, errstr string, errcode int) {
	var v cksumvalue
	sess := createSession(ct)
	svc := s3.New(sess)
	obj, err := svc.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(objname),
	})
	if err != nil {
		errcode = awsErrorToHTTP(err)
		errstr = fmt.Sprintf("Failed to GET %s/%s, err: %v", bucket, objname, err)
		return
	}
	defer obj.Body.Close()
	// may not have dfc metadata
	if htype, ok := obj.Metadata[awsGetDfcHashType]; ok {
		if hval, ok := obj.Metadata[awsGetDfcHashVal]; ok {
			v = newcksumvalue(*htype, *hval)
		}
	}
	md5, _ := strconv.Unquote(*obj.ETag)
	// FIXME: multipart
	if strings.Contains(md5, awsMultipartDelim) {
		if glog.V(3) {
			glog.Infof("Warning: multipart object %s/%s - not validating checksum %s", bucket, objname, md5)
		}
		md5 = ""
	}
	props = &objectProps{}
	if obj.VersionId != nil {
		props.version = *obj.VersionId
	}
	if _, props.nhobj, props.size, errstr = awsimpl.t.receive(fqn, false, objname, md5, v, obj.Body); errstr != "" {
		return
	}
	if glog.V(4) {
		glog.Infof("GET %s/%s", bucket, objname)
	}
	return
}

func (awsimpl *awsimpl) putobj(ct context.Context, file *os.File, bucket, objname string, ohash cksumvalue) (version string, errstr string, errcode int) {
	var (
		err          error
		htype, hval  string
		md           map[string]*string
		uploadoutput *s3manager.UploadOutput
	)
	if ohash != nil {
		htype, hval = ohash.get()
		md = make(map[string]*string)
		md[awsPutDfcHashType] = aws.String(htype)
		md[awsPutDfcHashVal] = aws.String(hval)
	}
	sess := createSession(ct)
	uploader := s3manager.NewUploader(sess)
	uploadoutput, err = uploader.Upload(&s3manager.UploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(objname),
		Body:     file,
		Metadata: md,
	})
	if err != nil {
		errcode = awsErrorToHTTP(err)
		errstr = fmt.Sprintf("Failed to PUT %s/%s, err: %v", bucket, objname, err)
		return
	}
	if glog.V(4) {
		if uploadoutput.VersionID != nil {
			version = *uploadoutput.VersionID
			glog.Infof("PUT %s/%s, version %s", bucket, objname, version)
		} else {
			glog.Infof("PUT %s/%s", bucket, objname)
		}
	}
	return
}

func (awsimpl *awsimpl) deleteobj(ct context.Context, bucket, objname string) (errstr string, errcode int) {
	sess := createSession(ct)
	svc := s3.New(sess)
	_, err := svc.DeleteObject(&s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: aws.String(objname)})
	if err != nil {
		errcode = awsErrorToHTTP(err)
		errstr = fmt.Sprintf("Failed to DELETE %s/%s, err: %v", bucket, objname, err)
		return
	}
	if glog.V(4) {
		glog.Infof("DELETE %s/%s", bucket, objname)
	}
	return
}
