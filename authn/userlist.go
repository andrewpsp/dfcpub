package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/NVIDIA/dfcpub/dfc"
	"github.com/dgrijalva/jwt-go"
	"github.com/golang/glog"
)

const (
	dbFile = "users.json"
)

type (
	userInfo struct {
		UserID          string `json:"name"`
		Password        string `json:"password,omitempty"`
		passwordDecoded string
		Creds           map[string]string `json:"creds,omitempty"` //TODO: aws?gcp?
	}
	tokenInfo struct {
		UserID  string    `json:"username"`
		Issued  time.Time `json:"issued"`
		Expires time.Time `json:"expires"`
		Token   string    `json:"token"`
	}
	userManager struct {
		userMtx  sync.Mutex
		tokenMtx sync.Mutex
		Path     string               `json:"-"`
		Users    map[string]*userInfo `json:"users"`
		tokens   map[string]*tokenInfo
		version  int64
		client   *http.Client
		proxy    *proxy
	}
)

// borrowed from DFC
func createHTTPClient() *http.Client {
	defaultTransport := http.DefaultTransport.(*http.Transport)
	transport := &http.Transport{
		// defaults
		Proxy: defaultTransport.Proxy,
		DialContext: (&net.Dialer{ // defaultTransport.DialContext,
			Timeout:   30 * time.Second, // must be reduced & configurable
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		IdleConnTimeout:       defaultTransport.IdleConnTimeout,
		ExpectContinueTimeout: defaultTransport.ExpectContinueTimeout,
		TLSHandshakeTimeout:   defaultTransport.TLSHandshakeTimeout,
		// custom
		MaxIdleConnsPerHost: defaultTransport.MaxIdleConnsPerHost,
		MaxIdleConns:        defaultTransport.MaxIdleConns,
	}
	return &http.Client{Transport: transport, Timeout: conf.Timeout.Default}
}

// Creates a new user manager. If user DB exists, it loads the data from the
// file and decrypts passwords
func newUserManager(dbPath string, proxy *proxy) *userManager {
	var (
		err   error
		bytes []byte
	)
	mgr := &userManager{
		Path:    dbPath,
		Users:   make(map[string]*userInfo, 0),
		tokens:  make(map[string]*tokenInfo, 0),
		client:  createHTTPClient(),
		proxy:   proxy,
		version: 1,
	}
	if _, err = os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			glog.Fatalf("Failed to load user list: %v\n", err)
		}
		return mgr
	}

	if err = dfc.LocalLoad(dbPath, &mgr.Users); err != nil {
		glog.Fatalf("Failed to load user list: %v\n", err)
	}
	tokenList := &dfc.TokenList{}
	err = dfc.LocalLoad(mgr.Path+".tokens", tokenList)
	if err == nil {
		mgr.version = tokenList.Version
		for _, tstr := range tokenList.Tokens {
			tinfo, e := mgr.decryptToken(tstr)
			if e != nil {
				glog.Errorf("Invalid token: %s", e)
				continue
			}
			mgr.tokens[tinfo.UserID] = tinfo
		}
	}

	for _, info := range mgr.Users {
		if bytes, err = base64.StdEncoding.DecodeString(info.Password); err != nil {
			glog.Fatalf("Failed to read user list: %v\n", err)
		}
		info.passwordDecoded = string(bytes)
	}

	return mgr
}

// save new user list to user DB
func (m *userManager) saveUsers() (err error) {
	m.userMtx.Lock()
	defer m.userMtx.Unlock()
	if err = dfc.LocalSave(m.Path, &m.Users); err != nil {
		err = fmt.Errorf("UserManager: Failed to save user list: %v", err)
	}
	return err
}

// Registers a new user
func (m *userManager) addUser(userID, userPass string) error {
	if userID == "" || userPass == "" {
		return fmt.Errorf("Invalid credentials")
	}

	m.userMtx.Lock()
	if _, ok := m.Users[userID]; ok {
		m.userMtx.Unlock()
		return fmt.Errorf("User '%s' already registered", userID)
	}
	m.Users[userID] = &userInfo{
		UserID:          userID,
		passwordDecoded: userPass,
		Password:        base64.StdEncoding.EncodeToString([]byte(userPass)),
	}
	m.userMtx.Unlock()

	// clean up in case of there is an old token issued for the same UserID
	m.tokenMtx.Lock()
	delete(m.tokens, userID)
	m.tokenMtx.Unlock()

	return m.saveUsers()
}

// Deletes an existing user
func (m *userManager) delUser(userID string) error {
	m.userMtx.Lock()
	if _, ok := m.Users[userID]; !ok {
		m.userMtx.Unlock()
		return fmt.Errorf("User %s does not exist", userID)
	}
	delete(m.Users, userID)
	m.userMtx.Unlock()

	m.tokenMtx.Lock()
	_, ok := m.tokens[userID]
	delete(m.tokens, userID)
	if ok {
		m.version++
	}
	m.tokenMtx.Unlock()
	if ok {
		go m.sendTokensToProxy()
	}

	return m.saveUsers()
}

