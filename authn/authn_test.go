package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

const (
	dbPath = "/tmp/users.json"
)

var (
	users = []string{"user1", "user2", "user3"}
	passs = []string{"pass2", "pass1", "passs"}
)

func init() {
	// Set default expiration time to 30 minutes
	if conf.Auth.ExpirePeriod == 0 {
		conf.Auth.ExpirePeriod = time.Minute * 30
	}
}

func createUsers(mgr *userManager, t *testing.T) {
	var (
		err error
	)

	if mgr.Path != dbPath {
		t.Fatalf("Invalid path used for user list: %s", mgr.Path)
	}

	vers := mgr.version
	for idx := range users {
		err = mgr.addUser(users[idx], passs[idx])
		if err != nil {
			t.Errorf("Failed to create a user %s: %v", users[idx], err)
		}
		if vers >= mgr.version {
			t.Error("Version must increase")
		}
		vers = mgr.version
	}

	if len(mgr.Users) != len(users) {
		t.Errorf("User count mismatch. Found %d users instead of %d", len(mgr.Users), len(users))
	}
	for _, username := range users {
		info, ok := mgr.Users[username]
		if info == nil || !ok {
			t.Errorf("User %s not found", username)
		}
	}
}

func deleteUsers(mgr *userManager, skipNotExist bool, t *testing.T) {
	var err error
	vers := mgr.version
	for _, username := range users {
		err = mgr.delUser(username)
		if err != nil {
			if !strings.Contains(err.Error(), "not exist") || !skipNotExist {
				t.Errorf("Failed to delete user %s: %v", username, err)
			}
		}
		if vers >= mgr.version {
			t.Error("Version must increase")
		}
		vers = mgr.version
	}

	err = os.Remove(dbPath)
	if err != nil {
		t.Error(err)
	}
}

func testInvalidUser(mgr *userManager, t *testing.T) {
	err := mgr.addUser(users[0], passs[1])
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Errorf("User with the existing name %s was created: %v", users[0], err)
	}

	vers := mgr.version
	nonexisting := "someuser"
	err = mgr.delUser(nonexisting)
	if vers != mgr.version {
		t.Error("Version has changed")
	}
	if err == nil || !strings.Contains(err.Error(), "") {
		t.Errorf("Non-existing user %s was deleted: %v", nonexisting, err)
	}
}

func reloadFromFile(mgr *userManager, t *testing.T) {
	proxy := &proxy{Url: ""}
	newmgr := newUserManager(dbPath, proxy)
	if newmgr == nil {
		t.Error("New manager has not been created")
	}
	if len(newmgr.Users) != len(mgr.Users) {
		t.Errorf("Number of users mismatch: old=%d, new=%d", len(mgr.Users), len(newmgr.Users))
	}
	for username, creds := range mgr.Users {
		if info, ok := newmgr.Users[username]; !ok || info == nil || info.Password != creds.Password {
			t.Errorf("User %s not found in reloaded list", username)
		}
	}
}

func testUserDelete(mgr *userManager, t *testing.T) {
	const (
		username = "newuser"
		userpass = "newpass"
	)
	vers := mgr.version
	err := mgr.addUser(username, userpass)
	if err != nil {
		t.Errorf("Failed to create a user %s: %v", username, err)
	}
	if len(mgr.Users) != len(users)+1 {
		t.Errorf("Expected %d users but found %d", len(users)+1, len(mgr.Users))
	}
	if vers >= mgr.version {
		t.Errorf("Version must increase: %d - %d", vers, mgr.version)
	}
	vers = mgr.version

	token, err := mgr.issueToken(username, userpass)
	if err != nil || token == "" {
		t.Errorf("Failed to generate token for %s: %v", username, err)
	}
	if vers >= mgr.version {
		t.Errorf("Version must increase: %d - %d", vers, mgr.version)
	}
	vers = mgr.version

	err = mgr.delUser(username)
	if err != nil {
		t.Errorf("Failed to delete user %s: %v", username, err)
	}
	if len(mgr.Users) != len(users) {
		t.Errorf("Expected %d users but found %d", len(users), len(mgr.Users))
	}
	if vers >= mgr.version {
		t.Errorf("Version must increase: %d - %d", vers, mgr.version)
	}
	vers = mgr.version
	token, err = mgr.issueToken(username, userpass)
	if token != "" || err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("Token issued for deleted user  %s: %v", username, token)
	}
	if vers != mgr.version {
		t.Error("Version has changed: %d - %d", vers, mgr.version)
	}
}

func Test_manager(t *testing.T) {
	proxy := &proxy{Url: ""}
	mgr := newUserManager(dbPath, proxy)
	if mgr == nil {
		t.Fatal("Manager has not been created")
	}
	createUsers(mgr, t)
	testInvalidUser(mgr, t)
	testUserDelete(mgr, t)
	reloadFromFile(mgr, t)
	deleteUsers(mgr, false, t)
}

func Test_token(t *testing.T) {
	var (
		err   error
		token string
	)

	proxy := &proxy{Url: ""}
	mgr := newUserManager(dbPath, proxy)
	if mgr == nil {
		t.Fatal("Manager has not been created")
	}
	createUsers(mgr, t)

	// correct user creds
	vers := mgr.version
	token, err = mgr.issueToken(users[1], passs[1])
	if err != nil || token == "" {
		t.Errorf("Failed to generate token for %s: %v", users[1], err)
	}
	info, err := mgr.userByToken(token)
	if err != nil {
		t.Errorf("Failed to get user by token %v: %v", token, err)
	}
	if info == nil || info.UserID != users[1] {
		if info == nil {
			t.Errorf("No user returned for token %v", token)
		} else {
			t.Errorf("Invalid user %s returned for token %v", info.UserID, token)
		}
	}
	if vers >= mgr.version {
		t.Errorf("Version must increase: %d - %d", vers, mgr.version)
	}
	vers = mgr.version

	// incorrect user creds
	tokenInval, err := mgr.issueToken(users[1], passs[0])
	if tokenInval != "" || err == nil {
		t.Errorf("Some token generated for incorrect user creds: %v", tokenInval)
	}
	if vers != mgr.version {
		t.Error("Version has changed: %d - %d", vers, mgr.version)
	}

	// expired token test
	tokeninfo, ok := mgr.tokens[users[1]]
	if !ok || tokeninfo == nil {
		t.Errorf("No token found for %s", users[1])
	}
	if tokeninfo != nil {
		tokeninfo.Expires = time.Now().Add(-1 * time.Hour)
	}
	info, err = mgr.userByToken(token)
	if info != nil || err == nil {
		t.Errorf("Token %s expected to be expired[%x]: %v", token, info, err)
	} else if err != nil && !strings.Contains(err.Error(), "expire") {
		t.Errorf("Invalid error(must be 'token expired'): %v", err)
	}

	// revoke token test
	token, err = mgr.issueToken(users[1], passs[1])
	if err == nil {
		_, err = mgr.userByToken(token)
	}
	if err != nil {
		t.Errorf("Failed to test revoking token% v", err)
	} else {
		mgr.revokeToken(token)
		info, err = mgr.userByToken(token)
		if info != nil {
			t.Errorf("Some user returned by revoken token %s: %s", token, info.UserID)
		} else if err == nil {
			t.Error("No error for revoked token")
		} else if !strings.Contains(err.Error(), "not found") {
			t.Errorf("Invalid error: %v", err)
		}
		if vers >= mgr.version {
			t.Error("Version must increase")
		}
	}

	deleteUsers(mgr, false, t)
}
