// Package dfc is a scalable object-storage based caching system with Amazon and Google Cloud backends.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 *
 */

// Authentication flow:
// 1. If AuthN server is disabled or directory with user credentials is not set:
//    Token in HTTP request header is ignored.
//    All user credentials are read from default files and environment variables.
//    AWS: file ~/.aws/credentials
//    GCP: file ~/gcp_creds.json and GOOGLE_CLOUD_PROJECT variable
//         a user should be logged in to GCP before running any DFC operation
//         that touches cloud objects
// 2. AuthN server is enabled and everything is set up
//    - DFC reads userID from HTTP request header: 'Authorization: Bearer <token>'.
//    - A user credentials is loaded for the userID
//      AWS: CredDir points to a directory that contains a file with user
//       credentials. Authn looks for a file with name 'credentials' or it takes
//       the first file found in the directory. The file must contain a section
//       for the userID in a form:
//       [userId]
//       region = us-east-1
//       aws_access_key_id = USERACCESSKEY
//       aws_secret_access_key = USERSECRETKEY
//      GCP: CredDir points to a directory that contains user files. A file per
//       a user. The file name must be 'userID' + '.json'
//    - Besides user credentials it loads a projectId for GCP (required by
//      bucketnames call)
//    - After successful loading and checking the user's credentials it opens
//      a new session with loaded from file data
//    - Extra step for GCP: if creating session for a given user fails, it tries
//      to start a session with default parameters
package dfc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dgrijalva/jwt-go"
	"github.com/golang/glog"
)

const (
	ctxUserID    = "userID"         // a field name of a context that contains userID
	ctxCredsDir  = "credDir"        // a field of a context that contains path to directory with credentials
	awsCredsFile = "credentials"    // a default AWS file with user credentials
	gcpCredsFile = "gcp_creds.json" // a default GOOGLE file with user credentials
	dfcCredsFile = "gcp_creds.json" // a default DFC file with user credentials
)

type (
	// TokenList is a list of tokens pushed by authn after any token change
	TokenList struct {
		Tokens  []string `json:"tokens"`
		Version int64    `json:"version,omitempty"`
	}

	authRec struct {
		userID  string
		issued  time.Time
		expires time.Time
		creds   map[string]string // TODO: what to keep in this field and how
	}

	authList map[string]*authRec

	authManager struct {
		// decrypted token information from TokenList
		sync.Mutex
		tokens        authList
		tokensVersion int64
	}
)

// Decrypts JWT token and returns all encrypted information.
// Used by proxy - to check a user access and token validity(e.g, expiration),
// and by target - only to get a user name for AWS/GCP access
func decryptToken(tokenStr string) (*authRec, error) {
	var (
		issueStr, expireStr string
		invalTokenErr       = fmt.Errorf("Invalid token")
	)
	rec := &authRec{}
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}

		return []byte(ctx.config.Auth.Secret), nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, invalTokenErr
	}
	if rec.userID, ok = claims["username"].(string); !ok {
		return nil, invalTokenErr
	}
	if issueStr, ok = claims["issued"].(string); !ok {
		return nil, invalTokenErr
	}
	if rec.issued, err = time.Parse(time.RFC822, issueStr); err != nil {
		return nil, invalTokenErr
	}
	if expireStr, ok = claims["expires"].(string); !ok {
		return nil, invalTokenErr
	}
	if rec.expires, err = time.Parse(time.RFC822, expireStr); err != nil {
		return nil, invalTokenErr
	}
	if rec.creds, ok = claims["creds"].(map[string]string); !ok {
		rec.creds = make(map[string]string, 0)
	}

	return rec, nil
}

// Converts token list sent by authn and checks for correct format
func newAuthList(tokenList *TokenList) (authList, int64, error) {
	auth := make(map[string]*authRec)
	if tokenList == nil || len(tokenList.Tokens) == 0 {
		return auth, 1, nil
	}

	for _, tokenStr := range tokenList.Tokens {
		rec, err := decryptToken(tokenStr)
		if err != nil {
			return nil, 0, err
		}
		auth[tokenStr] = rec
	}

	return auth, tokenList.Version, nil
}

// Retreives a userID from context or empty string if nothing found
func userIDFromContext(ct context.Context) string {
	userIf := ct.Value(ctxUserID)
	if userIf == nil {
		return ""
	}

	userID, ok := userIf.(string)
	if !ok {
		return ""
	}

	return userID
}

func pathToCredentials(baseDir, provider, userID string) string {
	credPath := filepath.Join(baseDir, provider, userID)
	switch provider {
	case ProviderAmazon:
		credPath = filepath.Join(credPath, awsCredsFile)
	case ProviderGoogle:
		credPath = filepath.Join(credPath, gcpCredsFile)
	case ProviderDfc:
		credPath = filepath.Join(credPath, dfcCredsFile)
	default:
		glog.Errorf("Invalid cloud provider: %s", provider)
	}
	return credPath
}

// Reads a directory with user credentials file.
// All credentials file paths should follow the rule:
//		<ctx.CredsDir>/<provider>/<userID>/<fileNameForProvider>
// Provider is the type of storage: AWS, GCP or DFC (Provider* constants in REST.go)
// Returns a full path to file with credentials or error
func userCredsPathFromContext(ct context.Context, userID, provider string) (string, error) {
	dirIf := ct.Value(ctxCredsDir)
	if dirIf == nil {
		return "", fmt.Errorf("Directory is not defined")
	}

	credDir, ok := dirIf.(string)
	if !ok {
		return "", fmt.Errorf("%s expected string type but it is %T (%v)", ctxCredsDir, dirIf, dirIf)
	}

	credPath := pathToCredentials(credDir, provider, userID)
	stat, err := os.Stat(credPath)
	if err != nil {
		glog.Errorf("Failed to open credential file: %v", err)
		return "", fmt.Errorf("Failed to open credentials file")
	}

	if stat.IsDir() {
		return "", fmt.Errorf("A file expected but %s is a directory", credPath)
	}

	return credPath, nil
}

// Looks for a token in the list of valid tokens and returns information
// about a user for whom the token was issued
func (a *authManager) validateToken(token string) (*authRec, error) {
	a.Lock()
	defer a.Unlock()
	auth, ok := a.tokens[token]
	if !ok {
		glog.Errorf("Token not found: %s", token)
		return nil, fmt.Errorf("Token not found")
	}

	if auth.expires.Before(time.Now()) {
		glog.Errorf("Expired token was used: %s", token)
		delete(a.tokens, token)
		return nil, fmt.Errorf("Token expired")
	}

	return auth, nil
}

var _ revs = &authManager{}

func (a *authManager) tag() string {
	return "token-list"
}

// func (a *authManager) cloneL() *TokenList {
func (a *authManager) cloneL() interface{} {
	a.Lock()
	defer a.Unlock()

	tlist := &TokenList{
		Tokens:  make([]string, 0, 0),
		Version: a.tokensVersion,
	}
	for token := range a.tokens {
		tlist.Tokens = append(tlist.Tokens, token)
	}

	return tlist
}
func (a *authManager) version() int64 {
	a.Lock()
	defer a.Unlock()
	return a.tokensVersion
}
func (a *authManager) marshal() ([]byte, error) {
	tlist := a.cloneL()
	return json.Marshal(tlist)
}