func (m *userManager) decryptToken(tokenStr string) (*tokenInfo, error) {
	var (
		issueStr, expireStr string
		invalTokenErr       = fmt.Errorf("Invalid token")
	)
	rec := &tokenInfo{}
	token, err := jwt.Parse(tokenStr, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("Unexpected signing method: %v", token.Header["alg"])
		}

		return []byte(conf.Auth.Secret), nil
	})
	if err != nil {
		return nil, err
	}

	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok || !token.Valid {
		return nil, invalTokenErr
	}
	if rec.UserID, ok = claims["username"].(string); !ok {
		return nil, invalTokenErr
	}
	if issueStr, ok = claims["issued"].(string); !ok {
		return nil, invalTokenErr
	}
	if rec.Issued, err = time.Parse(time.RFC822, issueStr); err != nil {
		return nil, invalTokenErr
	}
	if expireStr, ok = claims["expires"].(string); !ok {
		return nil, invalTokenErr
	}
	if rec.Expires, err = time.Parse(time.RFC822, expireStr); err != nil {
		return nil, invalTokenErr
	}
	rec.Token = tokenStr

	return rec, nil
}

// Generates a token for a user if user credentials are valid. If the token is
// already generated and is not expired yet the existing token is returned.
// Token includes information about userID, AWS/GCP creds and expire token time.
// If a new token was generated then it sends the proxy a new valid token list
func (m *userManager) issueToken(userID, pwd string) (string, error) {
	var (
		user  *userInfo
		token *tokenInfo
		ok    bool
		err   error
	)

	// check user name and pass in DB
	m.userMtx.Lock()
	if user, ok = m.Users[userID]; !ok {
		m.userMtx.Unlock()
		return "", fmt.Errorf("Invalid credentials")
	}
	passwordDecoded := user.passwordDecoded
	creds := user.Creds
	m.userMtx.Unlock()

	if passwordDecoded != pwd {
		return "", fmt.Errorf("Invalid username or password")
	}

	// check if a user is already has got token. If existing token expired then
	// delete it and reissue a new token
	m.tokenMtx.Lock()
	if token, ok = m.tokens[userID]; ok {
		if token.Expires.After(time.Now()) {
			m.tokenMtx.Unlock()
			return token.Token, nil
		}
		delete(m.tokens, userID)
	}
	m.tokenMtx.Unlock()

	// generate token
	issued := time.Now()
	expires := issued.Add(conf.Auth.ExpirePeriod)

	// put all useful info into token: who owns the token, when it was issued,
	// when it expires and credentials to log in AWS, GCP etc
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"issued":   issued.Format(time.RFC822),
		"expires":  expires.Format(time.RFC822),
		"username": userID,
		"creds":    creds,
	})
	tokenString, err := t.SignedString([]byte(conf.Auth.Secret))
	if err != nil {
		return "", fmt.Errorf("failed to generate token: %v", err)
	}

	token = &tokenInfo{
		UserID:  userID,
		Issued:  issued,
		Expires: expires,
		Token:   tokenString,
	}
	m.tokenMtx.Lock()
	m.tokens[userID] = token
	m.version++
	m.tokenMtx.Unlock()
	go m.sendTokensToProxy()

	return tokenString, nil
}

// Delete existing token, a.k.a log out
// If the token was removed successfully then it sends the proxy a new valid token list
func (m *userManager) revokeToken(token string) {
	tokenDeleted := false
	m.tokenMtx.Lock()
	for id, info := range m.tokens {
		if info.Token == token {
			delete(m.tokens, id)
			tokenDeleted = true
			break
		}
	}
	if tokenDeleted {
		m.version++
	}
	m.tokenMtx.Unlock()

	if tokenDeleted {
		go m.sendTokensToProxy()
	}
}

// update list of valid token on a proxy
func (m *userManager) sendTokensToProxy() {
	if m.proxy.Url == "" {
		glog.Error("Primary proxy is not defined")
		return
	}

	tokenList := &dfc.TokenList{Tokens: make([]string, 0, len(m.tokens))}
	m.tokenMtx.Lock()
	for userID, tokenRec := range m.tokens {
		if tokenRec.Expires.Before(time.Now()) {
			// remove expired token
			delete(m.tokens, userID)
			continue
		}

		tokenList.Tokens = append(tokenList.Tokens, tokenRec.Token)
	}
	tokenList.Version = m.version
	m.tokenMtx.Unlock()
	err := dfc.LocalSave(m.Path+".tokens", tokenList)
	if err != nil {
		glog.Errorf("Failed to save tokens: %v", err)
	}

	method := http.MethodPost
	injson, _ := json.Marshal(tokenList)
	for {
		url := fmt.Sprintf("%s/%s/%s", m.proxy.Url, dfc.Rversion, dfc.Rtokens)
		request, err := http.NewRequest(method, url, bytes.NewBuffer(injson))
		if err != nil {
			// Fatal - interrupt the loop
			glog.Error(err)
			return
		}

		request.Header.Set("Content-Type", "application/json")
		response, err := m.client.Do(request)
		if err != nil || (response != nil && response.StatusCode >= http.StatusBadRequest) {
			glog.Errorf("Failed to http-call %s %s: error %v", method, url, err)
			err = m.proxy.detectPrimary()
			if err != nil {
				// primary change is not detected or failed - interrupt the loop
				glog.Errorf("Failed to send token list: %v", err)
				return
			}

			m.proxy.saveSmap()
			if response != nil && response.Body != nil {
				response.Body.Close()
			}
		} else {
			response.Body.Close()
			break
		}
	}
}

func (m *userManager) userByToken(token string) (*userInfo, error) {
	m.tokenMtx.Lock()
	defer m.tokenMtx.Unlock()
	for id, info := range m.tokens {
		if info.Token == token {
			if info.Expires.Before(time.Now()) {
				delete(m.tokens, id)
				return nil, fmt.Errorf("Token expired")
			}

			m.userMtx.Lock()
			defer m.userMtx.Unlock()
			user, ok := m.Users[id]
			if !ok {
				return nil, fmt.Errorf("Invalid token")
			}

			return user, nil
		}
	}

	return nil, fmt.Errorf("Token not found")
}
